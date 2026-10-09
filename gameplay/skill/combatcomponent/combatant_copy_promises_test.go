package combatcomponent

// RR-20261005-NC-113：Combatant() 承诺返回 vitals 的副本，InitCombatant 承诺替换 vitals 块；两者都把
// ElementMultipliersBP 这张 map 原样共享。调用方改“副本”或改自己传进去的 map（例如多个实体共用的配置模板），
// 就在事务、逆操作和脏标记之外改掉了权威战斗状态：回滚撤不掉、持久化与同步都看不见，直到下一次 vitals 写入
// 才被顺带落盘。

import (
	"context"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/gameplay/skill/combat"
)

func TestCombatantCopiesDoNotShareElementMultipliers(t *testing.T) {
	dao := NewCombatDao(42, "game")
	component := NewCombatComponent(dao)
	template := map[combat.Element]int64{3: 12000}
	if _, err := nest.RunDetachedTransaction(context.Background(), &combatRecordingCommitter{}, "combatant_copy", func() (any, error) {
		component.InitCombatant(combat.Combatant{Alive: true, Health: 10, MaxHealth: 10, ElementMultipliersBP: template})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	template[3] = 1
	if got := component.Combatant().ElementMultipliersBP[3]; got != 12000 {
		t.Errorf("mutating the caller's template changed the stored multiplier to %d, want 12000", got)
	}
	// 独立于上一条断言：重置存储值后再检查返回的副本。
	dao.combatant.ElementMultipliersBP = map[combat.Element]int64{3: 12000}
	read := component.Combatant()
	read.ElementMultipliersBP[3] = 0
	if got := component.Combatant().ElementMultipliersBP[3]; got != 12000 {
		t.Fatalf("mutating the returned copy changed the stored multiplier to %d, want 12000", got)
	}
}
