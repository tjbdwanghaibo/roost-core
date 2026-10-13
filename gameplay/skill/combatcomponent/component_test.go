package combatcomponent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
	"github.com/tjbdwanghaibo/roost-core/gameplay/skill/combat"
)

const combatTestKind entity.EntityKind = 246

type combatTestEntity struct {
	*entity.EntityBase
	component *CombatComponent
}

func (e *combatTestEntity) Base() *entity.EntityBase { return e.EntityBase }
func (e *combatTestEntity) RangeDao(f func(entity.DaoInterface)) {
	if f != nil {
		f(e.component.Dao())
	}
}

type testGetter struct {
	mu       sync.RWMutex
	entities map[int64]entity.IThreadSafeEntity
	groups   *entity.EntityManager
}

func newTestGetter() *testGetter {
	return &testGetter{entities: make(map[int64]entity.IThreadSafeEntity), groups: entity.NewEntityManager()}
}

func (g *testGetter) Add(e entity.IThreadSafeEntity) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.entities[e.ID()] = e
}

func (g *testGetter) Get(_ context.Context, id int64, _ entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	e, ok := g.entities[id]
	if !ok {
		return nil, nest.ErrEntityNotFound
	}
	return e, nil
}

func (g *testGetter) GetMany(_ context.Context, ids []int64, _ []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	result := make([]entity.IThreadSafeEntity, len(ids))
	for index, id := range ids {
		result[index] = g.entities[id]
	}
	return result, nil
}

func (g *testGetter) GetGroupEntity(groupID, entityID int64) entity.IThreadSafeEntity {
	return g.groups.GetGroupEntity(groupID, entityID)
}

func (g *testGetter) GetGroupEntities(groupID int64) []entity.IThreadSafeEntity {
	return g.groups.GetGroupEntities(groupID)
}

func (g *testGetter) UpdateEntityGroup(value entity.IThreadSafeEntity, groupID int64) error {
	return g.groups.UpdateEntityGroup(value, groupID)
}

func newCombatTestEntity(t *testing.T, uniqueID int64) (*combatTestEntity, int64) {
	t.Helper()
	entity.MustRegisterEntityKindCategory(combatTestKind, entity.EntityCategory(1))
	id, err := entity.BuildEntityID(uniqueID, combatTestKind)
	if err != nil {
		t.Fatalf("BuildEntityID: %v", err)
	}
	dao := NewCombatDao(id, "game")
	test := &combatTestEntity{
		EntityBase: entity.NewEntityBase(id, entity.EntityCategory(1), false, combatTestKind),
		component:  NewCombatComponent(dao),
	}
	return test, id
}

