// Copyright (C) MongoDB, Inc. 2014-present.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

// Package mongodump creates BSON data from the contents of a MongoDB instance.
package mongodump

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mongodb/mongo-tools/common"
	"github.com/mongodb/mongo-tools/common/archive"
	"github.com/mongodb/mongo-tools/common/auth"
	"github.com/mongodb/mongo-tools/common/bsonutil"
	"github.com/mongodb/mongo-tools/common/db"
	"github.com/mongodb/mongo-tools/common/failpoint"
	"github.com/mongodb/mongo-tools/common/intents"
	"github.com/mongodb/mongo-tools/common/log"
	"github.com/mongodb/mongo-tools/common/options"
	"github.com/mongodb/mongo-tools/common/progress"
	"github.com/mongodb/mongo-tools/common/util"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// MongoDump is a container for the user-specified options and
// internal state used for running mongodump.
type MongoDump struct {
	// basic mongo tool options
	ToolOptions   *options.ToolOptions
	InputOptions  *InputOptions
	OutputOptions *OutputOptions

	// Skip dumping users and roles, regardless of namespace, when true.
	SkipUsersAndRoles bool

	ProgressManager progress.Manager

	// useful internals that we don't directly expose as options
	SessionProvider *db.SessionProvider
	manager         *intents.Manager
	query           bson.D
	oplogCollection string
	oplogStart      primitive.Timestamp
	oplogEnd        primitive.Timestamp
	isMongos        bool
	isAtlasProxy    bool
	serverVersion   string
	authVersion     int
	archive         *archive.Writer
	// shutdownIntentsNotifier is provided to the multiplexer
	// as well as the signal handler, and allows them to notify
	// the intent dumpers that they should shutdown
	shutdownIntentsNotifier *notifier
	// Writer to take care of BSON output when not writing to the local filesystem.
	// This is initialized to os.Stdout if unset.
	OutputWriter io.Writer

	// XXX Unused?!?
	// readPrefMode mgo.Mode
	// readPrefTags []bson.D
}

type notifier struct {
	notified chan struct{}
	once     sync.Once
}

func (n *notifier) Notify() { n.once.Do(func() { close(n.notified) }) }

func newNotifier() *notifier { return &notifier{notified: make(chan struct{})} }

// ValidateOptions checks for any incompatible sets of options.
func (dump *MongoDump) ValidateOptions() error {
	switch {
	case dump.OutputOptions.Out == "-" && dump.ToolOptions.Namespace.Collection == "":
		return fmt.Errorf("can only dump a single collection to stdout")
	case dump.ToolOptions.Namespace.DB == "" && dump.ToolOptions.Namespace.Collection != "":
		return fmt.Errorf("cannot dump a collection without a specified database")
	case dump.InputOptions.Query != "" && dump.ToolOptions.Namespace.Collection == "":
		return fmt.Errorf("cannot dump using a query without a specified collection")
	case dump.InputOptions.QueryFile != "" && dump.ToolOptions.Namespace.Collection == "":
		return fmt.Errorf("cannot dump using a queryFile without a specified collection")
	case dump.InputOptions.Query != "" && dump.InputOptions.QueryFile != "":
		return fmt.Errorf("either query or queryFile can be specified as a query option, not both")
	case dump.InputOptions.Query != "" && dump.InputOptions.TableScan:
		return fmt.Errorf("cannot use --forceTableScan when specifying --query")
	case dump.OutputOptions.DumpDBUsersAndRoles && dump.ToolOptions.Namespace.DB == "":
		return fmt.Errorf("must specify a database when running with dumpDbUsersAndRoles")
	case dump.OutputOptions.DumpDBUsersAndRoles && dump.ToolOptions.Namespace.Collection != "":
		return fmt.Errorf("cannot specify a collection when running with dumpDbUsersAndRoles")
	case strings.HasPrefix(dump.ToolOptions.Namespace.Collection, "system.buckets."):
		return fmt.Errorf("cannot specify a system.buckets collection in --collection. " +
			"Specifying the timeseries collection will dump the system.buckets collection")
	case dump.OutputOptions.Oplog && dump.ToolOptions.Namespace.DB != "":
		return fmt.Errorf("--oplog mode only supported on full dumps")
	case len(dump.OutputOptions.ExcludedCollections) > 0 && dump.ToolOptions.Namespace.Collection != "":
		return fmt.Errorf("--collection is not allowed when --excludeCollection is specified")
	case len(dump.OutputOptions.ExcludedCollectionPrefixes) > 0 && dump.ToolOptions.Namespace.Collection != "":
		return fmt.Errorf(
			"--collection is not allowed when --excludeCollectionsWithPrefix is specified",
		)
	case len(dump.OutputOptions.ExcludedCollections) > 0 && dump.ToolOptions.Namespace.DB == "":
		return fmt.Errorf("--db is required when --excludeCollection is specified")
	case len(dump.OutputOptions.ExcludedCollectionPrefixes) > 0 && dump.ToolOptions.Namespace.DB == "":
		return fmt.Errorf("--db is required when --excludeCollectionsWithPrefix is specified")
	case dump.OutputOptions.Out != "" && dump.OutputOptions.Archive != "":
		return fmt.Errorf("--out not allowed when --archive is specified")
	case dump.OutputOptions.Out == "-" && dump.OutputOptions.Gzip:
		return fmt.Errorf(
			"compression can't be used when dumping a single collection to standard output",
		)
	case dump.OutputOptions.NumParallelCollections <= 0:
		return fmt.Errorf("numParallelCollections must be positive")
	case dump.isAtlasProxy && (dump.OutputOptions.DumpDBUsersAndRoles || dump.ToolOptions.DB == "admin"):
		return fmt.Errorf(
			"can't dump from admin database when connecting to a MongoDB Atlas free or shared cluster",
		)
	}
	return nil
}

