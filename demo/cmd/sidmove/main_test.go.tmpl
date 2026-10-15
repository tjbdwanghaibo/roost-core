package main

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func walRecord(sequence byte) corenest.CommitRecord {
	var id corenest.TransactionID
	id[len(id)-1] = sequence
	return corenest.CommitRecord{
		ID: id, Handler: "sidmove.test", RequestID: "request", CreatedAt: 1,
		Durability: corenest.DurabilityStrict.Record(),
		Mutations: []corenest.EntityMutation{{
			Key:  dataengine.DocumentKey{ID: int64(sequence), Database: "game", Resource: "player"},
			Kind: dataengine.MutationPut, ExpectedVersion: uint64(sequence) - 1, NextVersion: uint64(sequence), Schema: 2, Codec: "bson", Data: []byte{sequence},
		}},
	}
}

// 搬机前的 WAL 检查：进程持有时拒绝；有未确认的尾部时不通过；全部确认后通过。
func TestWALDrainedOnlyWhenEveryRecordIsAcknowledged(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	wal, err := nestwal.Open(nestwal.DefaultOptions(dir))
	if err != nil {
		t.Fatal(err)
	}
	first, err := wal.Append(ctx, walRecord(1))
	if err != nil {
		t.Fatal(err)
	}
	second, err := wal.Append(ctx, walRecord(2))
	if err != nil {
		t.Fatal(err)
	}
	if err := wal.Ack(ctx, first); err != nil {
		t.Fatal(err)
	}
	if report, err := walDrained(ctx, dir); err == nil || report.Drained {
		t.Fatalf("a WAL held by a running writer passed: %+v, %v", report, err)
	}
	if err := wal.Close(ctx); err != nil {
		t.Fatal(err)
	}
	report, err := walDrained(ctx, dir)
	if !errors.Is(err, ErrNotDrained) || report.Drained || report.Records != 2 {
		t.Fatalf("an unacknowledged tail passed: %+v, %v", report, err)
	}

	wal, err = nestwal.Open(nestwal.DefaultOptions(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := wal.Ack(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := wal.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if report, err := walDrained(ctx, dir); err != nil || !report.Drained {
		t.Fatalf("a fully acknowledged WAL did not pass: %+v, %v", report, err)
	}
}

// 锁值 token|hostname|pid|started_unix_ms：报告持有者，不报告令牌。
func TestParseHolderDropsTheToken(t *testing.T) {
	hostname, pid, started := parseHolder("secret-token|host-a|4242|1760000000000")
	if hostname != "host-a" || pid != "4242" || started != "2025-10-09T08:53:20Z" {
		t.Fatalf("parseHolder = %q %q %q", hostname, pid, started)
	}
	if hostname, _, _ := parseHolder("not a holder"); hostname != "" {
		t.Fatal("a malformed value was parsed")
	}
}

// 客户端看到的唯一 id 与文档 _id 都接受，统一成文档 _id；非正数拒绝。
func TestPlayerDocumentIDsAcceptsBothSpellings(t *testing.T) {
	ids, err := playerDocumentIDs("2001")
	if err != nil || len(ids) != 1 || ids[0] == 2001 {
		t.Fatalf("unique id 2001 -> %v, %v; want the document _id", ids, err)
	}
	again, err := playerDocumentIDs(" 2001 , " + strconv.FormatInt(ids[0], 10))
	if err != nil || len(again) != 2 || again[0] != ids[0] || again[1] != ids[0] {
		t.Fatalf("both spellings -> %v, %v; want the same _id twice", again, err)
	}
	for _, bad := range []string{"", "0", "-3", "x"} {
		if _, err := playerDocumentIDs(bad); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}
}

// 两次快照之间任何一个玩家文档变了（版本、事务或内容），比较都不通过。
func TestPlayerSnapshotsDetectAWriteInBetween(t *testing.T) {
	ctx := context.Background()
	players := mongotest.NewClient().Collection("game", "player")
	for _, doc := range []bson.M{
		{"_id": int64(1), "_version": int64(3), "_schema": int64(2), "_last_tx": "a", "level": int32(1)},
		{"_id": int64(2), "_version": int64(5), "_schema": int64(2), "_last_tx": "b", "level": int32(1)},
	} {
		if err := players.Seed(doc); err != nil {
			t.Fatal(err)
		}
	}
	before, err := snapshotPlayers(ctx, players, []int64{1, 2, 3})
	if err != nil || !before[0].Found || before[2].Found {
		t.Fatalf("snapshot = %+v, %v", before, err)
	}
	same, _ := snapshotPlayers(ctx, players, []int64{1, 2, 3})
	if err := compareSnapshots(before, same); err != nil {
		t.Fatalf("an unchanged collection compared as changed: %v", err)
	}
	if _, err := players.UpdateOne(ctx, bson.M{"_id": int64(2)}, bson.M{"$set": bson.M{"_version": int64(6), "_last_tx": "late"}}); err != nil {
		t.Fatal(err)
	}
	after, _ := snapshotPlayers(ctx, players, []int64{1, 2, 3})
	if err := compareSnapshots(before, after); !errors.Is(err, ErrChanged) {
		t.Fatalf("a write between the snapshots was not reported: %v", err)
	}
}
