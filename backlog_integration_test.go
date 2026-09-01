//go:build integration

package nestwal_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/cube-core/app"
	coredata "github.com/tjbdwanghaibo/cube-core/dataengine"
	fmongo "github.com/tjbdwanghaibo/cube-core/mongo"
	corenest "github.com/tjbdwanghaibo/cube-core/nest"
	kitdata "github.com/tjbdwanghaibo/cube-kit/dataengine"
	kitmongo "github.com/tjbdwanghaibo/cube-kit/mongo"
	"github.com/tjbdwanghaibo/cube-kit/nestwal"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const realBacklogRecords = 100_000

func TestRealWALBacklogProjectsOneHundredThousandRecords(t *testing.T) {
	if os.Getenv("ROOST_DATAENGINE_IT") != "1" {
		t.Skip("set ROOST_DATAENGINE_IT=1 or use scripts/integration/dataengine-env.sh test")
	}
	mongoURI := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if mongoURI == "" {
		t.Fatal("ROOST_DATAENGINE_IT_MONGO_URI is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	database := fmt.Sprintf("roost_it_backlog_%d", os.Getpid())

	cfg := viper.New()
	cfg.Set("mongo.uri", mongoURI)
	cfg.Set("mongo.require_replica_set", true)
	cfg.Set("mongo.connect_timeout", 5*time.Second)
	cfg.Set("mongo.transaction_timeout", 15*time.Second)
	registry := app.NewRegistry(cfg)
	mongoMod := kitmongo.NewMongoMod()
	if err := mongoMod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mongoMod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if err := mongoMod.Start(); err != nil {
		t.Fatal(err)
	}
	mongoClient := mongoMod.Client()
	defer func() {
		cleanup, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = mongoClient.Database(database).Drop(cleanup)
		_ = mongoMod.StopWithContext(cleanup)
	}()

	store, err := kitdata.NewMongoStore(mongoClient, kitdata.MongoStoreConfig{DefaultDatabase: database, ServerID: 902})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}

	options := nestwal.DefaultOptions(t.TempDir())
	options.WriterVersion = nestwal.WriterVersionV2
	options.QueueCapacity = realBacklogRecords + 1
	options.BatchMaxRecords = 1024
	options.MaxDiskBytes = 2 << 30
	wal, err := nestwal.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	walOwned := true
	defer func() {
		if walOwned {
			cleanup, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cleanupCancel()
			_ = wal.Close(cleanup)
		}
	}()

	appendStarted := time.Now()
	var lastTicket corenest.CommitTicket
	for index := 0; index < realBacklogRecords; index++ {
		record, recordErr := backlogRecord(index, database)
		if recordErr != nil {
			t.Fatal(recordErr)
		}
		lastTicket, err = wal.Enqueue(ctx, record)
		if err != nil {
			t.Fatalf("enqueue record %d: %v", index, err)
		}
	}
	select {
	case <-lastTicket.Done():
		if err := lastTicket.Err(); err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatalf("waiting for backlog durability: %v", ctx.Err())
	}
	appendElapsed := time.Since(appendStarted)
	walBeforeProjection := wal.Stats()

	projectorOptions := kitdata.DefaultProjectorOptions()
	projectorOptions.ReplayBatchRecords = 1024
	projector, err := kitdata.NewProjector(wal, store, projectorOptions)
	if err != nil {
		t.Fatal(err)
	}
	walOwned = false
	defer func() {
		cleanup, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = projector.Close(cleanup)
	}()
	projectionStarted := time.Now()
	if err := projector.Flush(ctx); err != nil {
		t.Fatalf("project backlog: %v", err)
	}
	projectionElapsed := time.Since(projectionStarted)

	var document bson.M
	if err := mongoClient.Database(database).Collection("backlog_entities").FindOne(ctx, bson.M{"_id": int64(1)}, &document); err != nil {
		t.Fatal(err)
	}
	if got := backlogVersion(document["_version"]); got != realBacklogRecords {
		t.Fatalf("final version=%d, want %d", got, realBacklogRecords)
	}
	if got := backlogInt64(document["value"]); got != realBacklogRecords-1 {
		t.Fatalf("final value=%d, want %d", got, realBacklogRecords-1)
	}
	// Recovery batches write one durable idempotency marker per WAL record in
	// the same Mongo transaction as their ordered mutation bulk.
	assertBacklogCount(t, ctx, mongoClient, database, "_dataengine_transactions", realBacklogRecords)
	assertBacklogCount(t, ctx, mongoClient, database, "_dataengine_receipts", 0)
	walAfterProjection := wal.Stats()
	if got := projector.Stats().Projected; got != realBacklogRecords {
		t.Fatalf("projector projected=%d, want %d", got, realBacklogRecords)
	}
	remaining := 0
	if err := wal.Replay(ctx, func(corenest.CommitFence, corenest.CommitRecord) error {
		remaining++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("WAL still has %d unacknowledged records", remaining)
	}
	if walAfterProjection.Acknowledged == 0 || walAfterProjection.Acknowledged >= realBacklogRecords {
		t.Fatalf("WAL acknowledgement batches=%d, want bounded batching", walAfterProjection.Acknowledged)
	}
	t.Logf("100k backlog: append=%s (%.0f records/s), projection=%s (%.0f records/s), wal_bytes=%d ack_batches=%d", appendElapsed,
		float64(realBacklogRecords)/appendElapsed.Seconds(), projectionElapsed,
		float64(realBacklogRecords)/projectionElapsed.Seconds(), walBeforeProjection.Bytes, walAfterProjection.Acknowledged)
}

func backlogRecord(index int, database string) (coredata.CommitRecord, error) {
	var transactionID coredata.TransactionID
	binary.BigEndian.PutUint64(transactionID[8:], uint64(index+1))
	mutation := coredata.Mutation{
		Key:             coredata.DocumentKey{Database: database, Resource: "backlog_entities", ID: 1},
		ExpectedVersion: uint64(index), NextVersion: uint64(index + 1),
		Mask: 1, Schema: 1, Codec: "bson-v2",
	}
	var err error
	if index == 0 {
		mutation.Kind = coredata.MutationPut
		mutation.Mask = coredata.AllFields
		mutation.Data, err = bson.Marshal(bson.M{"_id": int64(1), "value": int64(index)})
	} else {
		mutation.Kind = coredata.MutationPatch
		mutation.Patch.SetBSON, err = bson.Marshal(bson.D{{Key: "value", Value: int64(index)}})
	}
	if err != nil {
		return coredata.CommitRecord{}, err
	}
	return coredata.CommitRecord{
		ID: transactionID, Handler: "backlog-integration", CreatedAt: time.Now().UnixNano(),
		Durability: corenest.DurabilityPipelined, Mutations: []coredata.Mutation{mutation},
	}, nil
}

func assertBacklogCount(t *testing.T, ctx context.Context, client fmongo.IMongo, database, collection string, want int) {
	t.Helper()
	got, err := client.Database(database).Collection(collection).CountDocuments(ctx, bson.M{})
	if err != nil {
		t.Fatal(err)
	}
	if got != int64(want) {
		t.Fatalf("%s count=%d, want %d", collection, got, want)
	}
}

func backlogVersion(value any) uint64 {
	switch typed := value.(type) {
	case int32:
		return uint64(typed)
	case int64:
		return uint64(typed)
	case uint64:
		return typed
	default:
		return 0
	}
}

func backlogInt64(value any) int64 {
	switch typed := value.(type) {
	case int32:
		return int64(typed)
	case int64:
		return typed
	case int:
		return int64(typed)
	default:
		return -1
	}
}