// Init performs preliminary setup operations for MongoDump.
func (dump *MongoDump) Init() error {
	log.Logvf(log.DebugHigh, "initializing mongodump object")

	pref, err := db.NewReadPreference(
		dump.InputOptions.ReadPreference,
		dump.ToolOptions.URI.ParsedConnString(),
	)
	if err != nil {
		return fmt.Errorf("error parsing --readPreference : %v", err)
	}
	dump.ToolOptions.ReadPreference = pref

	dump.SessionProvider, err = db.NewSessionProvider(*dump.ToolOptions)
	if err != nil {
		return fmt.Errorf("can't create session: %v", err)
	}

	dump.isMongos, err = dump.SessionProvider.IsMongos()
	if err != nil {
		return fmt.Errorf("error checking for Mongos: %v", err)
	}

	dump.isAtlasProxy = dump.SessionProvider.IsAtlasProxy()
	if dump.isAtlasProxy {
		log.Logv(log.DebugLow, "dumping from a MongoDB Atlas free or shared cluster")
	}

	err = dump.ValidateOptions()
	if err != nil {
		return fmt.Errorf("bad option: %v", err)
	}
	if dump.OutputWriter == nil {
		dump.OutputWriter = os.Stdout
	}

	if dump.isMongos && dump.OutputOptions.Oplog {
		return fmt.Errorf("can't use --oplog option when dumping from a mongos")
	}

	// warn if we are trying to dump from a secondary in a sharded cluster
	if dump.isMongos && pref != readpref.Primary() {
		log.Logvf(log.Always, db.WarningNonPrimaryMongosConnection)
	}

	dump.manager = intents.NewIntentManager()

	return nil
}

func (dump *MongoDump) verifyCollectionExists() (bool, error) {
	// Running MongoDump against a DB with no collection specified works. In this case, return true so the process
	// can continue.
	if dump.ToolOptions.Namespace.Collection == "" {
		return true, nil
	}

	coll := dump.SessionProvider.DB(dump.ToolOptions.Namespace.DB).
		Collection(dump.ToolOptions.Namespace.Collection)
	collInfo, err := db.GetCollectionInfo(coll)
	if err != nil {
		return false, err
	}

	return collInfo != nil, nil
}

