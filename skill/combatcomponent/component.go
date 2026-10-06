// Package combatcomponent wires the combat content battery into roost-core
// entities: the CombatDao holds the authoritative combat state behind a
// dataengine.Tracker, and the CombatComponent exposes mutators that are
// transaction-safe inside nest handlers. Every mutation goes through the DAO:
// the DAO records the inverse of its own fields (rollback=undo) or is
// restored from its snapshot (rollback=state) and marks field-level dirty
// bits, so a rolled-back handler leaves the entity byte-identical and
// persistence sees exactly what committed. The component registers no undo
// of its own: rollback is the DAO's rollback (A1, maintainer 2026-10-05).
//
// The component is deliberately owner-agnostic: generated entity factories
// construct the DAO, register it with the entity's DaoManager (it implements
// entity.DaoInterface, the nest dirty-tracker contract, and
// entity.PersistedDaoLoader), and hand it to NewCombatComponent.
//
// 属性 → 伤害字段的投影交给业务（维护者第十二轮决定，skill O2）：伤害管线读 Combatant 的
// 平铺字段（Armor、DamageTakenBP…），buff 与属性修饰只改 AttributeSet。业务用
// ProjectAttributes 给出“哪个属性写到哪个字段”，组件在每次改了属性来源（属性 base、buff）
// 的同一事务里把投影写进 DAO 的 vitals，回滚随 DAO 一起恢复（A1 的派生值规则）。
package combatcomponent

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/tjbdwanghaibo/roost-core/skill/combat"
)

// CollectionName is the default persistence collection for combat state.
const CollectionName = "combat_state"

// combatSchemaVersion versions the persisted payload for forward migration.
const combatSchemaVersion uint32 = 2

// Field-level dirty-mask bits.
const (
	FieldVitals     uint64 = 1 << 0 // the Combatant block: health, shield, life state, avoidance facts, and the fields ProjectAttributes writes
	FieldAttributes uint64 = 1 << 1 // attribute base values and bounds
	FieldBuffs      uint64 = 1 << 2 // buff container (and its attribute grants)
)

// CombatDao is the persistence-facing holder of an entity's combat state.
type CombatDao struct {
	id      int64
	dbName  string
	coll    string
	tracker dataengine.Tracker

	combatant  combat.Combatant
	attributes *combat.AttributeSet
	buffs      *combat.BuffContainer
}

// NewCombatDao builds an empty DAO for the entity's storage id. The dbName
// is the logical database the storage service resolves ("game" by default in
// generated factories).
func NewCombatDao(id int64, dbName string) *CombatDao {
	dao := &CombatDao{id: id, dbName: dbName, coll: CollectionName, attributes: combat.NewAttributeSet(), buffs: combat.NewBuffContainer()}
	dao.buffs.LinkAttributes(dao.attributes)
	return dao
}

func (dao *CombatDao) Id() int64                         { return dao.id }
func (dao *CombatDao) SetId(id int64)                    { dao.id = id }
func (dao *CombatDao) DbName() string                    { return dao.dbName }
func (dao *CombatDao) CollName() string                  { return dao.coll }
func (dao *CombatDao) Dirty() entity.IDirty              { return &dao.tracker }
func (dao *CombatDao) CleanDirty()                       { dao.tracker.SelfClean() }
func (dao *CombatDao) DirtyTracker() *dataengine.Tracker { return &dao.tracker }

type persistedCombatState struct {
	ID         int64                       `json:"-" bson:"_id"`
	Combatant  combat.Combatant            `json:"combatant" bson:"combatant"`
	Attributes []combat.AttributeBaseState `json:"attributes,omitempty" bson:"attributes,omitempty"`
	Buffs      combat.BuffContainerState   `json:"buffs" bson:"buffs"`
}

// MarshalPersisted serializes the full combat state for storage.
func (dao *CombatDao) MarshalPersisted() ([]byte, uint32, error) {
	payload, err := bson.Marshal(dao.persistedState())
	if err != nil {
		return nil, 0, err
	}
	return payload, combatSchemaVersion, nil
}

