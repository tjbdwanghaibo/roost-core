package combatcomponent

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// 组件 DAO 跟随宿主实体的 dbscope（维护者 2026-11-13）：战斗状态挂在 dbscope=sid 的 Player 上时，它的文档也要在
// 本服库里——声明的范围（加载、预加载、搬档按它选库）与提交的写入键（投影按它选库）必须是同一个。
func TestCombatDaoFollowsItsHostsDatabaseScope(t *testing.T) {
	for _, scope := range []dataengine.DatabaseScope{dataengine.DatabaseServer, dataengine.DatabaseGlobal} {
		dao := NewCombatDaoIn(7, "game", scope)
		if got := dataengine.ResolveDatabaseScope(dao); got != scope {
			t.Fatalf("NewCombatDaoIn(scope=%d) declares scope %d; loading and preload would pick the wrong database", scope, got)
		}
		mutation, err := dao.PrepareMutation(nest.PersistChange{Mask: FieldVitals})
		if err != nil {
			t.Fatal(err)
		}
		if mutation.Key.Scope != scope {
			t.Fatalf("scope %d: the commit writes key scope %d; the projector would write a different database than the one loads read", scope, mutation.Key.Scope)
		}
	}
	// 不指定范围的旧构造保持 global（独立示例、测试里直接 new 的战斗状态）。
	if got := dataengine.ResolveDatabaseScope(NewCombatDao(8, "game")); got != dataengine.DatabaseGlobal {
		t.Fatalf("NewCombatDao declares scope %d, want global", got)
	}
}