// Dump handles some final options checking and executes MongoDump.
func (dump *MongoDump) Dump() (err error) {
	defer dump.SessionProvider.Close()

	if !dump.OutputOptions.Oplog && (dump.InputOptions.SourceWritesDoneBarrier != "") {
		// Wait for tests to stop writes before dumping any collections.
		//
		// In resmoke testing, the barrier is used to ensure that mongodump captures the correct
		// state of the source cluster.  Events that occur before the barrier file is created will
		// definitely be captured in the dumped collections.  Events that occur after the barrier
		// file is created may not be captured.
		barrier := dump.InputOptions.SourceWritesDoneBarrier
		if err = waitForSourceWritesDoneBarrier(barrier); err != nil {
			return err
		}
	}

	// A test with the combination of
	//    1. --oplog and
	//    2. --internalSourceWritesOnly and
	//    3. a specified collection
	//    4. the collection didn't exist at the time mongodump was started but is
	//       created later and possibly captured in the oplog
	// would do this check too early and thus fail.
	//
	// That's out of scope for mongodump passthrough testing so we don't try to handle it.
	exists, err := dump.verifyCollectionExists()
	if err != nil {
		return fmt.Errorf("error verifying collection info: %v", err)
	}
	if !exists {
		log.Logvf(log.Always, "namespace with DB %s and collection %s does not exist",
			dump.ToolOptions.Namespace.DB, dump.ToolOptions.Namespace.Collection)
		return nil
	}

	log.Logvf(log.DebugHigh, "starting Dump()")

	dump.shutdownIntentsNotifier = newNotifier()

	if dump.InputOptions.HasQuery() {
		content, err := dump.InputOptions.GetQuery()
		if err != nil {
			return err
		}
		var query bson.D
		err = bson.UnmarshalExtJSON(content, false, &query)
		if err != nil {
			return fmt.Errorf("error parsing query as Extended JSON: %v", err)
		}
		dump.query = query
	}

	// If we enter this case, then we're not connected to an atlas proxy otherwise
	// mongodump would have errored earlier.
	if !dump.SkipUsersAndRoles && dump.OutputOptions.DumpDBUsersAndRoles {
		// first make sure this is possible with the connected database
		dump.authVersion, err = auth.GetAuthVersion(dump.SessionProvider)
		if err == nil {
			err = auth.VerifySystemAuthVersion(dump.SessionProvider)
		}
		if err != nil {
			return fmt.Errorf("error getting auth schema version for dumpDbUsersAndRoles: %v", err)
		}
		log.Logvf(log.DebugLow, "using auth schema version %v", dump.authVersion)
		if dump.authVersion < 3 {
			return fmt.Errorf("backing up users and roles is only supported for "+
				"deployments with auth schema versions >= 3, found: %v", dump.authVersion)
		}
	}

	if dump.OutputOptions.Archive != "" {
		//getArchiveOut gives us a WriteCloser to which we should write the archive
		var archiveOut io.WriteCloser
		archiveOut, err = dump.getArchiveOut()
		if err != nil {
			return err
		}
		dump.archive = &archive.Writer{
			// The archive.Writer needs its own copy of archiveOut because things
			// like the prelude are not written by the multiplexer.
			Out: archiveOut,
			Mux: archive.NewMultiplexer(archiveOut, dump.shutdownIntentsNotifier),
		}
		go dump.archive.Mux.Run()
		defer func() {
			// The Mux runs until its Control is closed
			close(dump.archive.Mux.Control)
			muxErr := <-dump.archive.Mux.Completed
			archiveOut.Close()
			if muxErr != nil {
				if err != nil {
					err = fmt.Errorf("archive writer: %v / %v", err, muxErr)
				} else {
					err = fmt.Errorf("archive writer: %v", muxErr)
				}
				log.Logvf(log.DebugLow, "%v", err)
			} else {
				log.Logvf(log.DebugLow, "mux completed successfully")
			}
		}()
	}

	// Confirm connectivity
	session, err := dump.SessionProvider.GetSession()
	if err != nil {
		return fmt.Errorf("error getting a client session: %v", err)
	}
	err = session.Ping(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("error connecting to host: %v", err)
	}

	// If oplog capturing is enabled, we first check the most recent
	// oplog entry and save its timestamp, this will let us later
	// copy all oplog entries that occurred while dumping, creating
	// what is effectively a point-in-time snapshot.
	if dump.OutputOptions.Oplog {
		err := dump.determineOplogCollectionName()
		if err != nil {
			return fmt.Errorf("error finding oplog: %v", err)
		}
		log.Logvf(log.Info, "getting most recent oplog timestamp")
		dump.oplogStart, err = dump.getOplogCopyStartTime()
		if err != nil {
			return fmt.Errorf("error getting oplog start: %v", err)
		}
	}

	if failpoint.Enabled(failpoint.PauseBeforeDumping) {
		log.Logvf(log.Info, "failpoint.PauseBeforeDumping: sleeping 15 sec")
		time.Sleep(15 * time.Second)
	}

	// switch on what kind of execution to do
	switch {
	case dump.ToolOptions.DB == "" && dump.ToolOptions.Collection == "":
		err = dump.CreateAllIntents()
	case dump.ToolOptions.DB != "" && dump.ToolOptions.Collection == "":
		err = dump.CreateIntentsForDatabase(dump.ToolOptions.DB)
	case dump.ToolOptions.DB != "" && dump.ToolOptions.Collection != "":
		err = dump.CreateCollectionIntent(dump.ToolOptions.DB, dump.ToolOptions.Collection)
	}
	if err != nil {
		return fmt.Errorf("error creating intents to dump: %v", err)
	}

	if dump.OutputOptions.Oplog {
		err = dump.CreateOplogIntents()
		if err != nil {
			return err
		}
	}

	// If we enter this case, then we're not connected to an atlas proxy otherwise
	// mongodump would have errored earlier.
	if !dump.SkipUsersAndRoles && dump.OutputOptions.DumpDBUsersAndRoles &&
		dump.ToolOptions.DB != "admin" {
		err = dump.CreateUsersRolesVersionIntentsForDB(dump.ToolOptions.DB)
		if err != nil {
			return err
		}
	}

	// IO Phase I
	// metadata, users, roles, and versions

	// TODO, either remove this debug or improve the language
	log.Logvf(log.DebugHigh, "dump phase I: metadata, indexes, users, roles, version")

	err = dump.DumpMetadata()
	if err != nil {
		return fmt.Errorf("error dumping metadata: %v", err)
	}

	dump.serverVersion, err = dump.SessionProvider.ServerVersion()
	if err != nil {
		log.Logvf(log.Always, "warning, couldn't get version information from server: %v", err)
		dump.serverVersion = common.ServerVersionUnknown
	}

	if dump.OutputOptions.Archive != "" {
		dump.archive.Prelude, err = archive.NewPrelude(
			dump.manager,
			dump.OutputOptions.NumParallelCollections,
			dump.serverVersion,
			dump.ToolOptions.VersionStr,
		)
		if err != nil {
			return fmt.Errorf("creating archive prelude: %v", err)
		}
		err = dump.archive.Prelude.Write(dump.archive.Out)
		if err != nil {
			return fmt.Errorf("error writing metadata into archive: %v", err)
		}
	}

	// Dump users and roles only if these settings are not configured to be skipped,
	// and mongodump isn't connected to an atlas proxy.
	if !dump.SkipUsersAndRoles && !dump.isAtlasProxy {
		if dump.ToolOptions.DB == "admin" || dump.ToolOptions.DB == "" {
			err = dump.DumpUsersAndRoles()
			if err != nil {
				return fmt.Errorf("error dumping users and roles: %v", err)
			}
		}
		if dump.OutputOptions.DumpDBUsersAndRoles {
			log.Logvf(log.Always, "dumping users and roles for %v", dump.ToolOptions.DB)
			if dump.ToolOptions.DB == "admin" {
				log.Logvf(log.Always, "skipping users/roles dump, already dumped admin database")
			} else {
				err = dump.DumpUsersAndRolesForDB(dump.ToolOptions.DB)
				if err != nil {
					return fmt.Errorf("error dumping users and roles: %v", err)
				}
			}
		}
	}

	// IO Phase II
	// regular collections

	// TODO, either remove this debug or improve the language
	log.Logvf(log.DebugHigh, "dump phase II: regular collections")

	// begin dumping intents
	if err := dump.DumpIntents(); err != nil {
		return err
	}

	// IO Phase III
	// oplog

	// TODO, either remove this debug or improve the language
	log.Logvf(log.DebugLow, "dump phase III: the oplog")

	if dump.OutputOptions.Oplog {
		if dump.InputOptions.SourceWritesDoneBarrier != "" {
			// Wait for tests to stop writes before choosing the oplogEnd time.
			//
			// In resmoke testing, the barrier is used to ensure that mongodump captures the correct
			// state of the source cluster.  Events that occur before the barrier file is created will
			// definitely be captured either in the dumped collections, or the dumped oplog.
			// Events that occur after the barrier file is created may not be captured.
			barrier := dump.InputOptions.SourceWritesDoneBarrier
			if err = waitForSourceWritesDoneBarrier(barrier); err != nil {
				return err
			}
		}
		dump.oplogEnd, err = dump.getCurrentOplogTime()
		if err != nil {
			return fmt.Errorf("error getting oplog end: %v", err)
		}

		// If we are capturing the oplog, we dump all oplog entries that occurred
		// while dumping the database. Before and after dumping the oplog,
		// we check to see if the oplog has rolled over (i.e. the most recent entry when
		// we started still exist, so we know we haven't lost data)
		log.Logvf(log.DebugLow, "checking if oplog entry %v still exists", dump.oplogStart)
		exists, err := dump.checkOplogTimestampExists(dump.oplogStart)
		if !exists {
			return fmt.Errorf(
				"oplog overflow: mongodump was unable to capture all new oplog entries during execution",
			)
		}
		if err != nil {
			return fmt.Errorf("unable to check oplog for overflow: %v", err)
		}
		log.Logvf(log.DebugHigh, "oplog entry %v still exists", dump.oplogStart)

		log.Logvf(log.Always, "writing captured oplog to %v", dump.manager.Oplog().Location)

		err = dump.DumpOplogBetweenTimestamps(dump.oplogStart, dump.oplogEnd)
		if err != nil {
			return fmt.Errorf("error dumping oplog: %v", err)
		}

		// Check the oplog for a rollover one last time, to avoid a race condition
		// wherein the oplog rolls over in the time after our first check, but before
		// we copy it.
		log.Logvf(log.DebugLow, "checking again if oplog entry %v still exists", dump.oplogStart)
		exists, err = dump.checkOplogTimestampExists(dump.oplogStart)
		if !exists {
			return fmt.Errorf(
				"oplog overflow: mongodump was unable to capture all new oplog entries during execution",
			)
		}
		if err != nil {
			return fmt.Errorf("unable to check oplog for overflow: %v", err)
		}
		log.Logvf(log.DebugHigh, "oplog entry %v still exists", dump.oplogStart)
	}

	if dump.OutputOptions.Archive == "" && dump.OutputOptions.Out != "-" {
		log.Logvf(log.DebugLow, "dump phase IV: top level metadata json")
		err = dump.DumpPreludeMetadata()
		if err != nil {
			return fmt.Errorf("failed to dump top level metadata: %v", err)
		}
	}

	log.Logvf(log.DebugLow, "finishing dump")

	return err
}

