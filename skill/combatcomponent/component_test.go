package combatcomponent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/nest"

	"github.com/tjbdwanghaibo/roost-skill/combat"
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

	engine := nest.NewEngine(
		nest.NestOptionWithGetter(getter),
		nest.NestOptionWithWorkerNumAndMsgCap(1, 1, 64),
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
	nest.MustRegisterHandlerWithMeta(nest.NewHandlerName("combat_seed"), seed, nest.HandlerMeta{Rollback: nest.RollbackUndo})
	nest.MustRegisterHandlerWithMeta(nest.NewHandlerName("combat_mutate_fail"), mutate, nest.HandlerMeta{Rollback: nest.RollbackUndo})

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
	engine := nest.NewEngine(
		nest.NestOptionWithGetter(getter),
		nest.NestOptionWithWorkerNumAndMsgCap(1, 1, 64),
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
	}, nest.HandlerMeta{Rollback: nest.RollbackUndo})
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
		nest.NestOptionWithWorkerNumAndMsgCap(1, 1, 64),
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