// RestorePersisted implements entity.PersistedDaoLoader.
func (dao *CombatDao) RestorePersisted(raw []byte, schemaVersion uint32, version uint64) error {
	if schemaVersion > combatSchemaVersion {
		return fmt.Errorf("combatcomponent: schema version %d is newer than supported %d", schemaVersion, combatSchemaVersion)
	}
	if schemaVersion < combatSchemaVersion {
		migrated, err := dao.Migrate(raw, schemaVersion)
		if err != nil {
			return err
		}
		raw = migrated
	}
	var state persistedCombatState
	if err := bson.Unmarshal(raw, &state); err != nil {
		return fmt.Errorf("combatcomponent: decode persisted BSON: %w", err)
	}
	if state.ID != 0 && state.ID != dao.id {
		return fmt.Errorf("combatcomponent: persisted id %d does not match dao id %d", state.ID, dao.id)
	}
	if err := dao.applyState(state); err != nil {
		return err
	}
	dao.tracker.SetVersion(version)
	dao.tracker.SelfClean()
	return nil
}

func (*CombatDao) SchemaVersion() uint32 { return combatSchemaVersion }

// Migrate converts the legacy schema-1 JSON payload into the BSON document
// owned by Data Engine. Later schema steps must be added explicitly.
func (dao *CombatDao) Migrate(raw []byte, from uint32) ([]byte, error) {
	if from != 1 || combatSchemaVersion != 2 {
		return nil, fmt.Errorf("combatcomponent: unsupported migration %d -> %d", from, combatSchemaVersion)
	}
	var state persistedCombatState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("combatcomponent: decode legacy JSON: %w", err)
	}
	state.ID = dao.id
	return bson.Marshal(state)
}

func (dao *CombatDao) PrepareMutation(change nest.PersistChange) (dataengine.Mutation, error) {
	version := dao.tracker.Version()
	mutation := dataengine.Mutation{
		Key:  dataengine.DocumentKey{Database: dao.DbName(), Resource: dao.CollName(), ID: dao.Id()},
		Kind: dataengine.MutationPatch, ExpectedVersion: version, NextVersion: version + 1,
		Mask: change.Mask, Schema: combatSchemaVersion, Codec: "bson-v2",
	}
	if change.Delete {
		mutation.Kind = dataengine.MutationDelete
		return mutation, nil
	}
	if version == 0 || change.Mask == dataengine.AllFields {
		payload, _, err := dao.MarshalPersisted()
		if err != nil {
			return dataengine.Mutation{}, err
		}
		mutation.Kind = dataengine.MutationPut
		mutation.Data = payload
		return mutation, nil
	}
	set := bson.M{}
	if change.Mask&FieldVitals != 0 {
		set["combatant"] = dao.combatant
	}
	if change.Mask&FieldAttributes != 0 {
		set["attributes"] = dao.attributes.BaseState()
	}
	if change.Mask&FieldBuffs != 0 {
		set["buffs"] = dao.buffs.State()
	}
	if len(set) == 0 {
		return dataengine.Mutation{}, fmt.Errorf("combatcomponent: persistence mask %#x has no fields", change.Mask)
	}
	patch, err := bson.Marshal(set)
	if err != nil {
		return dataengine.Mutation{}, err
	}
	mutation.Patch = dataengine.FieldPatch{SetBSON: patch}
	return mutation, nil
}

func (dao *CombatDao) AcceptMutation(mutation dataengine.Mutation) error {
	return dao.tracker.AcceptVersion(mutation.ExpectedVersion, mutation.NextVersion)
}

// CaptureRollbackState and RestoreRollbackState implement the nest
// state-rollback contract for RollbackState-policy handlers.
func (dao *CombatDao) CaptureRollbackState() ([]byte, error) {
	return json.Marshal(dao.persistedState())
}

func (dao *CombatDao) RestoreRollbackState(raw []byte) error { return dao.restoreState(raw) }

func (dao *CombatDao) restoreState(raw []byte) error {
	var state persistedCombatState
	if err := json.Unmarshal(raw, &state); err != nil {
		return fmt.Errorf("combatcomponent: decode combat state: %w", err)
	}
	return dao.applyState(state)
}

func (dao *CombatDao) persistedState() persistedCombatState {
	return persistedCombatState{ID: dao.id, Combatant: dao.combatant, Attributes: dao.attributes.BaseState(), Buffs: dao.buffs.State()}
}

func (dao *CombatDao) applyState(state persistedCombatState) error {
	buffs, err := combat.RestoreBuffContainer(state.Buffs)
	if err != nil {
		return fmt.Errorf("combatcomponent: restore buffs: %w", err)
	}
	dao.combatant = state.Combatant
	dao.attributes = combat.NewAttributeSet()
	dao.attributes.RestoreBase(state.Attributes)
	dao.buffs = buffs
	dao.buffs.LinkAttributes(dao.attributes)
	return nil
}

