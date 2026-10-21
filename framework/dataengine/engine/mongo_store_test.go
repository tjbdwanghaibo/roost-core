// MongoStore：版本化 Put / Patch / 删除墓碑、批投影单事务与 marker、身份裁决、读取与流式加载、effect 暂存竞态。

package engine

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// These tests run against internal/mongofake, an in-memory Mongo that actually
// evaluates filters, updates and unique indexes. That matters here more than
// anywhere else in the repo: every correctness property of projection is
// carried by a query predicate (the exact-version CAS, the transaction marker,
// the lease fence, the effect/receipt identity). A fake that returned canned
// results could not tell a correct predicate from a wrong one, so these tests
// seed real documents and assert stored state instead.

const testDatabase = "game"

type mongoRemoteProjectionFake struct {
	stored  int
	applied int
}

func (fake *mongoRemoteProjectionFake) ApplyRemoteCommitsInTransaction(_ context.Context, commits []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error) {
	fake.stored += len(commits)
	return nil, nil
}

func (fake *mongoRemoteProjectionFake) ApplyRemoteCommits(_ context.Context, _ entity.RemoteTransactionID, commits []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error) {
	fake.applied += len(commits)
	return nil, nil
}

func newMongoStoreTest(t *testing.T) (*MongoStore, *mongotest.Client, *mongotest.Collection) {
	t.Helper()
	client := mongotest.NewClient()
	store, err := NewMongoStore(client, MongoStoreConfig{Databases: identityDatabases(t), DefaultDatabase: testDatabase, ServerID: 3, TransactionReceiptTTL: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return store, client, client.Collection(testDatabase, "heroes")
}

func markerCollection(client *mongotest.Client) *mongotest.Collection {
	return client.Collection(testDatabase, TransactionCollection)
}

// seedHero installs the document an exact-version mutation expects to find.
func seedHero(t *testing.T, coll *mongotest.Collection, version uint64) {
	t.Helper()
	if err := coll.Seed(bson.M{"_id": int64(7), "_version": version, "level": int32(1)}); err != nil {
		t.Fatal(err)
	}
}

func testMutationRecord(kind coredata.MutationKind) coredata.CommitRecord {
	var id coredata.TransactionID
	id[15] = 9
	mutation := coredata.Mutation{
		Key:             coredata.DocumentKey{Database: testDatabase, Resource: "heroes", ID: 7},
		Kind:            kind,
		ExpectedVersion: 4,
		NextVersion:     5,
		Mask:            1,
		Schema:          2,
		Codec:           "bson-v2",
	}
	if kind == coredata.MutationPut {
		mutation.Data, _ = bson.Marshal(bson.M{"_id": int64(7), "level": int32(5)})
	}
	if kind == coredata.MutationPatch {
		mutation.Patch.SetBSON, _ = bson.Marshal(bson.D{{Key: "level", Value: int32(5)}})
	}
	return coredata.CommitRecord{ID: id, Mutations: []coredata.Mutation{mutation}}
}

func TestMongoStoreUsesAbsoluteExpiryForReceipts(t *testing.T) {
	store, client, _ := newMongoStoreTest(t)
	if err := store.EnsureInfrastructure(context.Background()); err != nil {
		t.Fatal(err)
	}
	receipts := client.Collection(testDatabase, ReceiptCollection)
	if len(receipts.Indexes) != 1 {
		t.Fatalf("receipt indexes=%d, want 1", len(receipts.Indexes))
	}
	index := receipts.Indexes[0]
	if !index.ExpireAt || index.TTL != 0 || !index.RecreateOnConflict {
		t.Fatalf("receipt expiry index=%+v", index)
	}
	// The outbox claim path and the effect-id uniqueness both depend on their
	// index existing; assert them rather than trusting the call went through.
	outbox := client.Collection(testDatabase, OutboxCollection)
	if !outbox.HasIndex("available_at", "lease_until") || !outbox.HasIndex("effect_id") {
		t.Fatalf("outbox indexes=%+v", outbox.Indexes)
	}
}

func leaseFenceReceipt(t *testing.T, documentID, owner string, token uint64, digest []byte) coredata.Receipt {
	t.Helper()
	receipt, err := coredata.NewLeaseFenceReceipt(coredata.LeaseFence{
		Database: testDatabase, Resource: "_dataengine_inbox_claims", DocumentID: documentID,
		Owner: owner, Token: token, Digest: digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

// seedClaim installs a live saga-step claim exactly as saga's
// DataEngineStepInbox writes it. The field names and the "pending" literal are
// the contract the projector's fence filter depends on.
func seedClaim(t *testing.T, client *mongotest.Client, documentID, owner string, token uint64, digest []byte, leaseUntil time.Time) {
	t.Helper()
	claims := client.Collection(testDatabase, "_dataengine_inbox_claims")
	if err := claims.Seed(bson.M{
		"_id": documentID, "owner": owner, "lease_token": token, "digest": digest,
		"status": "pending", "lease_until": leaseUntil,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMongoStoreLeaseFenceSkipsStaleSagaTransaction(t *testing.T) {
	store, client, collection := newMongoStoreTest(t)
	seedHero(t, collection, 4)
	record := testMutationRecord(coredata.MutationPatch)
	// No claim document exists at all: the lease is gone.
	record.Receipts = append(record.Receipts, leaseFenceReceipt(t, "saga-step/cmd-1", "worker-1", 7, bytes.Repeat([]byte{1}, 32)))

	if err := store.Project(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if doc, _ := collection.Lookup(int64(7)); doc["_version"] != int64(4) || doc["level"] != int32(1) {
		t.Fatalf("stale fenced transaction mutated business state: %v", doc)
	}
	markers := markerCollection(client).Documents()
	if len(markers) != 1 || markers[0]["skipped"] != true {
		t.Fatalf("transaction marker=%v, want skipped", markers)
	}
}

func TestMongoStoreLeaseFenceRejectsExpiredLease(t *testing.T) {
	store, client, collection := newMongoStoreTest(t)
	seedHero(t, collection, 4)
	digest := bytes.Repeat([]byte{5}, 32)
	// The claim exists and matches on identity, but its lease already expired.
	seedClaim(t, client, "saga-step/cmd-expired", "worker-1", 7, digest, time.Now().UTC().Add(-time.Minute))
	record := testMutationRecord(coredata.MutationPatch)
	record.Receipts = append(record.Receipts, leaseFenceReceipt(t, "saga-step/cmd-expired", "worker-1", 7, digest))

	if err := store.Project(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if doc, _ := collection.Lookup(int64(7)); doc["_version"] != int64(4) {
		t.Fatalf("expired lease applied the mutation: %v", doc)
	}
	if markers := markerCollection(client).Documents(); len(markers) != 1 || markers[0]["skipped"] != true {
		t.Fatalf("marker=%v, want skipped", markers)
	}
}

// WAL 里 Remote 写与 lease fence 同在的历史记录不再投影：MongoStore 在任何写之前以
// ErrRetiredRemoteLeaseFenceRecord 拒绝，旧进程留下的 skipped 标记也不改变这一点。
// 历史：RR-20260926-19 曾把它投影为 Remote Rejected；维护者 2026-10-13 决定移除旧格式兼容。
func TestMongoStoreRefusesARetiredRemoteLeaseFenceRecordWithoutWriting(t *testing.T) {
	// 只需要一个托管且没有 builder 的 kind。原用 242，与 entity_repository_promises_test.go 的本地 builder
	// dataEngineNilBuilderKind 撞号：两个用例先后运行时 kind 被升级成 managed 而 builder 仍是本地生命周期，
	// RR-20260926-71 起注册表拒绝这种升级。
	const kind entity.EntityKind = 236
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	entityID, err := entity.BuildEntityID(78, kind)
	if err != nil {
		t.Fatal(err)
	}
	store, client, _ := newMongoStoreTest(t)
	remoteProjection := &mongoRemoteProjectionFake{}
	if err := store.SetRemoteProjection(remoteProjection, remoteProjection); err != nil {
		t.Fatal(err)
	}
	var txID coredata.TransactionID
	txID[15] = 45
	remote := entity.RemoteCommit{
		TransactionID: entity.RemoteTransactionID(txID), EntityID: entityID, Kind: kind,
		BaseVersion: 1, NextVersion: 2, MarkerEpoch: 1, LockFence: 1, RouteEpoch: 1, Schema: 1, Codec: 1,
		Mutations: []entity.RemoteDataMutation{{Collection: "heroes", ID: entityID, Version: 2, Mask: 1, Data: []byte("remote")}},
	}
	record := coredata.CommitRecord{
		ID:       txID,
		Receipts: []coredata.Receipt{leaseFenceReceipt(t, "saga-step/cmd-remote", "worker-1", 8, bytes.Repeat([]byte{2}, 32))},
		Mutations: []coredata.Mutation{{
			Key: coredata.DocumentKey{Resource: "heroes", ID: entityID}, Kind: coredata.MutationPut,
			ExpectedVersion: 1, NextVersion: 2, Remote: &remote,
		}},
	}
	// 准入早已拒绝 Remote + lease fence；这种记录只可能来自旧版本 WAL，投影在任何写之前拒绝。
	if err := store.Project(context.Background(), record); !errors.Is(err, ErrRetiredRemoteLeaseFenceRecord) {
		t.Fatalf("Project(remote + lease fence) = %v, want ErrRetiredRemoteLeaseFenceRecord", err)
	}
	if remoteProjection.stored != 0 || remoteProjection.applied != 0 || markerCollection(client).Len() != 0 {
		t.Fatalf("retired record wrote: remote stored=%d published=%d markers=%d", remoteProjection.stored, remoteProjection.applied, markerCollection(client).Len())
	}
	// 旧版本可能已为它写下 skipped 标记（WAL 尚未 ack）：照样拒绝，不读标记、不再写。
	digest, err := digestRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.insertTransactionMarker(context.Background(), transactionDocument{ID: txID.String(), Digest: digest, CreatedAt: time.Now().UTC(), Skipped: true}); err != nil {
		t.Fatal(err)
	}
	if skipped, err := store.ProjectFenced(context.Background(), record); skipped || !errors.Is(err, ErrRetiredRemoteLeaseFenceRecord) {
		t.Fatalf("ProjectFenced over an old skipped marker = skipped=%v err=%v, want the retired-record refusal", skipped, err)
	}
	if remoteProjection.stored != 0 || remoteProjection.applied != 0 || markerCollection(client).Len() != 1 {
		t.Fatalf("retired record wrote on replay: remote stored=%d published=%d markers=%d", remoteProjection.stored, remoteProjection.applied, markerCollection(client).Len())
	}
}

func TestMongoStoreLeaseFenceAppliesOnlyMatchingOwnerAndToken(t *testing.T) {
	claimDigest := bytes.Repeat([]byte{3}, 32)
	// Each case keeps a live, identity-matching claim in storage and varies the
	// fence the transaction carries. Only the exact match may apply.
	for name, tc := range map[string]struct {
		owner  string
		token  uint64
		digest []byte
		apply  bool
	}{
		"exact match":  {"worker-1", 7, claimDigest, true},
		"other owner":  {"worker-2", 7, claimDigest, false},
		"other token":  {"worker-1", 8, claimDigest, false},
		"other digest": {"worker-1", 7, bytes.Repeat([]byte{4}, 32), false},
	} {
		t.Run(name, func(t *testing.T) {
			store, client, collection := newMongoStoreTest(t)
			seedHero(t, collection, 4)
			seedClaim(t, client, "saga-step/cmd-1", "worker-1", 7, claimDigest, time.Now().UTC().Add(time.Minute))
			record := testMutationRecord(coredata.MutationPatch)
			record.Receipts = append(record.Receipts, leaseFenceReceipt(t, "saga-step/cmd-1", tc.owner, tc.token, tc.digest))
			if err := store.Project(context.Background(), record); err != nil {
				t.Fatal(err)
			}
			doc, _ := collection.Lookup(int64(7))
			applied := doc["_version"] == int64(5)
			if applied != tc.apply {
				t.Fatalf("applied=%v want=%v (document=%v)", applied, tc.apply, doc)
			}
			// A lease fence is a precondition, never a business receipt.
			if receipts := client.Collection(testDatabase, ReceiptCollection).Len(); receipts != 0 {
				t.Fatalf("lease fence leaked into receipt collection (%d docs)", receipts)
			}
		})
	}
}

func TestMongoStorePatchExactVersionAndReplay(t *testing.T) {
	store, client, collection := newMongoStoreTest(t)
	seedHero(t, collection, 4)
	record := testMutationRecord(coredata.MutationPatch)
	if err := store.Project(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if client.Sessions() != 0 {
		t.Fatalf("single mutation fast path started %d sessions", client.Sessions())
	}
	doc, _ := collection.Lookup(int64(7))
	if doc["_version"] != int64(5) || doc["level"] != int32(5) || doc["_last_tx"] != record.ID.String() {
		t.Fatalf("projected document=%v", doc)
	}
	// Replay: the exact-version predicate no longer matches, and the stored
	// document proves this transaction already applied, so it is idempotent.
	if err := store.Project(context.Background(), record); err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if doc, _ := collection.Lookup(int64(7)); doc["_version"] != int64(5) {
		t.Fatalf("replay changed the document: %v", doc)
	}
}

func TestMongoStorePatchRejectsWrongOrMissingBase(t *testing.T) {
	t.Run("wrong base version", func(t *testing.T) {
		store, _, collection := newMongoStoreTest(t)
		// Stored at 3 while the record expects 4 and was written by another tx.
		if err := collection.Seed(bson.M{"_id": int64(7), "_version": int64(3), "_last_tx": "other"}); err != nil {
			t.Fatal(err)
		}
		if err := store.Project(context.Background(), testMutationRecord(coredata.MutationPatch)); !errors.Is(err, ErrProjectionConflict) {
			t.Fatalf("err=%v, want ErrProjectionConflict", err)
		}
	})
	t.Run("missing base document", func(t *testing.T) {
		store, _, _ := newMongoStoreTest(t)
		if err := store.Project(context.Background(), testMutationRecord(coredata.MutationPatch)); !errors.Is(err, ErrProjectionConflict) {
			t.Fatalf("err=%v, want ErrProjectionConflict", err)
		}
	})
}

func TestMongoStorePutUpsertsWhenNoBaseVersionExpected(t *testing.T) {
	store, _, collection := newMongoStoreTest(t)
	record := testMutationRecord(coredata.MutationPut)
	record.Mutations[0].ExpectedVersion = 0
	record.Mutations[0].NextVersion = 1
	if err := store.Project(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	doc, ok := collection.Lookup(int64(7))
	if !ok || doc["_version"] != int64(1) || doc["level"] != int32(5) {
		t.Fatalf("upserted document=%v ok=%v", doc, ok)
	}
}

func batchRecord(t *testing.T, id byte, docID int64, expected, next uint64) coredata.CommitRecord {
	t.Helper()
	record := testMutationRecord(coredata.MutationPatch)
	record.ID[15] = id
	record.Mutations[0].Key.ID = docID
	record.Mutations[0].ExpectedVersion = expected
	record.Mutations[0].NextVersion = next
	return record
}

func TestMongoStoreProjectBatchUsesOneTransactionAndDurableMarkers(t *testing.T) {
	store, client, collection := newMongoStoreTest(t)
	if err := collection.Seed(bson.M{"_id": int64(7), "_version": int64(4)}); err != nil {
		t.Fatal(err)
	}
	if err := collection.Seed(bson.M{"_id": int64(8), "_version": int64(5)}); err != nil {
		t.Fatal(err)
	}
	first := batchRecord(t, 9, 7, 4, 5)
	second := batchRecord(t, 10, 8, 5, 6)

	if err := store.ProjectBatch(context.Background(), []coredata.CommitRecord{first, second}); err != nil {
		t.Fatal(err)
	}
	if client.Sessions() != 1 {
		t.Fatalf("sessions=%d, want 1", client.Sessions())
	}
	firstDoc, _ := collection.Lookup(int64(7))
	secondDoc, _ := collection.Lookup(int64(8))
	if firstDoc["_version"] != int64(5) || secondDoc["_version"] != int64(6) {
		t.Fatalf("batch documents=%v %v", firstDoc, secondDoc)
	}
	if markers := markerCollection(client).Len(); markers != 2 {
		t.Fatalf("transaction markers=%d, want 2", markers)
	}
}

func TestMongoStoreProjectBatchRollsBackOnCASMismatch(t *testing.T) {
	store, _, collection := newMongoStoreTest(t)
	if err := collection.Seed(bson.M{"_id": int64(7), "_version": int64(4)}); err != nil {
		t.Fatal(err)
	}
	// The second record's expected version does not match storage.
	if err := collection.Seed(bson.M{"_id": int64(8), "_version": int64(5)}); err != nil {
		t.Fatal(err)
	}
	first := batchRecord(t, 9, 7, 4, 5)
	second := batchRecord(t, 10, 8, 1, 2)

	// A batch cannot tell an already-applied replay from a real conflict, so
	// it defers rather than declaring a verdict. The projector then re-projects
	// per record, and the single-record path classifies it (see
	// TestMongoStoreProjectBatchDefersThenSingleRecordClassifies).
	err := store.ProjectBatch(context.Background(), []coredata.CommitRecord{first, second})
	if !errors.Is(err, ErrProjectionBatchNeedsPerRecord) {
		t.Fatalf("err=%v, want ErrProjectionBatchNeedsPerRecord", err)
	}
	if errors.Is(err, ErrProjectionConflict) {
		t.Fatalf("deferral must not be a conflict verdict: %v", err)
	}
	// Nothing may be applied: the deferring batch is all-or-nothing.
	if doc, _ := collection.Lookup(int64(7)); doc["_version"] != int64(4) {
		t.Fatalf("deferred batch applied a partial write: %v", doc)
	}
}

// Once the batch defers, the single-record path must reach the right verdict
// for each record: idempotent for an already-applied replay, conflict for a
// genuinely stale base version.
func TestMongoStoreProjectBatchDefersThenSingleRecordClassifies(t *testing.T) {
	store, _, collection := newMongoStoreTest(t)
	if err := collection.Seed(bson.M{"_id": int64(7), "_version": int64(4)}); err != nil {
		t.Fatal(err)
	}
	if err := collection.Seed(bson.M{"_id": int64(8), "_version": int64(5), "_last_tx": "other"}); err != nil {
		t.Fatal(err)
	}
	applicable := batchRecord(t, 9, 7, 4, 5)
	stale := batchRecord(t, 10, 8, 1, 2)
	if err := store.ProjectBatch(context.Background(), []coredata.CommitRecord{applicable, stale}); !errors.Is(err, ErrProjectionBatchNeedsPerRecord) {
		t.Fatalf("err=%v, want a deferral", err)
	}
	if err := store.Project(context.Background(), applicable); err != nil {
		t.Fatalf("applicable record per-record: %v", err)
	}
	if err := store.Project(context.Background(), stale); !errors.Is(err, ErrProjectionConflict) {
		t.Fatalf("stale record per-record err=%v, want ErrProjectionConflict", err)
	}
}

func TestMongoStoreProjectBatchSkipsRecordsWithDurableMarker(t *testing.T) {
	store, client, collection := newMongoStoreTest(t)
	if err := collection.Seed(bson.M{"_id": int64(7), "_version": int64(4)}); err != nil {
		t.Fatal(err)
	}
	record := batchRecord(t, 9, 7, 4, 5)
	digest, err := digestRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := markerCollection(client).Seed(transactionDocument{ID: record.ID.String(), Digest: digest, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	// Already applied and marked: the batch must be a no-op, not a conflict.
	if err := store.ProjectBatch(context.Background(), []coredata.CommitRecord{record}); err != nil {
		t.Fatalf("marked record replay: %v", err)
	}
	if doc, _ := collection.Lookup(int64(7)); doc["_version"] != int64(4) {
		t.Fatalf("marked record was re-applied: %v", doc)
	}
}

func TestMongoStoreProjectBatchRejectsMarkerDigestMismatch(t *testing.T) {
	store, client, collection := newMongoStoreTest(t)
	if err := collection.Seed(bson.M{"_id": int64(7), "_version": int64(4)}); err != nil {
		t.Fatal(err)
	}
	record := batchRecord(t, 9, 7, 4, 5)
	if err := markerCollection(client).Seed(transactionDocument{
		ID: record.ID.String(), Digest: []byte("different"), CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ProjectBatch(context.Background(), []coredata.CommitRecord{record}); !errors.Is(err, ErrTransactionIdentity) {
		t.Fatalf("err=%v, want ErrTransactionIdentity", err)
	}
}

// A batch-eligible record projected alone goes through the single-record fast
// path, which deliberately writes no transaction marker (that saved round trip
// is the point of the fast path). If the WAL acknowledgement is then lost — a
// crash between the Mongo commit and the checkpoint fsync, or any Ack error —
// the record replays inside a multi-record batch, where the missing marker and
// a CAS predicate that can no longer match are indistinguishable from a real
// conflict. The batch must therefore defer, and the per-record retry must
// absorb it. Declaring a conflict here used to fence the projector
// permanently: the same batch replayed on every restart.
func TestMongoStoreProjectBatchAbsorbsFastPathProjection(t *testing.T) {
	store, _, collection := newMongoStoreTest(t)
	if err := collection.Seed(bson.M{"_id": int64(7), "_version": int64(4)}); err != nil {
		t.Fatal(err)
	}
	if err := collection.Seed(bson.M{"_id": int64(8), "_version": int64(5)}); err != nil {
		t.Fatal(err)
	}
	first := batchRecord(t, 9, 7, 4, 5)
	second := batchRecord(t, 10, 8, 5, 6)
	// Live projection of `first` alone: fast path, no marker.
	if err := store.Project(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	// Crash before ACK, restart, replay batches it with its neighbour. The
	// batch defers instead of fencing the process.
	err := store.ProjectBatch(context.Background(), []coredata.CommitRecord{first, second})
	if !errors.Is(err, ErrProjectionBatchNeedsPerRecord) {
		t.Fatalf("replay of a fast-path-projected record: err=%v, want a deferral", err)
	}
	if errors.Is(err, ErrProjectionConflict) || errors.Is(err, ErrTransactionIdentity) {
		t.Fatalf("benign replay was reported as a fatal verdict: %v", err)
	}
	// The per-record retry the projector performs must then converge: the
	// already-applied record is idempotent, its neighbour applies.
	if err := store.Project(context.Background(), first); err != nil {
		t.Fatalf("already-applied record must be idempotent: %v", err)
	}
	if err := store.Project(context.Background(), second); err != nil {
		t.Fatalf("neighbour must apply: %v", err)
	}
	firstDoc, _ := collection.Lookup(int64(7))
	secondDoc, _ := collection.Lookup(int64(8))
	if firstDoc["_version"] != int64(5) || secondDoc["_version"] != int64(6) {
		t.Fatalf("documents after replay=%v %v", firstDoc, secondDoc)
	}
}

func TestMongoStoreProjectBatchValidatesBeforeCheckingEligibility(t *testing.T) {
	store, client, collection := newMongoStoreTest(t)
	record := testMutationRecord(coredata.MutationPatch)
	record.Mutations[0].Key.Resource = ""
	record.Effects = []coredata.Effect{{ID: "special", Topic: "special"}}
	if err := coredata.ValidateCommitRecord(record); err == nil {
		t.Fatal("test record unexpectedly passed validation")
	}

	err := store.ProjectBatch(context.Background(), []coredata.CommitRecord{record})
	if err == nil || errors.Is(err, errProjectionBatchUnsupported) {
		t.Fatalf("err=%v, want validation error before unsupported classification", err)
	}
	if client.Sessions() != 0 || collection.Calls["BulkWrite"] != 0 {
		t.Fatalf("validation failure started sessions=%d bulk writes=%d", client.Sessions(), collection.Calls["BulkWrite"])
	}
}

func TestMongoStoreProjectBatchRejectsUnsupportedBeforeSessionOrWrites(t *testing.T) {
	store, client, collection := newMongoStoreTest(t)
	record := testMutationRecord(coredata.MutationPatch)
	record.Effects = []coredata.Effect{{ID: "special", Topic: "special"}}

	err := store.ProjectBatch(context.Background(), []coredata.CommitRecord{record})
	if !errors.Is(err, errProjectionBatchUnsupported) {
		t.Fatalf("err=%v, want errProjectionBatchUnsupported", err)
	}
	if client.Sessions() != 0 || collection.Calls["BulkWrite"] != 0 || markerCollection(client).Len() != 0 {
		t.Fatalf("unsupported batch started sessions=%d bulk writes=%d markers=%d",
			client.Sessions(), collection.Calls["BulkWrite"], markerCollection(client).Len())
	}
}

func TestMongoStoreDeleteWritesVersionedTombstone(t *testing.T) {
	store, _, collection := newMongoStoreTest(t)
	seedHero(t, collection, 4)
	if err := store.Project(context.Background(), testMutationRecord(coredata.MutationDelete)); err != nil {
		t.Fatal(err)
	}
	doc, ok := collection.Lookup(int64(7))
	if !ok {
		t.Fatal("delete physically removed the document instead of writing a tombstone")
	}
	if doc["_deleted"] != true || doc["_version"] != int64(5) || doc["_deleted_at"] == nil {
		t.Fatalf("tombstone=%v", doc)
	}
}

func TestMongoStoreHandlerCannotSuppressProjectionConflict(t *testing.T) {
	store, _, collection := newMongoStoreTest(t)
	// A concurrent writer already moved the document past this migration.
	if err := collection.Seed(bson.M{"_id": int64(7), "_version": int64(12), "_last_tx": "another-transaction"}); err != nil {
		t.Fatal(err)
	}
	record := testMutationRecord(coredata.MutationPut)
	record.Handler = "__dataengine_migration"
	if err := store.Project(context.Background(), record); !errors.Is(err, ErrProjectionConflict) {
		t.Fatalf("handler must not suppress projection conflict: %v", err)
	}
	if doc, _ := collection.Lookup(int64(7)); doc["_version"] != int64(12) {
		t.Fatalf("obsolete migration overwrote newer state: %v", doc)
	}
}

func TestMongoStoreProjectsRemoteAndOrdinaryMutationInOneSession(t *testing.T) {
	// 托管、无 builder 的 kind。原用 241，与 entity_repository_promises_test.go 的本地 builder dataEngineDuplicateDAOKind 撞号，
	// RR-20260926-71 起注册表拒绝把已有本地 builder 的 kind 升级为 managed。
	const kind entity.EntityKind = 235
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	entityID, err := entity.BuildEntityID(77, kind)
	if err != nil {
		t.Fatal(err)
	}
	store, client, _ := newMongoStoreTest(t)
	remoteProjection := &mongoRemoteProjectionFake{}
	if err := store.SetRemoteProjection(remoteProjection, remoteProjection); err != nil {
		t.Fatal(err)
	}
	var txID coredata.TransactionID
	txID[15] = 44
	remote := entity.RemoteCommit{
		TransactionID: entity.RemoteTransactionID(txID), EntityID: entityID, Kind: kind,
		BaseVersion: 1, NextVersion: 2, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Codec: 1,
		Mutations: []entity.RemoteDataMutation{{Collection: "heroes", ID: entityID, Version: 2, Mask: 1, Data: []byte("remote")}},
	}
	ordinary, _ := bson.Marshal(bson.M{"_id": int64(5), "value": "ordinary"})
	record := coredata.CommitRecord{ID: txID, Mutations: []coredata.Mutation{
		{Key: coredata.DocumentKey{Resource: "heroes", ID: entityID}, Kind: coredata.MutationPut, ExpectedVersion: 1, NextVersion: 2, Remote: &remote},
		{Key: coredata.DocumentKey{Database: testDatabase, Resource: "mail", ID: 5}, Kind: coredata.MutationPut, ExpectedVersion: 0, NextVersion: 1, Data: ordinary},
	}}
	if err := store.Project(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if client.Sessions() != 1 || remoteProjection.stored != 1 || remoteProjection.applied != 1 {
		t.Fatalf("sessions=%d stored=%d applied=%d", client.Sessions(), remoteProjection.stored, remoteProjection.applied)
	}
	if mail, ok := client.Collection(testDatabase, "mail").Lookup(int64(5)); !ok || mail["_version"] != int64(1) {
		t.Fatalf("ordinary mutation in the same session=%v ok=%v", mail, ok)
	}
}

func TestMongoStoreOlderPutCannotReviveTombstone(t *testing.T) {
	store, _, collection := newMongoStoreTest(t)
	if err := collection.Seed(bson.M{
		"_id": int64(7), "_version": int64(6), "_deleted": true, "_last_tx": "other",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Project(context.Background(), testMutationRecord(coredata.MutationPut)); !errors.Is(err, ErrProjectionConflict) {
		t.Fatalf("err=%v, want ErrProjectionConflict", err)
	}
	if doc, _ := collection.Lookup(int64(7)); doc["_deleted"] != true {
		t.Fatalf("stale put revived the tombstone: %v", doc)
	}
}

func TestMongoStoreNewerPutRevivesTombstoneAtHigherVersion(t *testing.T) {
	store, _, collection := newMongoStoreTest(t)
	if err := collection.Seed(bson.M{
		"_id": int64(7), "_version": int64(4), "_deleted": true, "_deleted_at": time.Now().UTC(), "_last_tx": "delete-tx",
	}); err != nil {
		t.Fatal(err)
	}
	// An explicit, strictly-higher Put is the documented resurrection path.
	if err := store.Project(context.Background(), testMutationRecord(coredata.MutationPut)); err != nil {
		t.Fatal(err)
	}
	doc, _ := collection.Lookup(int64(7))
	if doc["_version"] != int64(5) || doc["_deleted"] != nil || doc["_deleted_at"] != nil {
		t.Fatalf("resurrected document=%v", doc)
	}
}

func TestMongoStoreMultiRecordUsesTransactionAndStagesEffectsReceipts(t *testing.T) {
	store, client, collection := newMongoStoreTest(t)
	seedHero(t, collection, 4)
	record := testMutationRecord(coredata.MutationPatch)
	record.Effects = []coredata.Effect{{ID: "effect-1", Topic: "hero.changed", Payload: []byte{1}}}
	record.Receipts = []coredata.Receipt{{Namespace: "saga-step", ID: "step-1", Digest: []byte{2}}}
	if err := store.Project(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if client.Sessions() != 1 {
		t.Fatalf("sessions=%d, want 1", client.Sessions())
	}
	if client.Collection(testDatabase, OutboxCollection).Len() != 1 ||
		client.Collection(testDatabase, ReceiptCollection).Len() != 1 ||
		markerCollection(client).Len() != 1 {
		t.Fatal("transaction did not stage effect, receipt, and transaction identity")
	}
	// Replay must be absorbed by the durable marker: no duplicate staging.
	if err := store.Project(context.Background(), record); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if client.Collection(testDatabase, OutboxCollection).Len() != 1 || client.Collection(testDatabase, ReceiptCollection).Len() != 1 {
		t.Fatal("replay staged duplicates")
	}
}

// inMongoTransaction runs a staging step the way Project does: inside one
// session transaction. The fake aborts the transaction on a write error, so a
// step that reads back after a duplicate key fails here as it does on a real
// server instead of passing outside any transaction.
func inMongoTransaction(t *testing.T, client *mongotest.Client, step func(context.Context) error) error {
	t.Helper()
	session, err := client.StartSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer session.EndSession(context.Background())
	return session.WithTransaction(context.Background(), step)
}

func TestMongoStoreReceiptIdentityIncludesCompletionPayload(t *testing.T) {
	store, client, _ := newMongoStoreTest(t)
	receipt := coredata.Receipt{Namespace: "saga-step", ID: "command-1", Digest: []byte{1}, Payload: []byte("completion")}
	stage := func(receipt coredata.Receipt) error {
		return inMongoTransaction(t, client, func(ctx context.Context) error { return store.stageReceipt(ctx, "tx-1", receipt) })
	}
	if err := stage(receipt); err != nil {
		t.Fatal(err)
	}
	// Same receipt, same payload: idempotent.
	attempts := client.Attempts()
	if err := stage(receipt); err != nil {
		t.Fatalf("identical receipt replay: %v", err)
	}
	if client.Collection(testDatabase, ReceiptCollection).Len() != 1 {
		t.Fatal("identical receipt replay inserted a duplicate")
	}
	// Same identity, different completion payload: an identity conflict.
	conflicting := receipt
	conflicting.Payload = []byte("different")
	if err := stage(conflicting); !errors.Is(err, ErrReceiptIdentity) {
		t.Fatalf("err=%v, want ErrReceiptIdentity", err)
	}
	// Each verdict is reached on the first attempt; a transient retry loop
	// is what burns transaction_timeout on a real server.
	if got := client.Attempts() - attempts; got != 2 {
		t.Fatalf("attempts=%d, want one per verdict", got)
	}
}

func TestMongoStoreStageEffectRejectsIdentityDrift(t *testing.T) {
	store, client, _ := newMongoStoreTest(t)
	effect := coredata.Effect{ID: "effect-1", Topic: "hero.changed", Payload: []byte{1}}
	stage := func(txID string) error {
		return inMongoTransaction(t, client, func(ctx context.Context) error { return store.stageEffect(ctx, txID, effect) })
	}
	if err := stage("tx-1"); err != nil {
		t.Fatal(err)
	}
	attempts := client.Attempts()
	if err := stage("tx-1"); err != nil {
		t.Fatalf("identical effect replay: %v", err)
	}
	if client.Collection(testDatabase, OutboxCollection).Len() != 1 {
		t.Fatal("identical effect replay staged a duplicate")
	}
	// The same effect id under a different transaction is a WAL identity bug.
	if err := stage("tx-2"); !errors.Is(err, ErrTransactionIdentity) {
		t.Fatalf("err=%v, want ErrTransactionIdentity", err)
	}
	if got := client.Attempts() - attempts; got != 2 {
		t.Fatalf("attempts=%d, want one per verdict", got)
	}
}

// 跨事务复用 receipt / effect 身份时，Project 在第一次尝试内裁决：相同 receipt 成功并落库，receipt 漂移与 effect ID 复用是 fatal 身份冲突
// 且整笔回滚。saga start 的 effect ID 是命令内容哈希，同一 start 在两个事务各发一次即第三种形状，保持 fatal。
// 历史：RR-20260926-34。
func TestMongoStoreProjectIdentityVerdictsAcrossTransactions(t *testing.T) {
	store, client, _ := newMongoStoreTest(t)
	record := func(idByte byte, docID int64) coredata.CommitRecord {
		var id coredata.TransactionID
		id[15] = idByte
		data, _ := bson.Marshal(bson.M{"_id": docID, "level": int32(1)})
		return coredata.CommitRecord{ID: id, Mutations: []coredata.Mutation{{
			Key:  coredata.DocumentKey{Database: testDatabase, Resource: "heroes", ID: docID},
			Kind: coredata.MutationPut, NextVersion: 1, Mask: 1, Schema: 1, Codec: "bson-v2", Data: data,
		}}}
	}
	receipt := coredata.Receipt{Namespace: "saga-step", ID: "cmd-1", Digest: []byte("d1"), Payload: []byte("p1")}
	first := record(1, 1)
	first.Receipts = []coredata.Receipt{receipt}
	first.Effects = []coredata.Effect{{ID: "effect-1", Topic: "hero.changed", Payload: []byte{1}}}
	if err := store.Project(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	heroes := client.Collection(testDatabase, "heroes")
	for _, tc := range []struct {
		name   string
		record coredata.CommitRecord
		want   error
	}{
		{name: "identical receipt", record: func() coredata.CommitRecord {
			r := record(2, 2)
			r.Receipts = []coredata.Receipt{receipt}
			return r
		}()},
		{name: "receipt payload drift", want: ErrReceiptIdentity, record: func() coredata.CommitRecord {
			r := record(3, 3)
			r.Receipts = []coredata.Receipt{{Namespace: "saga-step", ID: "cmd-1", Digest: []byte("d1"), Payload: []byte("other")}}
			return r
		}()},
		{name: "effect id reused", want: ErrTransactionIdentity, record: func() coredata.CommitRecord {
			r := record(4, 4)
			r.Effects = []coredata.Effect{{ID: "effect-1", Topic: "hero.changed", Payload: []byte{1}}}
			return r
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempts := client.Attempts()
			err := store.Project(context.Background(), tc.record)
			if got := client.Attempts() - attempts; got != 1 {
				t.Fatalf("attempts=%d err=%v, want the verdict on the first attempt", got, err)
			}
			_, written := heroes.Lookup(tc.record.Mutations[0].Key.ID)
			fatal := (&Projector{}).isFatalProjection(err)
			if tc.want == nil {
				if err != nil || !written || fatal {
					t.Fatalf("err=%v written=%v fatal=%v, want success", err, written, fatal)
				}
				return
			}
			if !errors.Is(err, tc.want) || !fatal || written {
				t.Fatalf("err=%v written=%v fatal=%v, want fatal %v and a rolled-back DAO", err, written, fatal, tc.want)
			}
		})
	}
}

var _ fmongo.IMongo = (*mongotest.Client)(nil)

func TestMongoBatchRejectsConflictingDuplicateIDBeforeMarkerLookup(t *testing.T) {
	store, client, _ := newMongoStoreTest(t)
	first := batchRecord(t, 9, 7, 4, 5)
	second := coredata.CloneCommitRecord(first)
	second.Handler = "different"
	digest, err := digestRecord(second)
	if err != nil {
		t.Fatal(err)
	}
	if err = markerCollection(client).Seed(transactionDocument{ID: second.ID.String(), Digest: digest}); err != nil {
		t.Fatal(err)
	}
	if err = store.ProjectBatch(context.Background(), []coredata.CommitRecord{first, second}); !errors.Is(err, ErrTransactionIdentity) {
		t.Fatalf("conflicting batch identity=%v", err)
	}
}

func TestMongoStoreLoadCopiesVersionSchemaAndRemoteEnvelope(t *testing.T) {
	store, client, collection := newMongoStoreTest(t)
	ordinary, err := bson.Marshal(bson.M{"_id": int64(7), "_version": uint64(9), "_schema": uint32(3), "name": "hero"})
	if err != nil {
		t.Fatal(err)
	}
	if err := collection.Seed(bson.M{"_id": int64(7), "_version": uint64(9), "_schema": uint32(3), "name": "hero", "zone": int32(3)}); err != nil {
		t.Fatal(err)
	}
	if err := collection.Seed(bson.M{
		"_id": int64(8), "_ver": uint64(11), "_marker_epoch": uint64(4), "_lock_fence": uint64(5), "_route_epoch": uint64(6),
		"data": ordinary, "zone": int32(3),
	}); err != nil {
		t.Fatal(err)
	}
	docs, err := store.Load(context.Background(), coredata.LoadSpec{Database: testDatabase, Resource: "heroes", Filter: map[string]any{"zone": int32(3)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 || docs[0].Key.ID != 7 || docs[0].Version != 9 || docs[0].Schema != 3 {
		t.Fatalf("ordinary=%+v", docs)
	}
	if !docs[1].Enveloped || docs[1].Version != 11 || docs[1].MarkerEpoch != 4 || docs[1].LockFence != 5 || docs[1].RouteEpoch != 6 {
		t.Fatalf("remote=%+v", docs[1])
	}
	if client.Sessions() != 0 {
		t.Fatalf("load unexpectedly started session")
	}
}

// The tombstone exclusion is a load-path invariant, so assert it by storage
// behaviour: a deleted document must not come back, and a caller filter must
// still be honoured alongside it.
func TestMongoStoreLoadExcludesTombstonesAndHonoursCallerFilter(t *testing.T) {
	store, _, collection := newMongoStoreTest(t)
	for _, doc := range []bson.M{
		{"_id": int64(1), "_version": uint64(1), "zone": int32(3)},
		{"_id": int64(2), "_version": uint64(2), "zone": int32(3), "_deleted": true},
		{"_id": int64(3), "_version": uint64(1), "zone": int32(9)},
	} {
		if err := collection.Seed(doc); err != nil {
			t.Fatal(err)
		}
	}
	docs, err := store.Load(context.Background(), coredata.LoadSpec{
		Database: testDatabase, Resource: "heroes", Filter: map[string]any{"zone": int32(3)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Key.ID != 1 {
		t.Fatalf("loaded=%+v, want only the live zone-3 document", docs)
	}
	// Without a caller filter the tombstone must still be excluded.
	docs, err = store.Load(context.Background(), coredata.LoadSpec{Database: testDatabase, Resource: "heroes"})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Fatalf("loaded=%d documents, want 2 live ones", len(docs))
	}
	for _, doc := range docs {
		if doc.Deleted || doc.Key.ID == 2 {
			t.Fatalf("tombstone leaked into load: %+v", doc)
		}
	}
	// 聚合仓库的单实体读显式要墓碑（IncludeDeleted）：删过档与从未创建由此区分（D10）。
	docs, err = store.Load(context.Background(), coredata.LoadSpec{
		Database: testDatabase, Resource: "heroes", Filter: map[string]any{"_id": int64(2)}, IncludeDeleted: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Key.ID != 2 || !docs[0].Deleted || docs[0].Version != 2 {
		t.Fatalf("loaded=%+v, want the tombstone with its version", docs)
	}
}

func TestMongoStoreStreamLoadUsesCursorAndStopsOnConsumerError(t *testing.T) {
	store, _, collection := newMongoStoreTest(t)
	for id := int64(1); id <= 3; id++ {
		if err := collection.Seed(bson.M{"_id": id, "_version": uint64(1)}); err != nil {
			t.Fatal(err)
		}
	}
	boom := errors.New("consumer stop")
	seen := 0
	err := store.StreamLoad(context.Background(), coredata.LoadSpec{Database: testDatabase, Resource: "heroes"},
		func(coredata.RawDocument) error {
			seen++
			if seen == 2 {
				return boom
			}
			return nil
		})
	if !errors.Is(err, boom) || seen != 2 {
		t.Fatalf("err=%v seen=%d, want the consumer error to stop the stream", err, seen)
	}
	if collection.Calls["StreamFind"] == 0 {
		t.Fatal("StreamLoad did not use the streaming cursor path")
	}
}

func TestMongoStoreReadConsistentUsesOneSession(t *testing.T) {
	store, client, _ := newMongoStoreTest(t)
	called := false
	if err := store.ReadConsistent(context.Background(), func(context.Context) error { called = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !called || client.Sessions() != 1 {
		t.Fatalf("called=%v sessions=%d", called, client.Sessions())
	}
}

// ISession.WithTransaction is documented as retrying automatically, so every
// ReadConsistent callback must be idempotent across invocations.
func TestMongoStoreReadConsistentRetriesCallback(t *testing.T) {
	store, client, _ := newMongoStoreTest(t)
	client.TransientRetries = 1
	calls := 0
	if err := store.ReadConsistent(context.Background(), func(context.Context) error {
		calls++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("callback invocations=%d, want 2 (one transient retry)", calls)
	}
}

// 投影事务里 effect 先按 _id 做快照读，未命中才 InsertOne；两步之间并发写者插入同 _id 时 InsertOne 得到 ErrDuplicateKey：这不是身份冲突、
// 不 fatal——错误保留 errors.Is(fmongo.ErrDuplicateKey)、不带 ErrTransactionIdentity / ErrProjectionConflict / ErrReceiptIdentity，整笔事务
// 回滚（mutation、transaction marker 都不落库），由重试时的快照读裁决：同一事务身份视为已暂存，不同事务才是 ErrTransactionIdentity。
func TestStageEffectDuplicateAfterSnapshotMissIsRetryable(t *testing.T) {
	for _, tc := range []struct {
		name       string
		concurrent string // 并发写者暂存的 effect 属于哪个事务
		retry      func(t *testing.T, err error)
	}{
		{name: "same transaction identity", retry: func(t *testing.T, err error) {
			if err != nil {
				t.Fatalf("retry after the concurrent stage of the same effect = %v, want idempotent success", err)
			}
		}},
		{name: "other transaction", concurrent: "other-transaction", retry: func(t *testing.T, err error) {
			if !errors.Is(err, ErrTransactionIdentity) {
				t.Fatalf("retry after another transaction staged the same effect id = %v, want ErrTransactionIdentity", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := mongotest.NewClient()
			racing := &outboxInsertRace{Client: client}
			store, err := NewMongoStore(racing, MongoStoreConfig{Databases: identityDatabases(t), DefaultDatabase: testDatabase, ServerID: 3})
			if err != nil {
				t.Fatal(err)
			}
			record := projectorRecord(1, true)
			record.Mutations[0].Key.Database = testDatabase
			txID := record.ID.String()
			owner := txID
			if tc.concurrent != "" {
				owner = tc.concurrent
			}
			concurrentDoc := bson.M{"_id": record.Effects[0].ID, "effect_id": record.Effects[0].ID, "transaction_id": owner, "topic": record.Effects[0].Topic}
			racing.inject.Store(&concurrentDoc)

			err = store.Project(context.Background(), record)
			if !errors.Is(err, fmongo.ErrDuplicateKey) {
				t.Fatalf("Project = %v, want the duplicate key from the racing insert", err)
			}
			if errors.Is(err, ErrTransactionIdentity) || errors.Is(err, ErrProjectionConflict) || errors.Is(err, ErrReceiptIdentity) {
				t.Fatalf("Project = %v: a duplicate after a snapshot miss must not be classified as fatal", err)
			}
			if racing.injected.Load() != 1 {
				t.Fatalf("premise: racing insert injected %d times, want 1", racing.injected.Load())
			}
			// 整笔回滚：mutation 与 marker 都没有落库。
			if _, ok := client.Collection(testDatabase, "heroes").Lookup(int64(1)); ok {
				t.Fatal("mutation of the aborted projection transaction is visible")
			}
			if markerCollection(client).Len() != 0 {
				t.Fatal("transaction marker of the aborted projection transaction is visible")
			}

			// RR-20261004-NC-29：abort 不得抹掉并发已提交文档，不再用 Seed 重建它。
			if doc, ok := client.Collection(testDatabase, OutboxCollection).Lookup(record.Effects[0].ID); !ok || doc["transaction_id"] != owner {
				t.Fatalf("concurrent commit erased by abort: %v", doc)
			}
			err = store.Project(context.Background(), record)
			tc.retry(t, err)
			if err == nil {
				if _, ok := client.Collection(testDatabase, "heroes").Lookup(int64(1)); !ok {
					t.Fatal("retry succeeded without applying the mutation")
				}
				if n := client.Collection(testDatabase, OutboxCollection).Len(); n != 1 {
					t.Fatalf("outbox holds %d documents after the idempotent retry, want 1", n)
				}
			}
		})
	}
}

// outboxInsertRace 在 outbox 集合的 FindOne 未命中之后、返回之前，写入一次事务外并发提交，
// 并明确注入随后的 InsertOne 重复键错误；两项都只注入一次。
type outboxInsertRace struct {
	*mongotest.Client
	inject    atomic.Pointer[bson.M]
	injected  atomic.Int32
	duplicate atomic.Bool
}

func (m *outboxInsertRace) Database(name string) fmongo.IDatabase {
	return racingDatabase{IDatabase: m.Client.Database(name), race: m, name: name}
}

type racingDatabase struct {
	fmongo.IDatabase
	race *outboxInsertRace
	name string
}

func (d racingDatabase) Collection(name string) fmongo.ICollection {
	coll := d.IDatabase.Collection(name)
	if name != OutboxCollection {
		return coll
	}
	return racingCollection{ICollection: coll, race: d.race, raw: d.race.Client.Collection(d.name, name)}
}

type racingCollection struct {
	fmongo.ICollection
	race *outboxInsertRace
	raw  *mongotest.Collection
}

func (c racingCollection) FindOne(ctx context.Context, filter any, result any) error {
	err := c.ICollection.FindOne(ctx, filter, result)
	if errors.Is(err, fmongo.ErrNotFound) {
		if doc := c.race.inject.Swap(nil); doc != nil {
			if seedErr := c.raw.Seed(*doc); seedErr != nil {
				return seedErr
			}
			c.race.injected.Add(1)
			c.race.duplicate.Store(true)
		}
	}
	return err
}

// 私有快照不靠看到并发 Seed 制造重复键：明确注入本测试承诺的写错误，保留真实的事务外提交，让回滚 / 重试断言独立于替身隔离实现。
func (c racingCollection) InsertOne(ctx context.Context, doc any) (string, error) {
	if c.race.duplicate.Swap(false) {
		return "", fmongo.ErrDuplicateKey
	}
	return c.ICollection.InsertOne(ctx, doc)
}