func mustMarshal(t *testing.T, dao *CombatDao) []byte {
	t.Helper()
	payload, _, err := dao.MarshalPersisted()
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestNestUndoRollbackRestoresCombatStateExactly(t *testing.T) {
	nest.ResetHandlersForTest()
	t.Cleanup(nest.ResetHandlersForTest)
	getter := newTestGetter()
	attacker, attackerID := newCombatTestEntity(t, 9001)
	defender, defenderID := newCombatTestEntity(t, 9002)
	getter.Add(attacker)
	getter.Add(defender)

	// The combat DAO's fields are persistent, so the handlers below declare
	// a durable policy: since RR-20261006-41 a durability=memory transaction
	// that changes persistent fields is rejected and rolled back.
	engine := nest.NewEngine(
		nest.NestOptionWithGetter(getter),
		nest.NestOptionWithTransactionCommitter(acceptingCombatCommitter{}),
		nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 64}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{}),
		nest.NestOptionWithTickDuration(100*time.Millisecond),
	)
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = engine.Shutdown(context.Background()) }()

	seed := func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		defenderEntity := es[0].(*combatTestEntity)
		attackerEntity := es[1].(*combatTestEntity)
		defenderEntity.component.InitCombatant(combat.Combatant{Alive: true, Health: 100, MaxHealth: 100, Armor: 30})
		defenderEntity.component.SetAttributeBase(1, 100)
		defenderEntity.component.ApplyBuff(combat.BuffSpec{ID: 7, Tags: []combat.Tag{"magic"}, DurationTicks: 50, Modifiers: []combat.Modifier{{Attribute: 1, Flat: 25}}}, 0, int64(attackerID))
		attackerEntity.component.InitCombatant(combat.Combatant{Alive: true, Health: 80, MaxHealth: 80, VampBP: 5000})
		return nil, nil
	}
	mutate := func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		defenderEntity := es[0].(*combatTestEntity)
		attackerEntity := es[1].(*combatTestEntity)
		outcome, ok := defenderEntity.component.ApplyDamage(attackerEntity.component, combat.DamageInput{Amount: 40, Type: combat.DamageTypePhysical}, nil)
		if !ok || outcome.HealthDamage == 0 {
			return nil, errors.New("damage did not land")
		}
		defenderEntity.component.SetAttributeBase(1, 5)
		defenderEntity.component.DispelBuffs("magic", 0)
		defenderEntity.component.ApplyBuff(combat.BuffSpec{ID: 8, DurationTicks: 10}, 1, 0)
		return nil, errors.New("boom")
	}
	nest.MustRegisterHandlerWithMeta(nest.NewHandlerName("combat_seed"), seed, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityStrict})
	nest.MustRegisterHandlerWithMeta(nest.NewHandlerName("combat_mutate_fail"), mutate, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityStrict})

	if _, err := engine.RequestMulti(context.Background(), nest.NewHandlerName("combat_seed"), []int64{defenderID, attackerID}, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	defenderBefore := mustMarshal(t, defender.component.Dao())
	attackerBefore := mustMarshal(t, attacker.component.Dao())
	defender.component.Dao().CleanDirty()
	attacker.component.Dao().CleanDirty()

	if _, err := engine.RequestMulti(context.Background(), nest.NewHandlerName("combat_mutate_fail"), []int64{defenderID, attackerID}, nil); err == nil {
		t.Fatal("expected handler error")
	}
	if got := mustMarshal(t, defender.component.Dao()); !bytes.Equal(got, defenderBefore) {
		t.Fatalf("defender state diverged after rollback:\n got %s\nwant %s", got, defenderBefore)
	}
	if got := mustMarshal(t, attacker.component.Dao()); !bytes.Equal(got, attackerBefore) {
		t.Fatalf("attacker state diverged after rollback")
	}
	if defender.component.Dao().DirtyTracker().HasSyncDirty() || attacker.component.Dao().DirtyTracker().HasSyncDirty() {
		t.Fatal("sync dirty masks survived rollback")
	}
	// The rolled-back buff's attribute grant must be gone and the original
	// buff's grant restored.
	if got := defender.component.AttributeCurrent(1); got != 125 {
		t.Fatalf("attribute current = %d after rollback, want 125", got)
	}
}

func TestCombatDaoPersistenceRoundTrip(t *testing.T) {
	dao := NewCombatDao(42, "game")
	component := NewCombatComponent(dao)
	dao.combatant = combat.Combatant{Alive: true, Health: 70, MaxHealth: 100, Shield: 5}
	dao.attributes.SetBase(3, 40)
	dao.attributes.SetBounds(3, combat.AttributeBounds{Minimum: 0, Maximum: 500})
	dao.buffs.Apply(combat.BuffSpec{ID: 2, Tags: []combat.Tag{"magic"}, MaxStacks: 3, DurationTicks: 30, Modifiers: []combat.Modifier{{Attribute: 3, RateBP: 5000}}}, 10, 1)
	dao.buffs.Apply(combat.BuffSpec{ID: 2, Tags: []combat.Tag{"magic"}, MaxStacks: 3, DurationTicks: 30, Modifiers: []combat.Modifier{{Attribute: 3, RateBP: 5000}}}, 12, 1)

	payload, schemaVersion, err := dao.MarshalPersisted()
	if err != nil {
		t.Fatal(err)
	}
	restoredDao := NewCombatDao(42, "game")
	if err := restoredDao.RestorePersisted(payload, schemaVersion, 1); err != nil {
		t.Fatal(err)
	}
	restoredPayload, _, err := restoredDao.MarshalPersisted()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, restoredPayload) {
		t.Fatalf("round trip diverged:\n got %s\nwant %s", restoredPayload, payload)
	}
	restored := NewCombatComponent(restoredDao)
	if got := restored.AttributeCurrent(3); got != component.AttributeCurrent(3) || got != 80 { // 40 * (1 + 2*0.5)
		t.Fatalf("restored attribute = %d, want 80", got)
	}
	// The instance sequence survives, so new applications keep unique ids.
	id, _ := restoredDao.buffs.Apply(combat.BuffSpec{ID: 9}, 20, 0)
	if id != 2 {
		t.Fatalf("restored sequence issued id %d, want 2", id)
	}
	if err := restoredDao.RestorePersisted(payload, combatSchemaVersion+1, 1); err == nil {
		t.Fatal("newer schema version accepted")
	}
}