// CombatComponent is the behavior wrapper generated entity factories attach
// to an entity. Its only state is its DAO; the optional attribute projection
// is business code, not state. All mutators must run inside a nest
// transaction: the DAO joins each changed field to it (its own inverse under
// rollback=undo, the snapshot under rollback=state), and a mutator called
// outside one panics before changing anything. ProjectAttributes is the one
// exception — it may be installed at construction, outside a transaction.
// Reads are safe anywhere the entity lock is held.
type CombatComponent struct {
	dao        *CombatDao
	projection AttributeProjection
}

// AttributeProjection 把属性当前值（base 加 buff / 属性修饰）写到伤害管线读的 Combatant
// 字段，由业务提供：哪个属性对应 Armor、MagicResistance、DamageTakenBP… 是游戏设计。
// attribute 返回属性当前值；combatant 是即将写回 DAO 的 vitals 副本，投影只写由属性决定
// 的字段，不要改 Health / Shield / Alive 这类战斗过程状态。投影必须是纯函数：同样的
// 属性值写出同样的字段，不读外部可变状态。
type AttributeProjection func(attribute func(combat.AttributeID) int64, combatant *combat.Combatant)

func NewCombatComponent(dao *CombatDao) *CombatComponent { return &CombatComponent{dao: dao} }

func (component *CombatComponent) Name() string { return "combat" }

// OnInitFinish 在实体建好（新建或从存储加载）后投影一次：存储里的 vitals 是上次提交时
// 的投影结果，投影函数可能已随版本改变（A1 的“加载”触发点）。
func (component *CombatComponent) OnInitFinish(_ *entity.EntityCreateParam, _ bool) error {
	component.deriveProjection()
	return nil
}
func (component *CombatComponent) OnDestroy(_ entity.EntityDestroyReason) {}
func (component *CombatComponent) Dao() *CombatDao                        { return component.dao }

// ProjectAttributes 安装属性投影并立刻投影一次。在实体工厂里构造组件后调用一次即可；
// 之后组件在每个改属性来源的 mutator（InitCombatant、SetAttributeBase / Bounds、
// ApplyBuff、RemoveBuff、SetBuffStacks、AdoptBuff、DispelBuffs、TickBuffs）末尾、同一事务里
// 重新投影，伤害读到的字段总是当前属性的结果。nil 卸下投影，已写的字段保持原值。
func (component *CombatComponent) ProjectAttributes(projection AttributeProjection) {
	component.projection = projection
	component.deriveProjection()
}

// deriveProjection 是写投影字段的唯一入口（A1：派生值是 DAO 字段，由组件里唯一的 derive
// 在加载与改源字段的事务里写；回滚不是触发点）。投影写在 vitals 上：事务里经
// beginChange / markChanged，与源字段同一笔逆操作 / 快照，handler 失败或提交被拒时随
// DAO 回到事务开始时的值，不需要重算。不在事务里（加载、构造时安装）直接写内存，不登记
// 逆操作、不标脏——与存储里的值只在投影函数变了时不同，下一次 vitals 提交写回。
// 结果与现值相同时什么都不做，不产生持久写。
func (component *CombatComponent) deriveProjection() {
	if component.projection == nil {
		return
	}
	dao := component.dao
	projected := cloneCombatant(dao.combatant)
	component.projection(dao.attributes.Current, &projected)
	if reflect.DeepEqual(projected, dao.combatant) {
		return
	}
	if nest.CurrentRollbackTx() == nil {
		dao.combatant = projected
		return
	}
	dao.beginChange(FieldVitals)
	dao.combatant = projected
	dao.markChanged(FieldVitals)
}

// Combatant returns a copy of the vitals block. The element multiplier map is
// copied too: a shared map would let callers change authoritative state
// outside the transaction, undo, and dirty tracking (RR-20261005-NC-113).
func (component *CombatComponent) Combatant() combat.Combatant {
	return cloneCombatant(component.dao.combatant)
}

// cloneCombatant copies the only reference-typed field of the vitals block.
// The DAO never mutates ElementMultipliersBP in place, so every value that
// crosses the component boundary owning its own map keeps the stored map
// immutable — which also keeps beginChange's shallow "before" copy of the vitals exact.
func cloneCombatant(combatant combat.Combatant) combat.Combatant {
	if combatant.ElementMultipliersBP != nil {
		multipliers := make(map[combat.Element]int64, len(combatant.ElementMultipliersBP))
		for element, value := range combatant.ElementMultipliersBP {
			multipliers[element] = value
		}
		combatant.ElementMultipliersBP = multipliers
	}
	return combatant
}