type resettableOutputBuffer interface {
	io.Writer
	Close() error
	Reset(io.Writer)
}

type closableBufioWriter struct {
	*bufio.Writer
}

func (w closableBufioWriter) Close() error {
	return w.Flush()
}

func (dump *MongoDump) getResettableOutputBuffer() resettableOutputBuffer {
	if dump.OutputOptions.Archive != "" {
		return nil
	} else if dump.OutputOptions.Gzip {
		return gzip.NewWriter(nil)
	}
	return &closableBufioWriter{bufio.NewWriter(nil)}
}

// DumpIntents iterates through the previously-created intents and
// dumps all of the found collections.
func (dump *MongoDump) DumpIntents() error {
	jobs := dump.OutputOptions.NumParallelCollections
	if numIntents := len(dump.manager.Intents()); jobs > numIntents {
		jobs = numIntents
	}

	if jobs > 1 {
		dump.manager.Finalize(intents.LongestTaskFirst)
	} else {
		dump.manager.Finalize(intents.Legacy)
	}

	resultChan := make(chan error, jobs)
	log.Logvf(log.Info, "dumping up to %v collections in parallel", jobs)

	// start a goroutine for each job thread
	for i := 0; i < jobs; i++ {
		go func(id int) {
			buffer := dump.getResettableOutputBuffer()
			log.Logvf(log.DebugHigh, "starting dump routine with id=%v", id)
			for {
				intent := dump.manager.Pop()
				if intent == nil {
					log.Logvf(
						log.DebugHigh,
						"ending dump routine with id=%v, no more work to do",
						id,
					)
					resultChan <- nil
					return
				}
				if intent.BSONFile != nil {
					err := dump.DumpIntent(intent, buffer)
					if err != nil {
						resultChan <- err
						return
					}
				}
				dump.manager.Finish(intent)
			}
		}(i)
	}

	// wait until all goroutines are done or one of them errors out
	for i := 0; i < jobs; i++ {
		if err := <-resultChan; err != nil {
			return err
		}
	}

	return nil
}

