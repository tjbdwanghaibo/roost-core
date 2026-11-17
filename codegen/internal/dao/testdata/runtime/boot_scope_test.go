//go:build daoruntime

// dbscope=boot（R3，维护者 2026-11-14 D11 Q17 A：地形按地图归属选库范围）：同一个 DAO 的库范围由进程起服时选，
// 缺省按服分库；SetRealmDaoDbScope(global) 之后 DbScope 与提交记录里的文档键都换成共用库，换回 sid 也跟着换回。
// 提交记录的 Key.Scope 就是 DataEngine 投影、WAL 记下的范围，所以这里核对的是“写到哪个库”。
package testdata

import (
	"context"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

func TestBootScopedDaoFollowsTheProcessChoice(t *testing.T) {
	t.Cleanup(func() { SetRealmDaoDbScope(dataengine.DatabaseServer) })
	commitScope := func(owner int64) dataengine.DatabaseScope {
		t.Helper()
		realm := NewRealmDao()
		realm.SetId(owner)
		committer := &ownershipCommitter{}
		if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "claim", func() (any, error) {
			realm.SetOwner(owner)
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
		if len(committer.records) != 1 || len(committer.records[0].Mutations) != 1 {
			t.Fatalf("commit records = %+v", committer.records)
		}
		key := committer.records[0].Mutations[0].Key
		if key.Scope != realm.DbScope() || key.Database != RealmDaoDBName || key.Resource != RealmDaoCollection {
			t.Fatalf("mutation key %+v, DAO scope %d", key, realm.DbScope())
		}
		return key.Scope
	}
	if got := NewRealmDao().DbScope(); got != dataengine.DatabaseServer {
		t.Fatalf("default scope %d, want sid (DatabaseServer)", got)
	}
	if got := commitScope(1); got != dataengine.DatabaseServer {
		t.Fatalf("default commit scope %d, want sid", got)
	}
	SetRealmDaoDbScope(dataengine.DatabaseGlobal)
	if got := dataengine.ResolveDatabaseScope(NewRealmDao()); got != dataengine.DatabaseGlobal {
		t.Fatalf("after SetRealmDaoDbScope(global): scope %d", got)
	}
	if got := commitScope(2); got != dataengine.DatabaseGlobal {
		t.Fatalf("commit scope after SetRealmDaoDbScope(global) %d", got)
	}
	SetRealmDaoDbScope(dataengine.DatabaseServer)
	if got := commitScope(3); got != dataengine.DatabaseServer {
		t.Fatalf("commit scope after SetRealmDaoDbScope(sid) %d", got)
	}
	// 别的 DAO 不受影响：HeroDao 是写死的 dbscope=sid。
	if got := NewHeroDao().DbScope(); got != dataengine.DatabaseServer {
		t.Fatalf("HeroDao scope %d", got)
	}
}