func TestNestUndoWorksThroughRealHandlers(t *testing.T) {
	nest.ResetHandlersForTest()
	t.Cleanup(nest.ResetHandlersForTest)
	// Committed handlers must keep their mutations and their dirty bits.
	getter := newTestGetter()
	target, targetID := newCombatTestEntity(t, 9003)
	getter.Add(target)
	// The combat DAO's fields are persistent, so the handlers below declare
	// a durable policy: since RR-20261006-41 a durability=memory transaction
	// that changes persistent fields is rejected and rolled back.
	engine := nest.NewEngine(
		nest.NestOptionWithGetter(getter),
		nest.NestOptionWithTransactionCommitter(acceptingCombatCommitter{}),
		nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 64}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{}),
		nest.NestOptionWithTickDuration(100*time.Millisecond),
	)
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = engine.Shutdown(context.Background()) }()
	nest.MustRegisterHandlerWithMeta(nest.NewHandlerName("combat_commit"), func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		component := es[0].(*combatTestEntity).component
		component.InitCombatant(combat.Combatant{Alive: true, Health: 50, MaxHealth: 50})
		component.Heal(10)
		component.AddShield(8)
		return nil, nil
	}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityStrict})
	if _, err := engine.Request(context.Background(), nest.NewHandlerName("combat_commit"), targetID, nil); err != nil {
		t.Fatal(err)
	}
	combatant := target.component.Combatant()
	if combatant.Health != 50 || combatant.Shield != 8 {
		t.Fatalf("combatant = %+v", combatant)
	}
	if !target.component.Dao().DirtyTracker().HasSyncDirty() {
		t.Fatal("committed mutation left no sync dirty bits")
	}
}

type combatRecordingCommitter struct {
	record nest.CommitRecord
}

func (committer *combatRecordingCommitter) Commit(_ context.Context, record nest.CommitRecord) error {
	committer.record = record
	return nil
}

func TestCombatDaoProducesTransactionLocalMutationAndSyncMask(t *testing.T) {
	nest.ResetHandlersForTest()
	t.Cleanup(nest.ResetHandlersForTest)
	getter := newTestGetter()
	target, targetID := newCombatTestEntity(t, 9004)
	getter.Add(target)
	committer := &combatRecordingCommitter{}
	engine := nest.NewEngine(
		nest.NestOptionWithGetter(getter),
		nest.NestOptionWithTransactionCommitter(committer),
		nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 64}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{}),
	)
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = engine.Shutdown(context.Background()) }()

	nest.MustRegisterHandlerWithMeta(nest.NewHandlerName("combat_dataengine_commit"), func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		component := es[0].(*combatTestEntity).component
		component.InitCombatant(combat.Combatant{Alive: true, Health: 90, MaxHealth: 100})
		component.SetAttributeBase(3, 40)
		return nil, nil
	}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityStrict})
	if _, err := engine.Request(context.Background(), nest.NewHandlerName("combat_dataengine_commit"), targetID, nil); err != nil {
		t.Fatal(err)
	}

	if len(committer.record.Mutations) != 1 {
		t.Fatalf("mutations=%d, want 1", len(committer.record.Mutations))
	}
	mutation := committer.record.Mutations[0]
	wantMask := FieldVitals | FieldAttributes
	if mutation.Mask != wantMask || mutation.Kind != dataengine.MutationPut || mutation.ExpectedVersion != 0 || mutation.NextVersion != 1 {
		t.Fatalf("mutation=%+v", mutation)
	}
	if target.component.Dao().DirtyTracker().Version() != 1 || target.component.Dao().DirtyTracker().SyncDirtyMask() != wantMask {
		t.Fatalf("tracker=%+v", target.component.Dao().DirtyTracker().Snapshot())
	}
}

