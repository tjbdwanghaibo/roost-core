package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func mappedDatabases(t *testing.T, configured map[string]string) *coredata.DatabaseMap {
	t.Helper()
	databases, _, err := coredata.NewDatabaseMap(coredata.DatabaseMapConfig{
		Declared: []coredata.LogicalDatabase{{Name: "main", ServerScoped: true}}, Configured: configured, ServerID: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	return databases
}

func mappedRecord(id byte, kind coredata.MutationKind, expected, next uint64) coredata.CommitRecord {
	mutation := coredata.Mutation{
		Key:  coredata.DocumentKey{Database: "main", Scope: coredata.DatabaseServer, Resource: "player", ID: 7},
		Kind: kind, ExpectedVersion: expected, NextVersion: next, Schema: 1, Codec: "bson-v2",
	}
	if kind == coredata.MutationPut {
		mutation.Data, _ = bson.Marshal(bson.M{"_id": int64(7), "level": int32(next)})
	}
	return coredata.CommitRecord{ID: coredata.TransactionID{15: id}, Durability: coredata.DurabilityStrict, Mutations: []coredata.Mutation{mutation}}
}

// 投影（单条快路与批量）、冷加载与删档墓碑都经同一张映射表：逻辑库 main → 物理库 planet_x，按 sid 分库再加 _<sid>；
// 名为 main 的库里什么也没有。
func TestMongoStoreResolvesLogicalDatabasesOnEveryDocumentPath(t *testing.T) {
	client := mongotest.NewClient()
	store, err := NewMongoStore(client, MongoStoreConfig{DefaultDatabase: "de_system", Databases: mappedDatabases(t, map[string]string{"main": "planet_x"}), ServerID: 3, TransactionReceiptTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Project(ctx, mappedRecord(1, coredata.MutationPut, 0, 1)); err != nil {
		t.Fatal(err)
	}
	if err := store.ProjectBatch(ctx, []coredata.CommitRecord{mappedRecord(2, coredata.MutationPut, 1, 2), mappedRecord(3, coredata.MutationPut, 2, 3)}); err != nil {
		t.Fatal(err)
	}
	physical := client.Collection("planet_x_3", "player")
	if doc, ok := physical.Lookup(int64(7)); !ok || doc["_version"] != int64(3) {
		t.Fatalf("planet_x_3.player = %v (found %v); want version 3 from the single and batch projections", doc, ok)
	}
	docs, err := store.Load(ctx, coredata.LoadSpec{Database: "main", Scope: coredata.DatabaseServer, Resource: "player"})
	if err != nil || len(docs) != 1 || docs[0].Version != 3 || docs[0].Key.Database != "main" {
		t.Fatalf("cold load through logical main = %+v, %v; want the planet_x_3 document keyed by the logical name", docs, err)
	}
	if err := store.Project(ctx, mappedRecord(4, coredata.MutationDelete, 3, 4)); err != nil {
		t.Fatal(err)
	}
	if doc, _ := physical.Lookup(int64(7)); doc["_deleted"] != true {
		t.Fatalf("tombstone = %v; want _deleted in planet_x_3", doc)
	}
	tombstones, err := store.Load(ctx, coredata.LoadSpec{Database: "main", Scope: coredata.DatabaseServer, Resource: "player", IncludeDeleted: true})
	if err != nil || len(tombstones) != 1 || !tombstones[0].Deleted {
		t.Fatalf("tombstone read through logical main = %+v, %v", tombstones, err)
	}
	for _, name := range []string{"main", "main_3", "planet_x"} {
		if n := client.Collection(name, "player").Len(); n != 0 {
			t.Fatalf("%s.player holds %d documents; nothing may land outside the mapped physical database", name, n)
		}
	}
}

func openMappingWAL(t *testing.T, dir string) *nestwal.WAL {
	t.Helper()
	wal, err := nestwal.Open(nestwal.DefaultOptions(dir))
	if err != nil {
		t.Fatal(err)
	}
	return wal
}

// WAL 只记逻辑名；上次启动的映射记在 WAL 目录。还有未投影记录时改映射拒绝启动，排空后可以改。
func TestReconcileDatabaseMappingRefusesAChangeWhileRecordsAreUnprojected(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	original := mappedDatabases(t, map[string]string{"main": "planet_a"})
	moved := mappedDatabases(t, map[string]string{"main": "planet_b"})

	wal := openMappingWAL(t, dir)
	if err := reconcileDatabaseMapping(ctx, wal, original); err != nil {
		t.Fatalf("first start on an empty WAL: %v", err)
	}
	fence, err := wal.Append(ctx, mappedRecord(1, coredata.MutationPut, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := wal.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// 崩溃后带着新映射重启：记录还没投影，拒绝，并且记下的映射不变。
	wal = openMappingWAL(t, dir)
	err = reconcileDatabaseMapping(ctx, wal, moved)
	if !errors.Is(err, ErrDatabaseMappingChanged) || !strings.Contains(err.Error(), "main: planet_a -> planet_b") {
		t.Fatalf("restart with a changed mapping over an unprojected record = %v; want ErrDatabaseMappingChanged naming main", err)
	}
	if recorded, _, _ := nestwal.ReadDatabaseMapping(dir); recorded["main"] != "planet_a" {
		t.Fatalf("a refused start rewrote the recorded mapping: %v", recorded)
	}
	// 同一映射照常启动（崩溃恢复不受影响）。
	if err := reconcileDatabaseMapping(ctx, wal, original); err != nil {
		t.Fatalf("restart with the same mapping: %v", err)
	}
	// 排空（确认投影）后改映射：允许，并记下新映射。
	if err := wal.Ack(ctx, fence); err != nil {
		t.Fatal(err)
	}
	if err := reconcileDatabaseMapping(ctx, wal, moved); err != nil {
		t.Fatalf("changed mapping over a drained WAL: %v", err)
	}
	if recorded, found, err := nestwal.ReadDatabaseMapping(dir); err != nil || !found || recorded["main"] != "planet_b" {
		t.Fatalf("recorded mapping after the change = %v, %v, %v", recorded, found, err)
	}
	if err := wal.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

// 没有映射文件的 WAL（引入映射之前写下的）按“物理名就是逻辑名”写入：有未投影记录时，非 identity 的映射视为改变。
func TestReconcileDatabaseMappingTreatsAMissingRecordAsIdentity(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	wal := openMappingWAL(t, dir)
	if _, err := wal.Append(ctx, mappedRecord(1, coredata.MutationPut, 0, 1)); err != nil {
		t.Fatal(err)
	}
	if err := reconcileDatabaseMapping(ctx, wal, mappedDatabases(t, map[string]string{"main": "planet_a"})); !errors.Is(err, ErrDatabaseMappingChanged) {
		t.Fatalf("mapped start over an unrecorded WAL with pending records = %v; want ErrDatabaseMappingChanged", err)
	}
	if err := reconcileDatabaseMapping(ctx, wal, mappedDatabases(t, nil)); err != nil {
		t.Fatalf("identity start over an unrecorded WAL: %v", err)
	}
	if err := wal.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
