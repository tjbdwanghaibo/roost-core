//go:build integration

package dataengine

import (
	"encoding/binary"
	"errors"
	"strconv"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/cube-core/dataengine"
	corenest "github.com/tjbdwanghaibo/cube-core/nest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

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