// DumpIntent dumps the specified database's collection.
func (dump *MongoDump) DumpIntent(intent *intents.Intent, buffer resettableOutputBuffer) error {
	session, err := dump.SessionProvider.GetSession()
	if err != nil {
		return err
	}
	intendedDB := session.Database(intent.DB)
	var coll *mongo.Collection
	if intent.IsTimeseries() {
		coll = intendedDB.Collection("system.buckets." + intent.C)
	} else {
		coll = intendedDB.Collection(intent.C)
	}

	findQuery := &db.DeferredQuery{Coll: coll}
	if len(dump.query) > 0 {
		if intent.IsTimeseries() {
			timeseriesOptions, err := bsonutil.FindSubdocumentByKey("timeseries", &intent.Options)
			if err != nil {
				return errors.Wrapf(
					err,
					"could not find timeseries options for %s",
					intent.Namespace(),
				)
			}
			metaKey, err := bsonutil.FindStringValueByKey("metaField", &timeseriesOptions)
			if err != nil {
				return errors.Wrapf(
					err,
					"could not determine the metaField for %s",
					intent.Namespace(),
				)
			}
			for i, predicate := range dump.query {
				splitPredicateKey := strings.SplitN(predicate.Key, ".", 2)
				if splitPredicateKey[0] != metaKey {
					return fmt.Errorf("cannot process query %v for timeseries collection %s. "+
						"mongodump only processes queries on metadata fields for timeseries collections.", dump.query, intent.Namespace())
				}
				if len(splitPredicateKey) > 1 {
					dump.query[i].Key = "meta." + splitPredicateKey[1]
				} else {
					dump.query[i].Key = "meta"
				}

			}
		}
		findQuery.Filter = dump.query
	}

	var dumpCount int64

	if dump.OutputOptions.Out == "-" {
		log.Logvf(log.Always, "writing %v to stdout", intent.DataNamespace())
		dumpCount, err = dump.dumpQueryToIntent(findQuery, intent, buffer)
		if err == nil {
			// on success, print the document count
			log.Logvf(log.Always, "dumped %v %v", dumpCount, docPlural(dumpCount))
		}
		return err
	}

	log.Logvf(log.Always, "writing %v to %v", intent.DataNamespace(), intent.Location)
	if dumpCount, err = dump.dumpQueryToIntent(findQuery, intent, buffer); err != nil {
		return err
	}

	log.Logvf(
		log.Always,
		"done dumping %v (%v %v)",
		intent.DataNamespace(),
		dumpCount,
		docPlural(dumpCount),
	)
	return nil
}

