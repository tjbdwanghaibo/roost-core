//go:build daoruntime

// A1（维护者 2026-10-05：回滚统一走 DAO）：`dao:"nopersist,nosync"` 字段是“只参与事务”的状态。在真实
// roost-core 上：undo 策略下它的 mutator 登记逆操作，事务失败时回到原值；state 策略的快照覆盖它；提交记录
// （也就是 WAL 收到的记录）与同步载荷里没有它；从存储读回的文档也不带它。
//
// 和 roundtrip_test.go 一样，这个文件不由 `go test ./...` 编译：
// scripts/dao-golden-runtime.sh 把它和 golden 放进一次性模块里跑。
package testdata

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/nest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestATransientFieldRollsBackWithTheTransaction(t *testing.T) {
	variety := NewVarietyDao()
	variety.SetId(11)
	committer := &ownershipCommitter{}
	refused := errors.New("refused")
	if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "fail", func() (any, error) {
		variety.SetPersistOnly(1)
		variety.SetNeither(5)
		variety.SetPending(1, 10)
		return nil, refused
	}); !errors.Is(err, refused) {
		t.Fatalf("err = %v", err)
	}
	if variety.GetNeither() != 0 || variety.PendingLen() != 0 || variety.GetPersistOnly() != 0 {
		t.Fatalf("rollback left neither %d pending %d persistOnly %d", variety.GetNeither(), variety.PendingLen(), variety.GetPersistOnly())
	}
}

func TestATransientFieldNeverReachesTheCommitRecordOrSync(t *testing.T) {
	variety := NewVarietyDao()
	variety.SetId(12)
	committer := &ownershipCommitter{}
	for _, step := range []func(){
		func() { variety.SetPersistOnly(1); variety.SetNeither(5); variety.SetPending(1, 10) }, // first write: a Put
		func() { variety.SetPersistOnly(2); variety.SetNeither(6); variety.SetPending(2, 20) }, // then a patch
	} {
		if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "write", func() (any, error) {
			step()
			return nil, nil
		}); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	if variety.GetNeither() != 6 || variety.PendingLen() != 2 {
		t.Fatalf("committed values lost: neither %d pending %d", variety.GetNeither(), variety.PendingLen())
	}
	if len(committer.records) != 2 {
		t.Fatalf("%d records, want 2", len(committer.records))
	}
	for index, record := range committer.records {
		if len(record.Mutations) != 1 {
			t.Fatalf("record %d has %d mutations", index, len(record.Mutations))
		}
		mutation := record.Mutations[0]
		raw := mutation.Data
		if len(raw) == 0 {
			raw = mutation.Patch.SetBSON
		}
		var doc bson.M
		if err := bson.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		for key := range doc {
			if strings.HasPrefix(key, "neither") || strings.HasPrefix(key, "pending") {
				t.Fatalf("record %d carries the transient %s: %v", index, key, doc)
			}
		}
		if _, ok := doc["persist_only"]; !ok {
			t.Fatalf("record %d lost persist_only: %v", index, doc)
		}
	}
	if payload := variety.MarshalSync(^uint64(0)); strings.Contains(string(payload), "neither") || strings.Contains(string(payload), "pending") {
		t.Fatal("a transient field reached the sync payload")
	}
	restored := NewVarietyDao()
	if err := restored.Unmarshal(variety.Marshal()); err != nil {
		t.Fatal(err)
	}
	if restored.GetNeither() != 0 || restored.PendingLen() != 0 {
		t.Fatalf("storage brought transient state back: neither %d pending %d", restored.GetNeither(), restored.PendingLen())
	}
}

func TestATransientFieldIsInTheStateSnapshot(t *testing.T) {
	variety := NewVarietyDao()
	committer := &ownershipCommitter{}
	if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "seed", func() (any, error) {
		variety.SetNeither(5)
		variety.SetPending(1, 10)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := variety.CaptureRollbackState()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "change", func() (any, error) {
		variety.SetNeither(9)
		variety.DelPending(1)
		variety.SetPending(2, 20)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := variety.RestoreRollbackState(snapshot); err != nil {
		t.Fatal(err)
	}
	if got, ok := variety.GetPending(1); variety.GetNeither() != 5 || variety.PendingLen() != 1 || !ok || got != 10 {
		t.Fatalf("state restore = neither %d pending %d (%d %v)", variety.GetNeither(), variety.PendingLen(), got, ok)
	}
}