// AttributeCurrent resolves an attribute's effective value.
func (component *CombatComponent) AttributeCurrent(id combat.AttributeID) int64 {
	return component.dao.attributes.Current(id)
}

// AttributeBase reads an attribute's base value.
func (component *CombatComponent) AttributeBase(id combat.AttributeID) int64 {
	return component.dao.attributes.Base(id)
}

// ActiveBuffs lists the live buff instances in application order.
func (component *CombatComponent) ActiveBuffs() []combat.BuffInstance {
	return component.dao.buffs.Active()
}

// HasBuffTag reports whether any active buff carries the tag.
func (component *CombatComponent) HasBuffTag(tag combat.Tag) bool {
	return component.dao.buffs.HasTag(tag)
}

// beginChange makes the fields in mask part of the running transaction before
// the first change to them. It is the DAO's own half of every mutation, the
// same shape as a generated DAO setter: the DAO records the inverse of its
// own state, so the component that drives it registers nothing (A1,
// maintainer 2026-10-05: rollback is the DAO's rollback;
// docs/feature/REFACTOR-2026-10-05-dao-unified-rollback.md). Under
// rollback=undo each field records once per transaction (Nest keeps the
// first inverse per DAO and field), so the inverse restores transaction-start
// state however many mutations follow; under rollback=state the DAO snapshot
// covers it and nothing is recorded. Outside a transaction it panics before
// anything changes.
func (dao *CombatDao) beginChange(mask uint64) {
	if nest.CurrentRollbackTx() == nil {
		panic(fmt.Errorf("combatcomponent: persistence mutation outside transaction: %w", nest.ErrTransactionClosed))
	}
	if mask&FieldVitals != 0 {
		before := dao.combatant
		nest.RecordUndo(dao, FieldVitals, func() error {
			dao.combatant = before
			return nil
		})
	}
	if mask&FieldAttributes != 0 {
		before := dao.attributes.BaseState()
		nest.RecordUndo(dao, FieldAttributes, func() error {
			dao.attributes.RestoreBase(before)
			return nil
		})
	}
	if mask&FieldBuffs != 0 {
		before := dao.buffs.State()
		nest.RecordUndo(dao, FieldBuffs, func() error {
			// Revoke the grants of whatever is active now, then rebuild the
			// container; relinking re-grants the restored instances.
			restored, err := combat.RestoreBuffContainer(before)
			if err != nil {
				// before is a State() of a live container, which satisfies the
				// invariants by construction; failing here means memory corruption.
				return err
			}
			for _, instance := range dao.buffs.Active() {
				dao.attributes.Revoke(combat.ModifierHandle(instance.Instance))
			}
			dao.buffs = restored
			dao.buffs.LinkAttributes(dao.attributes)
			return nil
		})
	}
}

// markChanged marks the fields in mask for persistence and replication.
func (dao *CombatDao) markChanged(mask uint64) {
	if err := nest.MarkPersist(dao, mask); err != nil {
		panic(fmt.Errorf("combatcomponent: mark persistence: %w", err))
	}
	dao.tracker.MarkSync(mask)
}

// InitCombatant replaces the vitals block (spawn/config load). The caller's
// element multiplier map is copied, so a template shared across entities
// stays independent of every stored combatant (RR-20261005-NC-113).
func (component *CombatComponent) InitCombatant(combatant combat.Combatant) {
	component.dao.beginChange(FieldVitals)
	component.dao.combatant = cloneCombatant(combatant)
	component.dao.markChanged(FieldVitals)
	component.deriveProjection()
}

// SetAttributeBase sets an attribute's base value.
func (component *CombatComponent) SetAttributeBase(id combat.AttributeID, value int64) {
	component.dao.beginChange(FieldAttributes)
	component.dao.attributes.SetBase(id, value)
	component.dao.markChanged(FieldAttributes)
	component.deriveProjection()
}

// SetAttributeBounds sets an attribute's clamp bounds.
func (component *CombatComponent) SetAttributeBounds(id combat.AttributeID, bounds combat.AttributeBounds) {
	component.dao.beginChange(FieldAttributes)
	component.dao.attributes.SetBounds(id, bounds)
	component.dao.markChanged(FieldAttributes)
	component.deriveProjection()
}

// ApplyBuff applies a buff at the given tick.
func (component *CombatComponent) ApplyBuff(spec combat.BuffSpec, tick, source int64) (combat.BuffInstanceID, combat.BuffApplyOutcome) {
	component.dao.beginChange(FieldBuffs)
	id, outcome := component.dao.buffs.Apply(spec, tick, source)
	if outcome != combat.BuffBlockedImmune {
		component.dao.markChanged(FieldBuffs)
		component.deriveProjection()
	}
	return id, outcome
}