func TestCombatMutationOutsideTransactionPanicsBeforeStateChange(t *testing.T) {
	dao := NewCombatDao(42, "game")
	component := NewCombatComponent(dao)
	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("mutation outside transaction did not panic")
		}
		if dao.combatant.Health != 0 {
			t.Fatalf("state changed before transaction rejection: %+v", dao.combatant)
		}
		if !errors.Is(fmt.Errorf("%v", recovered), nest.ErrTransactionClosed) && !strings.Contains(fmt.Sprint(recovered), nest.ErrTransactionClosed.Error()) {
			t.Fatalf("panic=%v, want ErrTransactionClosed", recovered)
		}
	}()
	component.InitCombatant(combat.Combatant{Alive: true, Health: 10, MaxHealth: 10})
}

// A1（维护者 2026-10-05：回滚统一走 DAO，docs/feature/REFACTOR-2026-10-05-dao-unified-rollback.md）：战斗状态的回滚
// 由 CombatDao 自己负责——undo 策略下 DAO 的改动方法登记逆操作，state 策略下 DAO 快照恢复——组件只调 DAO，不登记 undo。
// 承诺：经真实 Nest 派发，两种策略 × handler 失败 / 提交被拒，伤害（双方 vitals）、属性、驱散与加 buff 全部回到事务开始时
// 的字节，buff 的属性加成随之恢复，同步脏位不残留。

type acceptingCombatCommitter struct{}

func (acceptingCombatCommitter) Commit(context.Context, nest.CommitRecord) error { return nil }

type rejectingCombatCommitter struct{}

func (rejectingCombatCommitter) Commit(context.Context, nest.CommitRecord) error {
	return errors.New("admission refused")
}

func TestCombatRollbackIsTheDaoRollback(t *testing.T) {
	policies := []nest.RollbackPolicy{nest.RollbackUndo, nest.RollbackState}
	failures := []struct {
		name      string
		committer nest.TransactionCommitter
		failAfter bool
	}{
		{name: "handler_fails", committer: acceptingCombatCommitter{}, failAfter: true},
		{name: "commit_rejected", committer: rejectingCombatCommitter{}},
	}
	uniqueID := int64(9100)
	for _, policy := range policies {
		for _, failure := range failures {
			uniqueID += 2
			defenderUnique, attackerUnique := uniqueID, uniqueID+1
			t.Run(policy.String()+"/"+failure.name, func(t *testing.T) {
				getter := newTestGetter()
				attacker, attackerID := newCombatTestEntity(t, attackerUnique)
				defender, defenderID := newCombatTestEntity(t, defenderUnique)
				getter.Add(attacker)
				getter.Add(defender)
				meta := nest.HandlerMeta{Rollback: policy, Durability: nest.DurabilityStrict}

				seed := func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
					defenderEntity := es[0].(*combatTestEntity)
					attackerEntity := es[1].(*combatTestEntity)
					defenderEntity.component.InitCombatant(combat.Combatant{Alive: true, Health: 100, MaxHealth: 100, Armor: 30})
					defenderEntity.component.SetAttributeBase(1, 100)
					defenderEntity.component.ApplyBuff(combat.BuffSpec{ID: 7, Tags: []combat.Tag{"magic"}, DurationTicks: 50, Modifiers: []combat.Modifier{{Attribute: 1, Flat: 25}}}, 0, attackerID)
					attackerEntity.component.InitCombatant(combat.Combatant{Alive: true, Health: 40, MaxHealth: 80, VampBP: 5000})
					return nil, nil
				}
				if _, err := runCombatHandler(t, getter, meta, acceptingCombatCommitter{}, "combat_seed", seed, defenderID, attackerID); err != nil {
					t.Fatalf("seed: %v", err)
				}
				defenderBefore := mustMarshal(t, defender.component.Dao())
				attackerBefore := mustMarshal(t, attacker.component.Dao())
				defender.component.Dao().CleanDirty()
				attacker.component.Dao().CleanDirty()

				mutate := func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
					defenderEntity := es[0].(*combatTestEntity)
					attackerEntity := es[1].(*combatTestEntity)
					outcome, ok := defenderEntity.component.ApplyDamage(attackerEntity.component, combat.DamageInput{Amount: 40, Type: combat.DamageTypePhysical}, nil)
					if !ok || outcome.HealthDamage == 0 || outcome.VampHeal == 0 {
						return nil, errors.New("damage did not land")
					}
					// 迁移/复制的目的实体与吸血来源都在 RequestMulti 的声明目标中。
					attackerEntity.component.AdoptBuff(defenderEntity.component.ActiveBuffs()[0])
					defenderEntity.component.SetAttributeBase(1, 5)
					defenderEntity.component.DispelBuffs("magic", 0)
					defenderEntity.component.ApplyBuff(combat.BuffSpec{ID: 8, DurationTicks: 10}, 1, 0)
					if failure.failAfter {
						return nil, errors.New("boom")
					}
					return nil, nil
				}
				if _, err := runCombatHandler(t, getter, meta, failure.committer, "combat_mutate", mutate, defenderID, attackerID); err == nil {
					t.Fatal("the transaction was expected to fail")
				}
				if got := mustMarshal(t, defender.component.Dao()); !bytes.Equal(got, defenderBefore) {
					t.Fatalf("defender state diverged after rollback:\n got %x\nwant %x", got, defenderBefore)
				}
				if got := mustMarshal(t, attacker.component.Dao()); !bytes.Equal(got, attackerBefore) {
					t.Fatal("attacker state diverged after rollback (the lifesteal heal survived)")
				}
				if defender.component.Dao().DirtyTracker().HasSyncDirty() || attacker.component.Dao().DirtyTracker().HasSyncDirty() {
					t.Fatal("sync dirty masks survived rollback")
				}
				if got := defender.component.AttributeCurrent(1); got != 125 {
					t.Fatalf("attribute current = %d after rollback, want 125 (base 100 + the restored buff's 25)", got)
				}
			})
		}
	}
}

