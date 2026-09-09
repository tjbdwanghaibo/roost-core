package combatcomponent

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/skill"
	"github.com/tjbdwanghaibo/roost-core/skill/combat"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// U-0130 · C2（空洞测试）· nightly gap map core `skill/combatcomponent` 11/20。
//
// 宿主适配器与状态桥在事务外会自开一段分离事务再重入自身；事务开不起来（无
// Committer）时拿到的是 nil 结果，必须把错误交回而不是对 nil 做类型断言。读请求
// 与费用支付对"没有战斗组件的实体"和"没有属性映射的资源"要报具体错误并且
// 不动属性底值。持久化 DAO：落盘 id 与 DAO id 不符、不支持的迁移起点、掩码里
// 没有任何字段的补丁都必须拒绝。状态桥修改一个目录里查不到策略的 buff 实例
// 要报错，而不是默认放行或默认拒绝。

func TestDetachedTransactionFailuresAreReturnedNotDereferenced(t *testing.T) {
	adapter, revision, _, defender := newAdapterFixture()
	adapter.Committer = nil
	_, handled, err := adapter.Apply(skill.EffectCommand{Payload: skill.DamageCommand{Source: 1, Target: 2, Amount: 10}})
	if !errors.Is(err, nest.ErrCommitterRequired) || !handled {
		t.Fatalf("Apply without a committer = (handled=%v, %v), want ErrCommitterRequired", handled, err)
	}
	if _, err := adapter.PayCosts(skill.CostPayment{Entity: 2, Entries: []skill.CostEntry{{Resource: "mana", Amount: 1}}}); !errors.Is(err, nest.ErrCommitterRequired) {
		t.Fatalf("PayCosts without a committer = %v", err)
	}
	if defender.Combatant().Health != 100 || defender.AttributeBase(5) != 50 || revision.revision != 0 {
		t.Fatalf("a failed detached transaction changed state: health=%d mana=%d revision=%d", defender.Combatant().Health, defender.AttributeBase(5), revision.revision)
	}

	bridge, bridgeRevision, target, _ := newBridgeFixture()
	bridge.Committer = nil
	_, handled, err = bridge.Apply(skill.EffectCommand{Payload: skill.StatusCommand{SourceOwner: 1, Target: 2, Status: 10, DurationTicks: 20, Stacks: 1}})
	if !errors.Is(err, nest.ErrCommitterRequired) || !handled {
		t.Fatalf("StatusBridge.Apply without a committer = (handled=%v, %v)", handled, err)
	}
	if len(target.ActiveBuffs()) != 0 || bridgeRevision.revision != 0 {
		t.Fatal("a failed detached status transaction applied a buff")
	}
}

func TestReadsAndPaymentsRefuseUnknownEntitiesAndUnmappedResources(t *testing.T) {
	adapter, revision, _, defender := newAdapterFixture()
	if _, handled, err := adapter.Read(skill.ReadRequest{Payload: skill.AttributeRead{Entity: 9, Attribute: 3}}); !handled || err == nil || !strings.Contains(err.Error(), "entity 9 has no combat component") {
		t.Fatalf("AttributeRead of an unknown entity = (handled=%v, %v)", handled, err)
	}
	if _, handled, err := adapter.Read(skill.ReadRequest{Payload: skill.ResourceRead{Entity: 9, Resource: "mana"}}); !handled || err == nil || !strings.Contains(err.Error(), "entity 9 has no combat component") {
		t.Fatalf("ResourceRead of an unknown entity = (handled=%v, %v)", handled, err)
	}
	if _, handled, err := adapter.Read(skill.ReadRequest{Payload: skill.ResourceRead{Entity: 2, Resource: "rage"}}); !handled || err == nil || !strings.Contains(err.Error(), `resource "rage" has no attribute mapping`) {
		t.Fatalf("ResourceRead of an unmapped resource = (handled=%v, %v)", handled, err)
	}
	result, handled, err := adapter.Read(skill.ReadRequest{Payload: skill.ResourceRead{Entity: 2, Resource: "mana"}})
	if mana, ok := result.Value.Int(); !handled || err != nil || !ok || mana != 50 {
		t.Fatalf("ResourceRead of mana = (%+v, %v, %v)", result, handled, err)
	}

	if _, err := adapter.PayCosts(skill.CostPayment{Entity: 9, Entries: []skill.CostEntry{{Resource: "mana", Amount: 1}}}); err == nil || !strings.Contains(err.Error(), "entity 9 has no combat component") {
		t.Fatalf("PayCosts for an unknown entity = %v", err)
	}
	if _, err := adapter.PayCosts(skill.CostPayment{Entity: 2, Entries: []skill.CostEntry{{Resource: "mana", Amount: 1}, {Resource: "rage", Amount: 1}}}); err == nil || !strings.Contains(err.Error(), `resource "rage" has no attribute mapping`) {
		t.Fatalf("PayCosts with an unmapped resource = %v", err)
	}
	if defender.AttributeBase(5) != 50 || revision.revision != 0 {
		t.Fatalf("a refused payment changed state: mana=%d revision=%d", defender.AttributeBase(5), revision.revision)
	}
}

