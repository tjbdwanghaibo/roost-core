package dataengine

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"github.com/tjbdwanghaibo/roost-kit/internal/mongofake"
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

func newMongoStoreTest(t *testing.T) (*MongoStore, *mongofake.Client, *mongofake.Collection) {
	t.Helper()
	client := mongofake.NewClient()
	store, err := NewMongoStore(client, MongoStoreConfig{DefaultDatabase: testDatabase, ServerID: 3, TransactionReceiptTTL: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return store, client, client.Collection(testDatabase, "heroes")
}

func markerCollection(client *mongofake.Client) *mongofake.Collection {
	return client.Collection(testDatabase, transactionCollection)
}

// seedHero installs the document an exact-version mutation expects to find.
func seedHero(t *testing.T, coll *mongofake.Collection, version uint64) {
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
	receipts := client.Collection(testDatabase, receiptCollection)
	if len(receipts.Indexes) != 1 {
		t.Fatalf("receipt indexes=%d, want 1", len(receipts.Indexes))
	}
	index := receipts.Indexes[0]
	if !index.ExpireAt || index.TTL != 0 || !index.RecreateOnConflict {
		t.Fatalf("receipt expiry index=%+v", index)
	}
	// The outbox claim path and the effect-id uniqueness both depend on their
	// index existing; assert them rather than trusting the call went through.
	outbox := client.Collection(testDatabase, outboxCollection)
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
func seedClaim(t *testing.T, client *mongofake.Client, documentID, owner string, token uint64, digest []byte, leaseUntil time.Time) {
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

func TestMongoStoreSkippedLeaseFenceNeverPublishesRemoteCommit(t *testing.T) {
	const kind entity.EntityKind = 242
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
	if err := store.Project(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if remoteProjection.stored != 0 || remoteProjection.applied != 0 {
		t.Fatalf("stale fenced remote commit stored=%d published=%d", remoteProjection.stored, remoteProjection.applied)
	}

	// A WAL replay sees the durable skipped marker written by the first pass.
	// It must remain a no-op and must not publish outside the transaction.
	if err := store.Project(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if remoteProjection.stored != 0 || remoteProjection.applied != 0 {
		t.Fatalf("replayed skipped remote commit stored=%d published=%d", remoteProjection.stored, remoteProjection.applied)
	}
	if markers := markerCollection(client).Len(); markers != 1 {
		t.Fatalf("replay wrote %d markers, want 1", markers)
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
			if receipts := client.Collection(testDatabase, receiptCollection).Len(); receipts != 0 {
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

func TestMongoStoreMigrationConflictBecomesObsoleteNoop(t *testing.T) {
	store, _, collection := newMongoStoreTest(t)
	// A concurrent writer already moved the document past this migration.
	if err := collection.Seed(bson.M{"_id": int64(7), "_version": int64(12), "_last_tx": "another-transaction"}); err != nil {
		t.Fatal(err)
	}
	record := testMutationRecord(coredata.MutationPut)
	record.Handler = MigrationHandler
	if err := store.Project(context.Background(), record); err != nil {
		t.Fatalf("obsolete migration must be acknowledged for repository reload: %v", err)
	}
	if doc, _ := collection.Lookup(int64(7)); doc["_version"] != int64(12) {
		t.Fatalf("obsolete migration overwrote newer state: %v", doc)
	}
}

func TestMongoStoreProjectsRemoteAndOrdinaryMutationInOneSession(t *testing.T) {
	const kind entity.EntityKind = 241
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
	if client.Collection(testDatabase, outboxCollection).Len() != 1 ||
		client.Collection(testDatabase, receiptCollection).Len() != 1 ||
		markerCollection(client).Len() != 1 {
		t.Fatal("transaction did not stage effect, receipt, and transaction identity")
	}
	// Replay must be absorbed by the durable marker: no duplicate staging.
	if err := store.Project(context.Background(), record); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if client.Collection(testDatabase, outboxCollection).Len() != 1 || client.Collection(testDatabase, receiptCollection).Len() != 1 {
		t.Fatal("replay staged duplicates")
	}
}

func TestMongoStoreReceiptIdentityIncludesCompletionPayload(t *testing.T) {
	store, client, _ := newMongoStoreTest(t)
	receipt := coredata.Receipt{Namespace: "saga-step", ID: "command-1", Digest: []byte{1}, Payload: []byte("completion")}
	if err := store.stageReceipt(context.Background(), "tx-1", receipt); err != nil {
		t.Fatal(err)
	}
	// Same receipt, same payload: idempotent.
	if err := store.stageReceipt(context.Background(), "tx-1", receipt); err != nil {
		t.Fatalf("identical receipt replay: %v", err)
	}
	if client.Collection(testDatabase, receiptCollection).Len() != 1 {
		t.Fatal("identical receipt replay inserted a duplicate")
	}
	// Same identity, different completion payload: an identity conflict.
	conflicting := receipt
	conflicting.Payload = []byte("different")
	if err := store.stageReceipt(context.Background(), "tx-1", conflicting); !errors.Is(err, ErrReceiptIdentity) {
		t.Fatalf("err=%v, want ErrReceiptIdentity", err)
	}
}

func TestMongoStoreStageEffectRejectsIdentityDrift(t *testing.T) {
	store, client, _ := newMongoStoreTest(t)
	effect := coredata.Effect{ID: "effect-1", Topic: "hero.changed", Payload: []byte{1}}
	if err := store.stageEffect(context.Background(), "tx-1", effect); err != nil {
		t.Fatal(err)
	}
	if err := store.stageEffect(context.Background(), "tx-1", effect); err != nil {
		t.Fatalf("identical effect replay: %v", err)
	}
	if client.Collection(testDatabase, outboxCollection).Len() != 1 {
		t.Fatal("identical effect replay staged a duplicate")
	}
	// The same effect id under a different transaction is a WAL identity bug.
	if err := store.stageEffect(context.Background(), "tx-2", effect); !errors.Is(err, ErrTransactionIdentity) {
		t.Fatalf("err=%v, want ErrTransactionIdentity", err)
	}
}

var _ fmongo.IMongo = (*mongofake.Client)(nil)