// documentValidator represents a callback used to validate individual documents. It takes a slice of bytes for a
// BSON document and returns a non-nil error if the document is not valid.
type documentValidator func([]byte) error

// dumpQueryToIntent takes an mgo Query, its intent, and a writer, performs the query,
// and writes the raw bson results to the writer. Returns a final count of documents
// dumped, and any errors that occurred.
func (dump *MongoDump) dumpQueryToIntent(
	query *db.DeferredQuery,
	intent *intents.Intent,
	buffer resettableOutputBuffer,
) (dumpCount int64, err error) {
	return dump.dumpValidatedQueryToIntent(query, intent, buffer, nil)
}

// getCount counts the number of documents in the namespace for the given intent. It does not run the count for
// the oplog collection to avoid the performance issue in TOOLS-2068.
func (dump *MongoDump) getCount(query *db.DeferredQuery, intent *intents.Intent) (int64, error) {
	if len(dump.query) != 0 || intent.IsOplog() {
		log.Logvf(log.DebugLow, "not counting query on %v", intent.Namespace())
		return 0, nil
	}

	log.Logvf(
		log.DebugHigh,
		"Getting estimated count for %v.%v",
		query.Coll.Database().Name(),
		query.Coll.Name(),
	)
	// We call getCount() when we are dumping a collection. If we are dumping views as collections, we need to run a
	// count instead of an estimatedDocumentCount which uses collStats. We don't do this if the intent is timeseries because
	// we would be dumping system.buckets.X which can use collStats.
	total, err := query.Count(intent.IsView())
	if err != nil {
		return 0, fmt.Errorf("error getting count from db: %v", err)
	}

	log.Logvf(
		log.DebugLow,
		"counted %v %v in %v",
		total,
		docPlural(int64(total)),
		intent.Namespace(),
	)
	return int64(total), nil
}