func runCombatHandler(t *testing.T, getter *testGetter, meta nest.HandlerMeta, committer nest.TransactionCommitter, name string, handler nest.BaseHandler, ids ...int64) (any, error) {
	t.Helper()
	engine := nest.NewEngine(
		nest.NestOptionWithGetter(getter),
		nest.NestOptionWithTransactionCommitter(committer),
		nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 16}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{}),
	)
	handlerName := nest.NewHandlerName(name)
	if err := engine.RegisterHandlerWithMeta(handlerName, handler, meta); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = engine.Shutdown(context.Background()) }()
	return engine.RequestMulti(context.Background(), handlerName, ids, nil)
}

// skill O2（维护者第十二轮决定，2026-10-06：组件给投影入口，投影交业务）：伤害管线读 Combatant
// 的平铺字段，buff 与属性修饰只改 AttributeSet，以前组件没有把属性写到 Combatant 的入口
// （CombatDao.attributes 不导出，applyState 还会换掉 AttributeSet 实例），经 buff 加的护甲
// 对伤害没有任何影响。
//
// 承诺：业务用 ProjectAttributes 装上投影后，buff 改属性 → 同一事务里投影写进 vitals →
// 伤害随之改变；投影写的字段走 DAO 的回滚（A1），两种回滚策略 × handler 失败 / 提交被拒
// 都回到事务开始时的字节；实体从存储加载后（OnInitFinish）按当前属性重新投影，不产生持久写。

const projectionArmor combat.AttributeID = 5

// projectArmor 是业务写的全部投影代码：属性 5 是护甲。
func projectArmor(attribute func(combat.AttributeID) int64, combatant *combat.Combatant) {
	combatant.Armor = attribute(projectionArmor)
}

func armorBuff(id combat.BuffID) combat.BuffSpec {
	return combat.BuffSpec{ID: id, DurationTicks: 50, Modifiers: []combat.Modifier{{Attribute: projectionArmor, Flat: 100}}}
}

func TestBuffAttributeModifierReachesDamage(t *testing.T) {
	getter := newTestGetter()
	defender, defenderID := newCombatTestEntity(t, 9301)
	defender.component.ProjectAttributes(projectArmor)
	getter.Add(defender)
	meta := nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityStrict}
	var withoutBuff, withBuff, afterDispel combat.DamageOutcome
	handler := func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		component := es[0].(*combatTestEntity).component
		component.InitCombatant(combat.Combatant{Alive: true, Health: 1000, MaxHealth: 1000})
		hit := func(outcome *combat.DamageOutcome) error {
			var ok bool
			if *outcome, ok = component.ApplyDamage(nil, combat.DamageInput{Amount: 100, Type: combat.DamageTypePhysical}, nil); !ok {
				return errors.New("damage did not land")
			}
			return nil
		}
		if err := hit(&withoutBuff); err != nil {
			return nil, err
		}
		buff, _ := component.ApplyBuff(armorBuff(11), 0, 0)
		if err := hit(&withBuff); err != nil {
			return nil, err
		}
		component.RemoveBuff(buff)
		return nil, hit(&afterDispel)
	}
	if _, err := runCombatHandler(t, getter, meta, acceptingCombatCommitter{}, "combat_projection", handler, defenderID); err != nil {
		t.Fatal(err)
	}
	if withoutBuff.HealthDamage != 100 || withBuff.HealthDamage != 50 || afterDispel.HealthDamage != 100 {
		t.Fatalf("health damage without / with / after removing the +100 armor buff = %d / %d / %d, want 100 / 50 / 100",
			withoutBuff.HealthDamage, withBuff.HealthDamage, afterDispel.HealthDamage)
	}
}

