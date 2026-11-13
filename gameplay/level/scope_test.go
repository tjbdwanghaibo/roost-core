package level

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// 组件 DAO 跟随宿主实体的 dbscope（维护者 2026-11-13）：挂在 dbscope=sid 的实体（如副本 Dungeon）上的关卡状态，
// 声明的范围与提交写入键的范围都要是宿主的那一个。
func TestLevelDAOFollowsItsHostsDatabaseScope(t *testing.T) {
	for _, scope := range []dataengine.DatabaseScope{dataengine.DatabaseServer, dataengine.DatabaseGlobal} {
		dao := NewLevelDAOIn(7, "game", scope)
		if got := dataengine.ResolveDatabaseScope(dao); got != scope {
			t.Fatalf("NewLevelDAOIn(scope=%d) declares scope %d", scope, got)
		}
		mutation, err := dao.PrepareMutation(nest.PersistChange{Mask: 1})
		if err != nil {
			t.Fatal(err)
		}
		if mutation.Key.Scope != scope {
			t.Fatalf("scope %d: the commit writes key scope %d", scope, mutation.Key.Scope)
		}
	}
	if got := dataengine.ResolveDatabaseScope(NewLevelDAO(8, "game")); got != dataengine.DatabaseGlobal {
		t.Fatalf("NewLevelDAO declares scope %d, want global", got)
	}
}