// dumpValidatedQueryToIntent takes an mgo Query, its intent, a writer, and a document validator, performs the query,
// validates the results with the validator,
// and writes the raw bson results to the writer. Returns a final count of documents
// dumped, and any errors that occurred.
func (dump *MongoDump) dumpValidatedQueryToIntent(
	query *db.DeferredQuery,
	intent *intents.Intent,
	buffer resettableOutputBuffer,
	validator documentValidator,
) (dumpCount int64, err error) {

	// restore of views from archives require an empty collection as the trigger to create the view
	// so, we open here before the early return if IsView so that we write an empty collection to the archive
	err = intent.BSONFile.Open()
	if err != nil {
		return 0, err
	}
	defer func() {
		closeErr := intent.BSONFile.Close()
		if err == nil && closeErr != nil {
			err = fmt.Errorf(
				"error writing data for collection `%v` to disk: %v",
				intent.Namespace(),
				closeErr,
			)
		}
	}()
	// don't dump any data for views being dumped as views
	if intent.IsView() && !dump.OutputOptions.ViewsAsCollections {
		return 0, nil
	}

	total, err := dump.getCount(query, intent)
	if err != nil {
		return 0, err
	}

	dumpProgressor := progress.NewCounter(total)
	if dump.ProgressManager != nil {
		dump.ProgressManager.Attach(intent.Namespace(), dumpProgressor)
		defer dump.ProgressManager.Detach(intent.Namespace())
	}

	var f io.Writer
	f = intent.BSONFile
	if buffer != nil {
		buffer.Reset(f)
		f = buffer
		defer func() {
			closeErr := buffer.Close()
			if err == nil && closeErr != nil {
				err = fmt.Errorf(
					"error writing data for collection `%v` to disk: %v",
					intent.Namespace(),
					closeErr,
				)
			}
		}()
	}

	cursor, err := query.Iter()
	if err != nil {
		return
	}
	err = dump.dumpValidatedIterToWriter(cursor, f, dumpProgressor, validator)
	dumpCount, _ = dumpProgressor.Progress()
	if err != nil {
		err = fmt.Errorf(
			"error writing data for collection `%v` to disk: %v",
			intent.Namespace(),
			err,
		)
	}
	return
}

// dumpValidatedIterToWriter takes a cursor, a writer, an Updateable object, and a documentValidator and validates and
// dumps the iterator's contents to the writer.
func (dump *MongoDump) dumpValidatedIterToWriter(
	iter *mongo.Cursor,
	writer io.Writer,
	progressCount progress.Updateable,
	validator documentValidator,
) error {
	defer iter.Close(context.Background())
	var termErr error

	// We run the result iteration in its own goroutine,
	// this allows disk i/o to not block reads from the db,
	// which gives a slight speedup on benchmarks
	buffChan := make(chan []byte)
	go func() {
		ctx := context.Background()
		for {
			select {
			case <-dump.shutdownIntentsNotifier.notified:
				log.Logvf(log.DebugHigh, "terminating writes")
				termErr = util.ErrTerminated
				close(buffChan)
				return
			default:
				if !iter.Next(ctx) {
					if err := iter.Err(); err != nil {
						termErr = err
					}
					close(buffChan)
					return
				}

				if validator != nil {
					if err := validator(iter.Current); err != nil {
						termErr = err
						close(buffChan)
						return
					}
				}

				out := make([]byte, len(iter.Current))
				copy(out, iter.Current)
				buffChan <- out
			}
		}
	}()

	// while there are still results in the database,
	// grab results from the goroutine and write them to filesystem
	for {
		buff, alive := <-buffChan
		if !alive {
			if iter.Err() != nil {
				return fmt.Errorf("error reading collection: %v", iter.Err())
			}
			break
		}
		_, err := writer.Write(buff)
		if err != nil {
			return fmt.Errorf("error writing to file: %v", err)
		}
		progressCount.Inc(1)
	}
	return termErr
}

// DumpUsersAndRolesForDB queries and dumps the users and roles tied to the given
// database. Only works with an authentication schema version >= 3.
func (dump *MongoDump) DumpUsersAndRolesForDB(name string) error {
	session, err := dump.SessionProvider.GetSession()
	buffer := dump.getResettableOutputBuffer()
	if err != nil {
		return err
	}

	dbQuery := bson.M{"db": name}
	usersQuery := &db.DeferredQuery{
		Coll:   session.Database("admin").Collection("system.users"),
		Filter: dbQuery,
	}
	_, err = dump.dumpQueryToIntent(usersQuery, dump.manager.Users(), buffer)
	if err != nil {
		return fmt.Errorf("error dumping db users: %v", err)
	}

	rolesQuery := &db.DeferredQuery{
		Coll:   session.Database("admin").Collection("system.roles"),
		Filter: dbQuery,
	}
	_, err = dump.dumpQueryToIntent(rolesQuery, dump.manager.Roles(), buffer)
	if err != nil {
		return fmt.Errorf("error dumping db roles: %v", err)
	}

	versionQuery := &db.DeferredQuery{
		Coll: session.Database("admin").Collection("system.version"),
	}
	_, err = dump.dumpQueryToIntent(versionQuery, dump.manager.AuthVersion(), buffer)
	if err != nil {
		return fmt.Errorf("error dumping db auth version: %v", err)
	}

	return nil
}