// RemoveBuff drops one buff instance by id.
func (component *CombatComponent) RemoveBuff(id combat.BuffInstanceID) (combat.BuffInstance, bool) {
	component.dao.beginChange(FieldBuffs)
	instance, removed := component.dao.buffs.Remove(id)
	if removed {
		component.dao.markChanged(FieldBuffs)
		component.deriveProjection()
	}
	return instance, removed
}

// SetBuffStacks pins a buff instance's stack count (zero removes it).
func (component *CombatComponent) SetBuffStacks(id combat.BuffInstanceID, stacks int64) (combat.BuffInstance, bool) {
	component.dao.beginChange(FieldBuffs)
	instance, ok := component.dao.buffs.SetStacks(id, stacks)
	if ok {
		component.dao.markChanged(FieldBuffs)
		component.deriveProjection()
	}
	return instance, ok
}

// SetBuffDueTick pins a buff instance's expiry. Expiry does not change
// attributes, so there is nothing to re-project.
func (component *CombatComponent) SetBuffDueTick(id combat.BuffInstanceID, dueTick int64) (combat.BuffInstance, bool) {
	component.dao.beginChange(FieldBuffs)
	instance, ok := component.dao.buffs.SetDueTick(id, dueTick)
	if ok {
		component.dao.markChanged(FieldBuffs)
	}
	return instance, ok
}

// AdoptBuff injects a copied or transferred instance under a fresh id.
func (component *CombatComponent) AdoptBuff(instance combat.BuffInstance) combat.BuffInstanceID {
	component.dao.beginChange(FieldBuffs)
	id := component.dao.buffs.Adopt(instance)
	component.dao.markChanged(FieldBuffs)
	component.deriveProjection()
	return id
}

// DispelBuffs removes up to limit buffs carrying the tag, newest first.
func (component *CombatComponent) DispelBuffs(tag combat.Tag, limit int) []combat.BuffInstance {
	component.dao.beginChange(FieldBuffs)
	removed := component.dao.buffs.Dispel(tag, limit)
	if len(removed) > 0 {
		component.dao.markChanged(FieldBuffs)
		component.deriveProjection()
	}
	return removed
}

// TickBuffs expires due buffs and returns them.
func (component *CombatComponent) TickBuffs(now int64) []combat.BuffInstance {
	component.dao.beginChange(FieldBuffs)
	expired := component.dao.buffs.Tick(now)
	if len(expired) > 0 {
		component.dao.markChanged(FieldBuffs)
		component.deriveProjection()
	}
	return expired
}

// ApplyDamage runs one damage instance against this component. A nil source
// means world-sourced damage. Both sides' vitals join the transaction before
// the pipeline runs; the target is marked dirty when the damage resolves, the
// source only when a vampiric heal changed it. Damage changes no attribute, so
// nothing is re-projected.
func (component *CombatComponent) ApplyDamage(source *CombatComponent, input combat.DamageInput, hooks combat.Hooks) (combat.DamageOutcome, bool) {
	component.dao.beginChange(FieldVitals)
	var sourceCombatant *combat.Combatant
	if source != nil {
		source.dao.beginChange(FieldVitals)
		sourceCombatant = &source.dao.combatant
	}
	outcome, ok := combat.ResolveDamage(sourceCombatant, &component.dao.combatant, input, hooks)
	if !ok {
		return outcome, false
	}
	component.dao.markChanged(FieldVitals)
	if source != nil && outcome.VampHeal > 0 {
		source.dao.markChanged(FieldVitals)
	}
	return outcome, true
}

// Heal applies a heal capped at missing health.
func (component *CombatComponent) Heal(amount int64) (combat.HealOutcome, bool) {
	component.dao.beginChange(FieldVitals)
	outcome, ok := combat.ResolveHeal(&component.dao.combatant, amount)
	if ok && outcome.Effective > 0 {
		component.dao.markChanged(FieldVitals)
	}
	return outcome, ok
}

// AddShield grants shield points.
func (component *CombatComponent) AddShield(amount int64) (int64, bool) {
	component.dao.beginChange(FieldVitals)
	added, ok := combat.AddShield(&component.dao.combatant, amount)
	if ok && added > 0 {
		component.dao.markChanged(FieldVitals)
	}
	return added, ok
}