func TestAttributeProjectionRollsBackWithTheDao(t *testing.T) {
	failures := []struct {
		name      string
		committer nest.TransactionCommitter
		failAfter bool
	}{
		{name: "handler_fails", committer: acceptingCombatCommitter{}, failAfter: true},
		{name: "commit_rejected", committer: rejectingCombatCommitter{}},
	}
	uniqueID := int64(9310)
	for _, policy := range []nest.RollbackPolicy{nest.RollbackUndo, nest.RollbackState} {
		for _, failure := range failures {
			uniqueID++
			unique := uniqueID
			t.Run(policy.String()+"/"+failure.name, func(t *testing.T) {
				getter := newTestGetter()
				defender, defenderID := newCombatTestEntity(t, unique)
				defender.component.ProjectAttributes(projectArmor)
				getter.Add(defender)
				meta := nest.HandlerMeta{Rollback: policy, Durability: nest.DurabilityStrict}
				seed := func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
					component := es[0].(*combatTestEntity).component
					component.InitCombatant(combat.Combatant{Alive: true, Health: 1000, MaxHealth: 1000})
					component.SetAttributeBase(projectionArmor, 20)
					return nil, nil
				}
				if _, err := runCombatHandler(t, getter, meta, acceptingCombatCommitter{}, "combat_projection_seed", seed, defenderID); err != nil {
					t.Fatalf("seed: %v", err)
				}
				if got := defender.component.Combatant().Armor; got != 20 {
					t.Fatalf("armor after seeding base 20 = %d", got)
				}
				before := mustMarshal(t, defender.component.Dao())
				defender.component.Dao().CleanDirty()

				mutate := func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
					component := es[0].(*combatTestEntity).component
					component.ApplyBuff(armorBuff(12), 0, 0)
					if got := component.Combatant().Armor; got != 120 {
						return nil, errors.New("buff did not project into armor")
					}
					if failure.failAfter {
						return nil, errors.New("boom")
					}
					return nil, nil
				}
				if _, err := runCombatHandler(t, getter, meta, failure.committer, "combat_projection_mutate", mutate, defenderID); err == nil {
					t.Fatal("the transaction was expected to fail")
				}
				if got := mustMarshal(t, defender.component.Dao()); !bytes.Equal(got, before) {
					t.Fatalf("combat state diverged after rollback:\n got %x\nwant %x", got, before)
				}
				if got := defender.component.Combatant().Armor; got != 20 {
					t.Fatalf("projected armor after rollback = %d, want 20", got)
				}
				if defender.component.Dao().DirtyTracker().HasSyncDirty() {
					t.Fatal("sync dirty masks survived rollback")
				}
			})
		}
	}
}

func TestAttributeProjectionReprojectsOnLoad(t *testing.T) {
	getter := newTestGetter()
	stored, storedID := newCombatTestEntity(t, 9320)
	getter.Add(stored)
	meta := nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityStrict}
	// 存储里的 vitals 是没有投影（或旧投影）时提交的：属性护甲 50，Combatant.Armor 0。
	seed := func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		component := es[0].(*combatTestEntity).component
		component.InitCombatant(combat.Combatant{Alive: true, Health: 1000, MaxHealth: 1000})
		component.SetAttributeBase(projectionArmor, 50)
		return nil, nil
	}
	if _, err := runCombatHandler(t, getter, meta, acceptingCombatCommitter{}, "combat_projection_store", seed, storedID); err != nil {
		t.Fatal(err)
	}
	raw, schema, err := stored.component.Dao().MarshalPersisted()
	if err != nil {
		t.Fatal(err)
	}

	// 实体工厂的顺序：建 DAO 与组件、装投影，再从存储加载，最后 OnInitFinish。
	dao := NewCombatDao(storedID, "game")
	component := NewCombatComponent(dao)
	component.ProjectAttributes(projectArmor)
	if err := dao.RestorePersisted(raw, schema, 1); err != nil {
		t.Fatal(err)
	}
	if err := component.OnInitFinish(nil, false); err != nil {
		t.Fatal(err)
	}
	if got := component.Combatant().Armor; got != 50 {
		t.Fatalf("armor after load = %d, want the projected attribute 50", got)
	}
	if dao.DirtyTracker().HasSyncDirty() {
		t.Fatal("load-time projection marked the DAO dirty")
	}
}