// DumpUsersAndRoles dumps all of the users and roles and versions
// TODO: This and DumpUsersAndRolesForDB should be merged, correctly.
func (dump *MongoDump) DumpUsersAndRoles() error {
	var err error
	buffer := dump.getResettableOutputBuffer()
	if dump.manager.Users() != nil {
		err = dump.DumpIntent(dump.manager.Users(), buffer)
		if err != nil {
			return err
		}
	}
	if dump.manager.Roles() != nil {
		err = dump.DumpIntent(dump.manager.Roles(), buffer)
		if err != nil {
			return err
		}
	}
	if dump.manager.AuthVersion() != nil {
		err = dump.DumpIntent(dump.manager.AuthVersion(), buffer)
		if err != nil {
			return err
		}
	}

	return nil
}

// DumpMetadata dumps the metadata for each intent in the manager
// that has metadata.
func (dump *MongoDump) DumpMetadata() error {
	allIntents := dump.manager.Intents()
	buffer := dump.getResettableOutputBuffer()
	for _, intent := range allIntents {
		if intent.MetadataFile != nil {
			err := dump.dumpMetadata(intent, buffer)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

type PreludeData struct {
	ServerVersion string `json:"ServerVersion"`
	ToolVersion   string `json:"ToolVersion"`
}

// DumpPreludeMetadata dumps information about the server and the dump in json format
// Currently only writes the server version and tool version, but we can use this to write other metadata about the dump in the future.
func (dump *MongoDump) DumpPreludeMetadata() error {
	preludeData := PreludeData{
		ServerVersion: dump.serverVersion,
		ToolVersion:   dump.ToolOptions.VersionStr,
	}

	filename := "prelude.json"

	if dump.ToolOptions.Namespace.DB != "" {
		filename = filepath.Join(dump.ToolOptions.Namespace.DB, filename)
	}
	if dump.OutputOptions.Out == "" {
		filename = filepath.Join("dump", filename)
	} else {
		filename = filepath.Join(dump.OutputOptions.Out, filename)
	}
	if dump.OutputOptions.Gzip {
		filename += ".gz"
	}

	log.Logvf(log.DebugLow, "dumping prelude metadata to file %#q", filename)

	file, err := os.Create(filename)
	if errors.Is(err, os.ErrNotExist) {
		// if parent directory doesn't exist, there was no data to dump, don't write prelude.json
		log.Logvf(log.DebugLow, "parent directory does not exist, not writing %#q", filename)
		return nil
	} else if err != nil {
		return fmt.Errorf("failed to open file %#q: %w", filename, err)
	}
	defer file.Close()

	var writer io.WriteCloser = file
	if dump.OutputOptions.Gzip {
		writer = gzip.NewWriter(file)
		defer writer.Close()
	}
	bytes, err := json.Marshal(preludeData)
	if err != nil {
		return fmt.Errorf("error marshaling prelude data: %w", err)
	}

	_, err = writer.Write(bytes)
	if err != nil {
		return fmt.Errorf("failed to write prelude metadata to file %#q: %w", filename, err)
	}

	return nil
}

// nopCloseWriter implements io.WriteCloser. It wraps up a io.Writer, and adds a no-op Close.
type nopCloseWriter struct {
	io.Writer
}

// Close does nothing on nopCloseWriters.
func (*nopCloseWriter) Close() error {
	return nil
}

func (dump *MongoDump) getArchiveOut() (out io.WriteCloser, err error) {
	if dump.OutputOptions.Archive == "-" {
		out = &nopCloseWriter{dump.OutputWriter}
	} else {
		targetStat, err := os.Stat(dump.OutputOptions.Archive)
		if err == nil && targetStat.IsDir() {
			defaultArchiveFilePath :=
				filepath.Join(dump.OutputOptions.Archive, "archive")
			if dump.OutputOptions.Gzip {
				defaultArchiveFilePath = defaultArchiveFilePath + ".gz"
			}
			out, err = os.Create(defaultArchiveFilePath)
			if err != nil {
				return nil, err
			}
		} else {
			out, err = os.Create(dump.OutputOptions.Archive)
			if err != nil {
				return nil, err
			}
		}
	}
	if dump.OutputOptions.Gzip {
		return &util.WrappedWriteCloser{gzip.NewWriter(out), out}, nil
	}
	return out, nil
}

// docPlural returns "document" or "documents" depending on the
// count of documents passed in.
func docPlural(count int64) string {
	return util.Pluralize(int(count), "document", "documents")
}

func (dump *MongoDump) HandleInterrupt() {
	if dump.shutdownIntentsNotifier != nil {
		dump.shutdownIntentsNotifier.Notify()
	}
}
