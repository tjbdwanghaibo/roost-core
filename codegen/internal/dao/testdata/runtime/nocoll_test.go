//go:build daoruntime

// W-2026-09-18-09（选 A）：`//roost:dao nocoll` 的 DAO 在真实 roost-core 上要做到
// 三件事——改动只复制、不进提交记录的持久化部分；事务失败时内存回滚照常；它没有
// 任何持久化能力可以被误用（不是 MutationParticipant，也不是 PersistedDaoLoader）。
//
// 和 roundtrip_test.go 一样，这个文件不由 `go test ./...` 编译：
// scripts/dao-golden-runtime.sh 把它和 golden 放进一次性模块里跑。
package testdata

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestANoCollectionDaoReplicatesAndNeverPersists(t *testing.T) {
	wraith := NewWraithDao()
	wraith.SetId(7)
	committer := &ownershipCommitter{}
	if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "move", func() (any, error) {
		wraith.SetPosX(10)
		wraith.SetBuffs(3, 30)
		wraith.AddTrail(99)
		return nil, nil
	}); err != nil {
		t.Fatalf("move: %v", err)
	}
	for _, record := range committer.records {
		if len(record.Mutations) != 0 {
			t.Fatalf("a nocoll DAO produced persistence mutations: %+v", record.Mutations)
		}
	}

	mask := wraith.DirtyTracker().TakeSyncDirty()
	if mask == 0 {
		t.Fatal("the move left no sync dirty bits; nothing would be replicated")
	}
	payload := wraith.MarshalSync(mask)
	var doc bson.M
	if err := bson.Unmarshal(payload, &doc); err != nil {
		t.Fatal(err)
	}
	if _, leaked := doc["scratch"]; leaked {
		t.Fatal("a nosync field reached the sync payload")
	}
	mirror := NewWraithDao()
	if err := mirror.ApplySync(payload); err != nil {
		t.Fatal(err)
	}
	if mirror.GetPosX() != 10 || mirror.TrailLen() != 1 {
		t.Fatalf("mirror = posX %d trail %d", mirror.GetPosX(), mirror.TrailLen())
	}
	if buff, ok := mirror.GetBuffs(3); !ok || buff != 30 {
		t.Fatalf("mirror buff = %d %v", buff, ok)
	}
}

func TestANoCollectionDaoRollsBackInMemory(t *testing.T) {
	wraith := NewWraithDao()
	committer := &ownershipCommitter{}
	refused := errors.New("refused")
	if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "move", func() (any, error) {
		wraith.SetPosX(10)
		wraith.SetBuffs(3, 30)
		return nil, refused
	}); !errors.Is(err, refused) {
		t.Fatalf("err = %v", err)
	}
	if wraith.GetPosX() != 0 || wraith.BuffsLen() != 0 {
		t.Fatalf("rollback left posX %d buffs %d", wraith.GetPosX(), wraith.BuffsLen())
	}
}

func TestANoCollectionDaoHasNoStorageCapability(t *testing.T) {
	var dao any = NewWraithDao()
	if _, ok := dao.(nest.MutationParticipant); ok {
		t.Fatal("a nocoll DAO can prepare persistence mutations")
	}
	if _, ok := dao.(entity.PersistedDaoLoader); ok {
		t.Fatal("a nocoll DAO can be hydrated from storage")
	}
	if _, ok := dao.(entity.DatabaseScopedDao); ok {
		t.Fatal("a nocoll DAO declares a database scope")
	}
	wraith := dao.(*WraithDao)
	if wraith.DbName() != "" || wraith.CollName() != WraithDaoRegistryKey {
		t.Fatalf("DbName=%q CollName=%q", wraith.DbName(), wraith.CollName())
	}
}
