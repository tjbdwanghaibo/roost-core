//go:build integration

package dataengine

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-kit/nestwal"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type projectionOnlyMongoStore struct{ delegate ProjectionStore }

func (store *projectionOnlyMongoStore) Project(ctx context.Context, record coredata.CommitRecord) error {
	return store.delegate.Project(ctx, record)
}

type blockingProjectionOnlyMongoStore struct {
	delegate  ProjectionStore
	attempted chan struct{}
	release   chan struct{}
}

func (store *blockingProjectionOnlyMongoStore) Project(ctx context.Context, record coredata.CommitRecord) error {
	select {
	case store.attempted <- struct{}{}:
	default:
	}
	select {
	case <-store.release:
		return store.delegate.Project(ctx, record)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestRealMultiDocumentReceiptAndOutboxAreAtomic(t *testing.T) {
	fx := newRealFixture(t)
	defer fx.close()

	record := realRecord(1, []coredata.Mutation{
		realPut(t, fx.database, "players", 101, 0, 1, bson.M{"name": "alpha"}),
		realPut(t, fx.database, "inventories", 101, 0, 1, bson.M{"gold": int64(50)}),
	})
	record.Receipts = []coredata.Receipt{{Namespace: "saga-step", ID: "command-1", Digest: []byte("digest")}}
	record.Effects = []coredata.Effect{{
		ID: "effect-atomic-1", Topic: "player.changed", Key: "101", Payload: []byte("payload"),
		AvailableAt: time.Now().Add(time.Hour).UnixNano(),
	}}

	ticket, err := fx.runtime.Projector.CommitSystem(fx.context(), record)
	if err != nil {
		t.Fatal(err)
	}
	if err := coredata.WaitProjection(fx.context(), ticket); err != nil {
		t.Fatal(err)
	}

	assertDocumentVersion(t, fx, "players", 101, 1)
	assertDocumentVersion(t, fx, "inventories", 101, 1)
	assertCollectionCount(t, fx, receiptCollection, 1)
	assertCollectionCount(t, fx, outboxCollection, 1)
	assertCollectionCount(t, fx, transactionCollection, 1)
}

func TestRealMultiDocumentFailureRollsBackEarlierMutation(t *testing.T) {
	fx := newRealFixture(t)
	defer fx.close()

	seed := realRecord(2, []coredata.Mutation{
		realPut(t, fx.database, "a_players", 202, 0, 1, bson.M{"name": "before"}),
		realPut(t, fx.database, "z_inventories", 202, 0, 1, bson.M{"gold": int64(10)}),
	})
	if err := fx.runtime.Store.Project(fx.context(), seed); err != nil {
		t.Fatal(err)
	}

	patch, err := bson.Marshal(bson.D{{Key: "name", Value: "must-rollback"}})
	if err != nil {
		t.Fatal(err)
	}
	conflict, err := bson.Marshal(bson.D{{Key: "gold", Value: int64(99)}})
	if err != nil {
		t.Fatal(err)
	}
	record := realRecord(3, []coredata.Mutation{
		{Key: coredata.DocumentKey{Database: fx.database, Resource: "a_players", ID: 202}, Kind: coredata.MutationPatch, ExpectedVersion: 1, NextVersion: 2, Mask: 1, Schema: 1, Codec: "bson-v2", Patch: coredata.FieldPatch{SetBSON: patch}},
		{Key: coredata.DocumentKey{Database: fx.database, Resource: "z_inventories", ID: 202}, Kind: coredata.MutationPatch, ExpectedVersion: 2, NextVersion: 3, Mask: 1, Schema: 1, Codec: "bson-v2", Patch: coredata.FieldPatch{SetBSON: conflict}},
	})
	if err := fx.runtime.Store.Project(fx.context(), record); !errors.Is(err, ErrProjectionConflict) {
		t.Fatalf("err=%v, want ErrProjectionConflict", err)
	}

	doc := findDocument(t, fx, "a_players", 202)
	if doc["name"] != "before" || documentVersion(t, doc) != 1 {
		t.Fatalf("first mutation escaped aborted transaction: %#v", doc)
	}
}

func TestRealPatchConflictFencesWithoutFullFallback(t *testing.T) {
	fx := newRealFixture(t)
	defer fx.close()

	if err := fx.runtime.Store.Project(fx.context(), realRecord(4, []coredata.Mutation{
		realPut(t, fx.database, "players", 303, 0, 1, bson.M{"name": "authoritative"}),
	})); err != nil {
		t.Fatal(err)
	}
	set, err := bson.Marshal(bson.D{{Key: "name", Value: "stale"}})
	if err != nil {
		t.Fatal(err)
	}
	stale := realRecord(5, []coredata.Mutation{{
		Key: coredata.DocumentKey{Database: fx.database, Resource: "players", ID: 303}, Kind: coredata.MutationPatch,
		ExpectedVersion: 2, NextVersion: 3, Mask: 1, Schema: 1, Codec: "bson-v2", Patch: coredata.FieldPatch{SetBSON: set},
	}})
	if err := fx.runtime.Store.Project(fx.context(), stale); !errors.Is(err, ErrProjectionConflict) {
		t.Fatalf("err=%v, want ErrProjectionConflict", err)
	}
	doc := findDocument(t, fx, "players", 303)
	if doc["name"] != "authoritative" || documentVersion(t, doc) != 1 {
		t.Fatalf("stale patch overwrote projection: %#v", doc)
	}
}

func TestRealLoadAndMigrationRestoresTrackerVersion(t *testing.T) {
	fx := newRealFixture(t)
	defer fx.close()

	if err := fx.runtime.Store.Project(fx.context(), realRecord(6, []coredata.Mutation{
		realPutWithSchema(t, fx.database, "profiles", 404, 0, 1, 1, bson.M{"name": "legacy"}),
	})); err != nil {
		t.Fatal(err)
	}
	docs, err := fx.runtime.Store.Load(fx.context(), coredata.LoadSpec{Database: fx.database, Resource: "profiles", BatchSize: 16})
	if err != nil || len(docs) != 1 {
		t.Fatalf("load docs=%d err=%v", len(docs), err)
	}
	dao := &realMigrationDAO{}
	runner, err := NewMigrationRunner(fx.runtime.Projector)
	if err != nil {
		t.Fatal(err)
	}
	migrated, err := runner.Migrate(fx.context(), dao, docs[0])
	if err != nil || !migrated {
		t.Fatalf("migrated=%v err=%v", migrated, err)
	}
	docs, err = fx.runtime.Store.Load(fx.context(), coredata.LoadSpec{Database: fx.database, Resource: "profiles", BatchSize: 16})
	if err != nil || len(docs) != 1 {
		t.Fatalf("reload docs=%d err=%v", len(docs), err)
	}
	if err := dao.RestorePersisted(docs[0].Data, docs[0].Schema, docs[0].Version); err != nil {
		t.Fatal(err)
	}
	if dao.tracker.Version() != 2 || dao.schema != 2 || !dao.migrated {
		t.Fatalf("tracker=%d schema=%d migrated=%v", dao.tracker.Version(), dao.schema, dao.migrated)
	}
}

type realMigrationDAO struct {
	tracker  coredata.Tracker
	schema   uint32
	migrated bool
}

func (*realMigrationDAO) SchemaVersion() uint32 { return 2 }

func (*realMigrationDAO) Migrate(raw []byte, from uint32) ([]byte, error) {
	if from != 1 {
		return nil, errors.New("unexpected source schema")
	}
	var doc bson.M
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	doc["migrated"] = true
	return bson.Marshal(doc)
}

func (dao *realMigrationDAO) RestorePersisted(raw []byte, schema uint32, version uint64) error {
	var doc bson.M
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return err
	}
	dao.schema = schema
	dao.migrated, _ = doc["migrated"].(bool)
	dao.tracker.SetVersion(version)
	dao.tracker.SelfClean()
	return nil
}

func TestRealIntegrationDeadlineIsBounded(t *testing.T) {
	fx := newRealFixture(t)
	defer fx.close()
	deadline, ok := fx.context().Deadline()
	if !ok || time.Until(deadline) > 31*time.Second {
		t.Fatalf("integration context deadline=%v ok=%v", deadline, ok)
	}
}

func TestRealMixedProjectionSegmentsPreserveOrder(t *testing.T) {
	fx := newRealFixture(t)
	defer fx.close()

	first := realRecord(41, []coredata.Mutation{
		realPut(t, fx.database, "mixed_entities", 4101, 0, 1, bson.M{"value": int64(1)}),
	})
	patch2, err := bson.Marshal(bson.D{{Key: "value", Value: int64(2)}})
	if err != nil {
		t.Fatal(err)
	}
	second := realRecord(42, []coredata.Mutation{{
		Key:  coredata.DocumentKey{Database: fx.database, Resource: "mixed_entities", ID: 4101},
		Kind: coredata.MutationPatch, ExpectedVersion: 1, NextVersion: 2, Schema: 1,
		Patch: coredata.FieldPatch{SetBSON: patch2},
	}})
	second.Receipts = []coredata.Receipt{{Namespace: "mixed", ID: "receipt-42", Digest: []byte("42")}}
	second.Effects = []coredata.Effect{{
		ID: "mixed-effect-42", Topic: "mixed.changed", Key: "4101",
		AvailableAt: time.Now().Add(time.Hour).UnixNano(),
	}}
	patch3, err := bson.Marshal(bson.D{{Key: "value", Value: int64(3)}})
	if err != nil {
		t.Fatal(err)
	}
	third := realRecord(43, []coredata.Mutation{{
		Key:  coredata.DocumentKey{Database: fx.database, Resource: "mixed_entities", ID: 4101},
		Kind: coredata.MutationPatch, ExpectedVersion: 2, NextVersion: 3, Schema: 1,
		Patch: coredata.FieldPatch{SetBSON: patch3},
	}})

	options := nestwal.DefaultOptions(t.TempDir())
	options.WriterVersion = nestwal.WriterVersionV2
	wal, err := nestwal.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := wal.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	projector, err := NewProjector(wal, fx.runtime.Store, ProjectorOptions{CloseWAL: false, IdlePoll: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	projector.cancel()
	awaitChan(t, projector.done, "the projector to finish its pass")
	for _, record := range []coredata.CommitRecord{first, second, third} {
		if _, err := wal.Append(fx.context(), record); err != nil {
			t.Fatal(err)
		}
	}
	if processed, err := projector.replayPass(fx.context()); err != nil || processed != 3 {
		t.Fatalf("processed=%d err=%v", processed, err)
	}

	assertDocumentVersion(t, fx, "mixed_entities", 4101, 3)
	assertCollectionCount(t, fx, receiptCollection, 1)
	assertCollectionCount(t, fx, outboxCollection, 1)
	assertCollectionCount(t, fx, transactionCollection, 1)
	replayed := 0
	if err := wal.Replay(fx.context(), func(corenest.CommitFence, coredata.CommitRecord) error {
		replayed++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if replayed != 0 {
		t.Fatalf("replay records=%d, want 0", replayed)
	}
}

func TestRealProjectionOnlyMongoAckFailureRestartPreservesSameEntityOrder(t *testing.T) {
	fx := newRealFixture(t)
	defer fx.close()

	first := realRecord(51, []coredata.Mutation{
		realPut(t, fx.database, "projection_only_restart", 5101, 0, 1, bson.M{"value": int64(1)}),
	})
	set, err := bson.Marshal(bson.D{{Key: "value", Value: int64(2)}})
	if err != nil {
		t.Fatal(err)
	}
	second := realRecord(52, []coredata.Mutation{{
		Key:  coredata.DocumentKey{Database: fx.database, Resource: "projection_only_restart", ID: 5101},
		Kind: coredata.MutationPatch, ExpectedVersion: 1, NextVersion: 2,
		Mask: 1, Schema: 1, Codec: "bson-v2", Patch: coredata.FieldPatch{SetBSON: set},
	}})
	records := []coredata.CommitRecord{first, second}

	walDir := t.TempDir()
	options := nestwal.DefaultOptions(walDir)
	options.WriterVersion = nestwal.WriterVersionV2
	wal, err := nestwal.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	firstStore := &projectionOnlyMongoStore{delegate: fx.runtime.Store}
	firstProjector, err := NewProjector(wal, firstStore, ProjectorOptions{
		ReplayBatchRecords: 16, ReplayBatchBytes: 4 << 20, CloseWAL: false, IdlePoll: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstProjector.cancel()
	awaitChan(t, firstProjector.done, "the first projector to finish its pass")
	for i := range records {
		if _, err := wal.Append(fx.context(), records[i]); err != nil {
			t.Fatal(err)
		}
	}
	ackErr := errors.New("injected checkpoint failure after Mongo success")
	firstProjector.ack = func(context.Context, corenest.CommitFence) error { return ackErr }
	processed, err := firstProjector.replayPass(fx.context())
	if !errors.Is(err, ackErr) || processed != 1 {
		t.Fatalf("pre-restart processed=%d err=%v", processed, err)
	}
	assertDocumentVersion(t, fx, "projection_only_restart", 5101, 1)
	assertWALReplayIDs(t, wal, []coredata.TransactionID{first.ID, second.ID})
	if err := firstProjector.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := wal.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	reopened, err := nestwal.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	restartStore := &blockingProjectionOnlyMongoStore{
		delegate: fx.runtime.Store, attempted: make(chan struct{}, 1), release: make(chan struct{}),
	}
	restarted, err := NewProjector(reopened, restartStore, ProjectorOptions{
		RetryMin: time.Hour, RetryMax: time.Hour, IdlePoll: time.Hour,
		ReplayBatchRecords: 16, ReplayBatchBytes: 4 << 20, CloseWAL: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-restartStore.attempted:
	case <-fx.context().Done():
		t.Fatalf("restarted projector did not attempt WAL replay: %v", fx.context().Err())
	}
	close(restartStore.release)
	if err := restarted.Flush(fx.context()); err != nil {
		t.Fatalf("restart replay raised false projection conflict: %v", err)
	}
	if stats := restarted.Stats(); stats.FatalProjectionConflicts != 0 {
		t.Fatalf("restart stats=%+v", stats)
	}
	doc := findDocument(t, fx, "projection_only_restart", 5101)
	if got := documentVersion(t, doc); got != 2 || doc["value"] != int64(2) {
		t.Fatalf("restart projection=%#v version=%d", doc, got)
	}
	assertWALReplayCount(t, reopened, 0)
	if err := restarted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRealMongoMixedRatioWALReplayAckThroughput(t *testing.T) {
	fx := newRealFixture(t)
	defer fx.close()

	const (
		recordCount = 256
		entityCount = 16
	)
	totalSpecial := 0
	for workloadIndex, workload := range []struct {
		name         string
		specialEvery int
	}{
		{name: "ordinary_only"},
		{name: "special_1_percent", specialEvery: 100},
		{name: "special_10_percent", specialEvery: 10},
		{name: "all_special", specialEvery: 1},
	} {
		t.Run(workload.name, func(t *testing.T) {
			resource := fmt.Sprintf("mixed_ratio_%d", workloadIndex)
			records, specialCount := realMixedRatioRecords(t, fx.database, resource, workloadIndex, recordCount, entityCount, workload.specialEvery)
			options := nestwal.DefaultOptions(t.TempDir())
			options.WriterVersion = nestwal.WriterVersionV2
			wal, err := nestwal.Open(options)
			if err != nil {
				t.Fatal(err)
			}
			projector, err := NewProjector(wal, fx.runtime.Store, ProjectorOptions{
				ReplayBatchRecords: recordCount, ReplayBatchBytes: 64 << 20,
				CloseWAL: false, IdlePoll: time.Hour,
			})
			if err != nil {
				t.Fatal(err)
			}
			projector.cancel()
			<-projector.done
			for i := range records {
				if _, err := wal.Append(fx.context(), records[i]); err != nil {
					t.Fatal(err)
				}
			}
			if err := wal.Sync(fx.context()); err != nil {
				t.Fatal(err)
			}
			ack := wal.Ack
			ackCalls := 0
			projector.ack = func(ctx context.Context, fence corenest.CommitFence) error {
				ackCalls++
				return ack(ctx, fence)
			}

			started := time.Now()
			if err := projector.Flush(fx.context()); err != nil {
				t.Fatal(err)
			}
			elapsed := time.Since(started)
			if stats := projector.Stats(); stats.Projected != recordCount || ackCalls == 0 {
				t.Fatalf("stats=%+v checkpoint acks=%d", stats, ackCalls)
			}
			assertWALReplayCount(t, wal, 0)
			for entityIndex := 0; entityIndex < entityCount; entityIndex++ {
				assertDocumentVersion(t, fx, resource, int64(entityIndex+1), recordCount/entityCount)
			}
			totalSpecial += specialCount
			assertCollectionCount(t, fx, receiptCollection, int64(totalSpecial))
			t.Logf("backend=real MongoDB + file WAL workload=%s records=%d entities=%d special=%d checkpoint_acks=%d elapsed=%s throughput=%.0f records/s",
				workload.name, recordCount, entityCount, specialCount, ackCalls, elapsed, float64(recordCount)/elapsed.Seconds())
			if err := projector.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := wal.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func realMixedRatioRecords(t *testing.T, database, resource string, workloadIndex, count, entityCount, specialEvery int) ([]coredata.CommitRecord, int) {
	t.Helper()
	versions := make([]uint64, entityCount)
	records := make([]coredata.CommitRecord, count)
	specialCount := 0
	for i := range records {
		entityIndex := i % entityCount
		entityID := int64(entityIndex + 1)
		expected := versions[entityIndex]
		next := expected + 1
		var mutation coredata.Mutation
		if expected == 0 {
			mutation = realPut(t, database, resource, entityID, expected, next, bson.M{"value": int64(i)})
		} else {
			set, err := bson.Marshal(bson.D{{Key: "value", Value: int64(i)}})
			if err != nil {
				t.Fatal(err)
			}
			mutation = coredata.Mutation{
				Key:  coredata.DocumentKey{Database: database, Resource: resource, ID: entityID},
				Kind: coredata.MutationPatch, ExpectedVersion: expected, NextVersion: next,
				Mask: 1, Schema: 1, Codec: "bson-v2", Patch: coredata.FieldPatch{SetBSON: set},
			}
		}
		var transactionID coredata.TransactionID
		binary.BigEndian.PutUint64(transactionID[:8], uint64(workloadIndex+1))
		binary.BigEndian.PutUint64(transactionID[8:], uint64(i+1))
		record := coredata.CommitRecord{
			ID: transactionID, Handler: "real-mixed-ratio", CreatedAt: time.Now().UnixNano(),
			Durability: corenest.DurabilityAsync, Mutations: []coredata.Mutation{mutation},
		}
		if specialEvery > 0 && (i+1)%specialEvery == 0 {
			record.Receipts = []coredata.Receipt{{
				Namespace: "real-mixed-ratio", ID: fmt.Sprintf("%s-%d", resource, i+1),
			}}
			specialCount++
		}
		records[i] = record
		versions[entityIndex] = next
	}
	return records, specialCount
}

func TestRealSagaReceiptTransactionThroughput(t *testing.T) {
	fx := newRealFixture(t)
	defer fx.close()

	const records = 1_000
	started := time.Now()
	for index := 0; index < records; index++ {
		var id coredata.TransactionID
		binary.BigEndian.PutUint64(id[8:], uint64(index+1))
		var mutation coredata.Mutation
		if index == 0 {
			mutation = realPut(t, fx.database, "saga_entities", 701, 0, 1, bson.M{"step": int64(index)})
		} else {
			set, err := bson.Marshal(bson.D{{Key: "step", Value: int64(index)}})
			if err != nil {
				t.Fatal(err)
			}
			mutation = coredata.Mutation{
				Key:  coredata.DocumentKey{Database: fx.database, Resource: "saga_entities", ID: 701},
				Kind: coredata.MutationPatch, ExpectedVersion: uint64(index), NextVersion: uint64(index + 1),
				Mask: 1, Schema: 1, Codec: "bson-v2", Patch: coredata.FieldPatch{SetBSON: set},
			}
		}
		record := coredata.CommitRecord{
			ID: id, Handler: "saga-throughput", CreatedAt: time.Now().UnixNano(),
			Durability: corenest.DurabilityStrict, Mutations: []coredata.Mutation{mutation},
			Receipts: []coredata.Receipt{{Namespace: "saga-throughput", ID: strconv.Itoa(index + 1)}},
		}
		if err := fx.runtime.Store.Project(fx.context(), record); err != nil {
			t.Fatalf("project Saga transaction %d: %v", index, err)
		}
	}
	elapsed := time.Since(started)
	assertDocumentVersion(t, fx, "saga_entities", 701, records)
	assertCollectionCount(t, fx, transactionCollection, records)
	assertCollectionCount(t, fx, receiptCollection, records)
	t.Logf("Saga receipt transactions: %s (%.0f records/s)", elapsed, float64(records)/elapsed.Seconds())
}
