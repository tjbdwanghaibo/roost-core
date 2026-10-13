//go:build integration

package dataengine

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type projectionOnlyMongoStore struct{ delegate engine.ProjectionStore }

func (store *projectionOnlyMongoStore) Project(ctx context.Context, record coredata.CommitRecord) error {
	return store.delegate.Project(ctx, record)
}

type blockingProjectionOnlyMongoStore struct {
	delegate  engine.ProjectionStore
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
	assertCollectionCount(t, fx, engine.ReceiptCollection, 1)
	assertCollectionCount(t, fx, engine.OutboxCollection, 1)
	assertCollectionCount(t, fx, engine.TransactionCollection, 1)
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
	if err := fx.runtime.Store.Project(fx.context(), record); !errors.Is(err, engine.ErrProjectionConflict) {
		t.Fatalf("err=%v, want engine.ErrProjectionConflict", err)
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
	if err := fx.runtime.Store.Project(fx.context(), stale); !errors.Is(err, engine.ErrProjectionConflict) {
		t.Fatalf("err=%v, want engine.ErrProjectionConflict", err)
	}
	doc := findDocument(t, fx, "players", 303)
	if doc["name"] != "authoritative" || documentVersion(t, doc) != 1 {
		t.Fatalf("stale patch overwrote projection: %#v", doc)
	}
}

func TestRealLoadRestoresTrackerVersion(t *testing.T) {
	fx := newRealFixture(t)
	defer fx.close()

	if err := fx.runtime.Store.Project(fx.context(), realRecord(6, []coredata.Mutation{
		realPutWithSchema(t, fx.database, "profiles", 404, 0, 1, 1, bson.M{"name": "current"}),
	})); err != nil {
		t.Fatal(err)
	}
	docs, err := fx.runtime.Store.Load(fx.context(), coredata.LoadSpec{Database: fx.database, Resource: "profiles", BatchSize: 16})
	if err != nil || len(docs) != 1 {
		t.Fatalf("load docs=%d err=%v", len(docs), err)
	}
	dao := &realLoadedDAO{}
	if err := dao.RestorePersisted(docs[0].Data, docs[0].Schema, docs[0].Version); err != nil {
		t.Fatal(err)
	}
	if dao.tracker.Version() != 1 || dao.schema != 1 {
		t.Fatalf("tracker=%d schema=%d", dao.tracker.Version(), dao.schema)
	}
}

type realLoadedDAO struct {
	id      int64
	tracker coredata.Tracker
	schema  uint32
}

func (dao *realLoadedDAO) Id() int64         { return dao.id }
func (*realLoadedDAO) SchemaVersion() uint32 { return 1 }

func (dao *realLoadedDAO) RestorePersisted(raw []byte, schema uint32, version uint64) error {
	if schema != dao.SchemaVersion() {
		return coredata.ErrSchemaMismatch
	}
	var doc struct {
		ID int64 `bson:"_id"`
	}
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return err
	}
	dao.id = doc.ID
	dao.schema = schema
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
	wal, err := nestwal.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := wal.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	// 手动回放：没有后台循环抢先投影，下面追加的三条由一次 ReplayPass 处理。
	projector := manualRealProjector(t, wal, fx.runtime.Store)
	for _, record := range []coredata.CommitRecord{first, second, third} {
		if _, err := wal.Append(fx.context(), record); err != nil {
			t.Fatal(err)
		}
	}
	if processed, err := projector.ReplayPass(fx.context()); err != nil || processed != 3 {
		t.Fatalf("processed=%d err=%v", processed, err)
	}

	assertDocumentVersion(t, fx, "mixed_entities", 4101, 3)
	assertCollectionCount(t, fx, engine.ReceiptCollection, 1)
	assertCollectionCount(t, fx, engine.OutboxCollection, 1)
	assertCollectionCount(t, fx, engine.TransactionCollection, 1)
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
	wal, err := nestwal.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	firstStore := &projectionOnlyMongoStore{delegate: fx.runtime.Store}
	firstProjector, err := engine.NewProjector(wal, firstStore, engine.ProjectorOptions{
		ReplayBatchRecords: 16, ReplayBatchBytes: 4 << 20, CloseWAL: false, ManualReplay: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := range records {
		if _, err := wal.Append(fx.context(), records[i]); err != nil {
			t.Fatal(err)
		}
	}
	ackErr := errors.New("injected checkpoint failure after Mongo success")
	firstProjector.OverrideAck(func(context.Context, corenest.CommitFence) error { return ackErr })
	processed, err := firstProjector.ReplayPass(fx.context())
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
	restarted, err := engine.NewProjector(reopened, restartStore, engine.ProjectorOptions{
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
			wal, err := nestwal.Open(options)
			if err != nil {
				t.Fatal(err)
			}
			projector, err := engine.NewProjector(wal, fx.runtime.Store, engine.ProjectorOptions{
				ReplayBatchRecords: recordCount, ReplayBatchBytes: 64 << 20,
				CloseWAL: false, ManualReplay: true,
			})
			if err != nil {
				t.Fatal(err)
			}
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
			projector.OverrideAck(func(ctx context.Context, fence corenest.CommitFence) error {
				ackCalls++
				return ack(ctx, fence)
			})

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
			assertCollectionCount(t, fx, engine.ReceiptCollection, int64(totalSpecial))
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
			Durability: corenest.DurabilityAsync.Record(), Mutations: []coredata.Mutation{mutation},
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
			Durability: corenest.DurabilityStrict.Record(), Mutations: []coredata.Mutation{mutation},
			Receipts: []coredata.Receipt{{Namespace: "saga-throughput", ID: strconv.Itoa(index + 1)}},
		}
		if err := fx.runtime.Store.Project(fx.context(), record); err != nil {
			t.Fatalf("project Saga transaction %d: %v", index, err)
		}
	}
	elapsed := time.Since(started)
	assertDocumentVersion(t, fx, "saga_entities", 701, records)
	assertCollectionCount(t, fx, engine.TransactionCollection, records)
	assertCollectionCount(t, fx, engine.ReceiptCollection, records)
	t.Logf("Saga receipt transactions: %s (%.0f records/s)", elapsed, float64(records)/elapsed.Seconds())
}

func assertWALReplayIDs(t *testing.T, wal *nestwal.WAL, want []coredata.TransactionID) {
	t.Helper()
	got := make([]coredata.TransactionID, 0, len(want))
	if err := wal.Replay(context.Background(), func(_ corenest.CommitFence, record coredata.CommitRecord) error {
		got = append(got, record.ID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("replay transaction IDs=%v want=%v", got, want)
	}
}

func assertWALReplayCount(t *testing.T, wal *nestwal.WAL, want int) {
	t.Helper()
	got := 0
	if err := wal.Replay(context.Background(), func(corenest.CommitFence, coredata.CommitRecord) error {
		got++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("replay records=%d want=%d", got, want)
	}
}

// 真实 Mongo 上走正式 kit 装配出的 MongoStore：真实服务端在事务内任一写错误后中止事务，之后同一事务的读返回
// NoSuchTransaction；投影撞唯一键后不能仍在事务内读回裁决，否则驱动重跑到 transaction_timeout、WAL 头部卡住。这里对每种身份
// 裁决断言结论、fatal 分类、DAO 是否落库，以及耗时远小于事务超时。

// rr34TransactionTimeout 与 newRealFixtureWithNATS 的 mongo.transaction_timeout 一致。
const rr34TransactionTimeout = 15 * time.Second

// rr34Budget 是一次裁决允许的耗时：驱动重跑到超时必然越过它。
const rr34Budget = rr34TransactionTimeout / 5

const rr34Resource = "rr34_heroes"

type rr34Outcome struct {
	err     error
	elapsed time.Duration
	fatal   uint64
	written bool
}

// projectThroughWAL 把一条记录写进独立 WAL，由手动 Projector 投影一遍：fatal 分类与
// 生产投影器是同一段代码。每次用新的 WAL/Projector，前一个用例的 fatal 不会遮住后一个。
func projectThroughWAL(t *testing.T, fx *realFixture, record coredata.CommitRecord) rr34Outcome {
	t.Helper()
	opts := nestwal.DefaultOptions(t.TempDir())
	wal, err := nestwal.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wal.Close(context.Background()) })
	projector := manualRealProjector(t, wal, fx.runtime.Store)
	if _, err := wal.Append(fx.context(), record); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = projector.ReplayPass(fx.context())
	elapsed := time.Since(start)
	written, countErr := fx.mongo.Database(fx.database).Collection(rr34Resource).CountDocuments(context.Background(), bson.M{"_id": record.Mutations[0].Key.ID})
	if countErr != nil {
		t.Fatal(countErr)
	}
	return rr34Outcome{err: err, elapsed: elapsed, fatal: projector.Stats().FatalProjectionConflicts, written: written == 1}
}

func TestRealProjectionIdentityVerdictsAcrossTransactions(t *testing.T) {
	fx := newRealFixture(t)
	defer fx.close()
	// 让 outbox worker 不领取这些 effect：投递后 outbox 文档会被删除，effect ID 复用的
	// 身份裁决只在文档仍在时成立（见 RR-20260926-34 修复记录）。
	later := time.Now().Add(time.Hour).UnixNano()
	receipt := coredata.Receipt{Namespace: "saga-step", ID: "rr34-cmd-1", Digest: []byte("d1"), Payload: []byte("p1")}
	put := func(seed byte, id int64) coredata.CommitRecord {
		return realRecord(seed, []coredata.Mutation{realPut(t, fx.database, rr34Resource, id, 0, 1, bson.M{"level": int32(1)})})
	}
	first := put(0x31, 1)
	first.Receipts = []coredata.Receipt{receipt}
	first.Effects = []coredata.Effect{{ID: "rr34-effect-1", Topic: "hero.changed", Payload: []byte{1}, AvailableAt: later}}
	if err := fx.runtime.Store.Project(fx.context(), first); err != nil {
		t.Fatalf("first projection: %v", err)
	}

	t.Run("identical receipt from another transaction succeeds", func(t *testing.T) {
		record := put(0x32, 2)
		record.Receipts = []coredata.Receipt{receipt}
		got := projectThroughWAL(t, fx, record)
		if got.err != nil || got.fatal != 0 || !got.written {
			t.Fatalf("err=%v fatal=%d written=%v elapsed=%s, want success with the DAO written", got.err, got.fatal, got.written, got.elapsed)
		}
		if got.elapsed >= rr34Budget {
			t.Fatalf("elapsed=%s, want well under transaction_timeout=%s", got.elapsed, rr34TransactionTimeout)
		}
	})

	t.Run("receipt payload drift is a fatal receipt identity conflict", func(t *testing.T) {
		record := put(0x33, 3)
		record.Receipts = []coredata.Receipt{{Namespace: "saga-step", ID: "rr34-cmd-1", Digest: []byte("d1"), Payload: []byte("other")}}
		got := projectThroughWAL(t, fx, record)
		if !errors.Is(got.err, engine.ErrReceiptIdentity) || got.fatal != 1 || got.written {
			t.Fatalf("err=%v fatal=%d written=%v elapsed=%s, want fatal ErrReceiptIdentity and a rolled-back DAO", got.err, got.fatal, got.written, got.elapsed)
		}
		if got.elapsed >= rr34Budget {
			t.Fatalf("elapsed=%s, want well under transaction_timeout=%s", got.elapsed, rr34TransactionTimeout)
		}
	})

	// saga start 的 effect ID 是命令内容哈希（saga/nest.go），同一 start 在两个事务各发一次
	// 就是这种形状：按现有设计是 fatal 身份冲突，修复保持不变。
	t.Run("effect id reused by another transaction is a fatal transaction identity conflict", func(t *testing.T) {
		record := put(0x34, 4)
		record.Effects = []coredata.Effect{{ID: "rr34-effect-1", Topic: "hero.changed", Payload: []byte{1}, AvailableAt: later}}
		got := projectThroughWAL(t, fx, record)
		if !errors.Is(got.err, engine.ErrTransactionIdentity) || got.fatal != 1 || got.written {
			t.Fatalf("err=%v fatal=%d written=%v elapsed=%s, want fatal ErrTransactionIdentity and a rolled-back DAO", got.err, got.fatal, got.written, got.elapsed)
		}
		if got.elapsed >= rr34Budget {
			t.Fatalf("elapsed=%s, want well under transaction_timeout=%s", got.elapsed, rr34TransactionTimeout)
		}
	})

	t.Run("replaying the committed record is absorbed by its marker", func(t *testing.T) {
		start := time.Now()
		if err := fx.runtime.Store.Project(fx.context(), first); err != nil {
			t.Fatalf("replay: %v", err)
		}
		if elapsed := time.Since(start); elapsed >= rr34Budget {
			t.Fatalf("replay elapsed=%s", elapsed)
		}
		assertCollectionCount(t, fx, engine.OutboxCollection, 1)
		assertCollectionCount(t, fx, engine.ReceiptCollection, 1)
	})

	t.Run("concurrent projection of one record converges on one marker", func(t *testing.T) {
		record := put(0x35, 5)
		record.Effects = []coredata.Effect{{ID: "rr34-effect-5", Topic: "hero.changed", AvailableAt: later}}
		record.Receipts = []coredata.Receipt{{Namespace: "saga-step", ID: "rr34-cmd-5", Digest: []byte("d5")}}
		errs := make([]error, 8)
		var wg sync.WaitGroup
		start := time.Now()
		for i := range errs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs[i] = fx.runtime.Store.Project(fx.context(), record)
			}(i)
		}
		wg.Wait()
		elapsed := time.Since(start)
		for i, err := range errs {
			if err != nil {
				t.Fatalf("projection %d: %v", i, err)
			}
		}
		markers, err := fx.mongo.Database(fx.database).Collection(engine.TransactionCollection).CountDocuments(fx.context(), bson.M{"_id": record.ID.String()})
		if err != nil {
			t.Fatal(err)
		}
		if markers != 1 {
			t.Fatalf("markers=%d, want 1", markers)
		}
		if elapsed >= rr34TransactionTimeout {
			t.Fatalf("concurrent elapsed=%s reached transaction_timeout", elapsed)
		}
	})
}

func realLocalMultiRecords(t *testing.T, fx *realFixture) []coredata.CommitRecord {
	var records []coredata.CommitRecord
	for i := range 4 {
		// 两个 Entity，各有两个 DAO；跨集合且持续修改同一文档。
		var mutations []coredata.Mutation
		for _, collection := range []string{"a_wallets", "z_inventories"} {
			for _, id := range []int64{1, 2} {
				mutations = append(mutations, realPut(t, fx.database, collection, id, uint64(i), uint64(i+1), bson.M{"value": i + 1}))
			}
		}
		records = append(records, realRecord(byte(120+i), mutations))
	}
	return records
}

// manualRealProjector 返回不启动后台循环的 Projector，由测试逐步调用 ReplayPass / Flush（Close 之后会拒绝 ReplayPass，
// 不能用“先 Close 停循环”的办法）。
func manualRealProjector(t *testing.T, wal *nestwal.WAL, store engine.ProjectionStore) *engine.Projector {
	t.Helper()
	p, err := engine.NewProjector(wal, store, engine.ProjectorOptions{CloseWAL: false, CheckpointInterval: time.Hour, ManualReplay: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	return p
}

func TestRealLocalMultiBatchOrderIdentityAndLostCheckpoint(t *testing.T) {
	fx := newRealFixture(t)
	defer fx.close()
	records := realLocalMultiRecords(t, fx)
	opts := nestwal.DefaultOptions(t.TempDir())
	wal, err := nestwal.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = wal.Close(context.Background()) }()
	p := manualRealProjector(t, wal, fx.runtime.Store)
	for _, r := range records {
		if _, err = wal.Append(fx.context(), r); err != nil {
			t.Fatal(err)
		}
	}
	ackLost := errors.New("injected lost checkpoint after batch")
	p.OverrideAck(func(context.Context, corenest.CommitFence) error { return ackLost })
	if n, err := p.ReplayPass(fx.context()); n != 4 || !errors.Is(err, ackLost) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	for _, c := range []string{"a_wallets", "z_inventories"} {
		for _, id := range []int64{1, 2} {
			assertDocumentVersion(t, fx, c, id, 4)
		}
	}
	assertCollectionCount(t, fx, engine.TransactionCollection, 4)
	assertWALReplayCount(t, wal, 4)
	if err = wal.Close(fx.context()); err != nil {
		t.Fatal(err)
	}
	wal, err = nestwal.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	// 已提交 marker 必须允许在当前版本 4 时重放版本 1..4。
	p = manualRealProjector(t, wal, fx.runtime.Store)
	if err = p.Flush(fx.context()); err != nil {
		t.Fatal(err)
	}
	assertWALReplayCount(t, wal, 0)
	assertCollectionCount(t, fx, engine.TransactionCollection, 4)
	changed := coredata.CloneCommitRecord(records[0])
	changed.Handler = "different-payload"
	if err = fx.runtime.Store.ProjectBatch(fx.context(), []coredata.CommitRecord{changed, records[1]}); !errors.Is(err, engine.ErrTransactionIdentity) {
		t.Fatalf("identity=%v", err)
	}
}

func TestRealLocalMultiBatchLateConflictRollsBackThenAcknowledgesPrefix(t *testing.T) {
	fx := newRealFixture(t)
	defer fx.close()
	records := realLocalMultiRecords(t, fx)
	// 第三笔的最后一个 DAO 冲突，前两笔与第四笔均不可提前提交。
	records[2].Mutations[3].ExpectedVersion = 8
	records[2].Mutations[3].NextVersion = 9
	if err := fx.runtime.Store.ProjectBatch(fx.context(), records); !errors.Is(err, engine.ErrProjectionBatchNeedsPerRecord) {
		t.Fatalf("batch=%v", err)
	}
	for _, c := range []string{"a_wallets", "z_inventories", engine.TransactionCollection} {
		assertCollectionCount(t, fx, c, 0)
	}
	wal, err := nestwal.Open(nestwal.DefaultOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer wal.Close(context.Background())
	p := manualRealProjector(t, wal, fx.runtime.Store)
	for _, r := range records {
		if _, err = wal.Append(fx.context(), r); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := p.ReplayPass(fx.context()); n != 2 || !errors.Is(err, engine.ErrProjectionConflict) {
		t.Fatalf("fallback n=%d err=%v", n, err)
	}
	for _, c := range []string{"a_wallets", "z_inventories"} {
		for _, id := range []int64{1, 2} {
			assertDocumentVersion(t, fx, c, id, 2)
		}
	}
	assertCollectionCount(t, fx, engine.TransactionCollection, 2)
	assertWALReplayCount(t, wal, 2)
	if p.Stats().FatalProjectionConflicts != 1 {
		t.Fatalf("stats=%+v", p.Stats())
	}
}