// Combatant() 返回 vitals 的副本、InitCombatant 替换 vitals 块，都不共享 ElementMultipliersBP：调用方改“副本”或改自己
// 传进去的 map（例如多个实体共用的配置模板）不能绕过事务、逆操作和脏标记改掉权威战斗状态。

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

// HostAdapter.Apply 在 nest.CurrentRollbackTx()==nil 时自开 RunDetachedTransaction（memory handler 正是这种情况）：
// committer 返回 ErrCommitIndeterminate、业务吞掉错误时，handler 返回前引擎被 fence（FenceError 满足 ErrNestFenced 与
// ErrCommitIndeterminate），后续请求被拒。

type combatIndeterminateCommitter struct{ calls atomic.Int64 }

func (c *combatIndeterminateCommitter) Commit(context.Context, nest.CommitRecord) error {
	c.calls.Add(1)
	return fmt.Errorf("%w: simulated WAL outcome unknown", nest.ErrCommitIndeterminate)
}

func TestHostAdapterApplyInMemoryHandlerFencesOnIndeterminateOutcome(t *testing.T) {
	getter := newTestGetter()
	attacker, attackerID := newCombatTestEntity(t, 9101)
	defender, defenderID := newCombatTestEntity(t, 9102)
	getter.Add(attacker)
	getter.Add(defender)
	attacker.component.dao.combatant = combat.Combatant{Alive: true, Health: 80, MaxHealth: 80}
	defender.component.dao.combatant = combat.Combatant{Alive: true, Health: 100, MaxHealth: 100}
	committer := &combatIndeterminateCommitter{}
	adapter := &HostAdapter{
		Resolver:  mapResolver{1: attacker.component, 2: defender.component},
		Revision:  &testRevision{},
		Committer: committer,
	}
	engine := nest.NewEngine(nest.NestOptionWithGetter(getter), nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 16}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{}))
	name := nest.NewHandlerName("b04_combat_apply_in_memory_handler")
	var attempts atomic.Int64
	var applyErr, fencedInHandler error
	var hadTx bool
	engine.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...nest.HandlerOption) (any, error) {
		attempts.Add(1)
		hadTx = nest.CurrentRollbackTx() != nil
		_, _, applyErr = adapter.Apply(skill.EffectCommand{Payload: skill.DamageCommand{Source: 1, Target: 2, Amount: 10}})
		fencedInHandler = engine.FenceError() // 业务吞掉结果未知
		return "ok", nil
	}, nest.HandlerMeta{})
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = engine.Shutdown(context.Background()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := engine.RequestMulti(ctx, name, []int64{defenderID, attackerID}, nil); err != nil {
		t.Fatalf("first request: %v", err)
	}
	if hadTx || committer.calls.Load() != 1 || !errors.Is(applyErr, nest.ErrCommitIndeterminate) {
		t.Fatalf("premise: memory handler had RollbackTx=%v, detached commits=%d, Apply err=%v; want no tx, one indeterminate detached commit", hadTx, committer.calls.Load(), applyErr)
	}
	if !errors.Is(fencedInHandler, nest.ErrNestFenced) || !errors.Is(fencedInHandler, nest.ErrCommitIndeterminate) {
		t.Fatalf("FenceError=%v when HostAdapter.Apply returned an indeterminate outcome inside a memory handler: want ErrNestFenced + ErrCommitIndeterminate (RR-76)", fencedInHandler)
	}
	if _, err := engine.RequestMulti(ctx, name, []int64{defenderID, attackerID}, nil); !errors.Is(err, nest.ErrNestFenced) || attempts.Load() != 1 {
		t.Fatalf("second request err=%v attempts=%d: a fenced engine must refuse new work", err, attempts.Load())
	}
}