func TestCombatDaoRefusesForeignPersistedStateAndEmptyPatches(t *testing.T) {
	source := NewCombatDao(42, "game")
	source.combatant = combat.Combatant{Alive: true, Health: 70, MaxHealth: 100}
	payload, schemaVersion, err := source.MarshalPersisted()
	if err != nil {
		t.Fatal(err)
	}
	// 落盘的是 42 的状态，却装进 43 的 DAO。
	other := NewCombatDao(43, "game")
	if err := other.RestorePersisted(payload, schemaVersion, 1); err == nil || !strings.Contains(err.Error(), "persisted id 42 does not match dao id 43") {
		t.Fatalf("RestorePersisted with another entity's document = %v", err)
	}
	if other.combatant.Health != 0 {
		t.Fatal("a refused restore applied the foreign state")
	}
	// 迁移只认 1 → 2。
	if _, err := other.Migrate(payload, 0); err == nil || !strings.Contains(err.Error(), "unsupported migration 0 -> 2") {
		t.Fatalf("Migrate from schema 0 = %v", err)
	}
	if err := other.RestorePersisted(payload, 0, 1); err == nil || !strings.Contains(err.Error(), "unsupported migration 0 -> 2") {
		t.Fatalf("RestorePersisted from schema 0 = %v", err)
	}
	legacy, err := json.Marshal(persistedCombatState{Combatant: combat.Combatant{Alive: true, Health: 33, MaxHealth: 100}})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.RestorePersisted(legacy, 1, 1); err != nil || other.combatant.Health != 33 {
		t.Fatalf("RestorePersisted from legacy schema 1 = %v, health=%d", err, other.combatant.Health)
	}
	// 已有版本的 DAO：掩码里没有任何字段的补丁不能变成一个空 $set。
	restored := NewCombatDao(42, "game")
	if err := restored.RestorePersisted(payload, schemaVersion, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.PrepareMutation(nest.PersistChange{Mask: 1 << 10}); err == nil || !strings.Contains(err.Error(), "has no fields") {
		t.Fatalf("PrepareMutation with a fieldless mask = %v", err)
	}
	mutation, err := restored.PrepareMutation(nest.PersistChange{Mask: FieldVitals})
	if err != nil || len(mutation.Patch.SetBSON) == 0 {
		t.Fatalf("PrepareMutation with vitals = (%+v, %v)", mutation, err)
	}
	var set bson.M
	if err := bson.Unmarshal(mutation.Patch.SetBSON, &set); err != nil || set["combatant"] == nil {
		t.Fatalf("vitals patch = %v, %v", set, err)
	}
}

func TestStatusBridgeRefusesToModifyABuffWithoutACatalogPolicy(t *testing.T) {
	bridge, revision, target, _ := newBridgeFixture()
	// 目录里没有 handle 99 的条目，但容器里有这样一个 buff（例如目录改版后遗留）。
	instanceID, _ := target.dao.buffs.Apply(combat.BuffSpec{ID: 99, MaxStacks: 3, DurationTicks: 30}, 0, 1)
	_, handled, err := bridge.Apply(skill.EffectCommand{Payload: statusInstanceCommand(1, 2, instanceID, "add_stacks", 1)})
	if !handled || err == nil || !strings.Contains(err.Error(), "buff 99 has no status policy") {
		t.Fatalf("modifying a buff without a catalog policy = (handled=%v, %v)", handled, err)
	}
	if revision.revision != 0 {
		t.Fatal("a refused modification committed a revision")
	}
}
