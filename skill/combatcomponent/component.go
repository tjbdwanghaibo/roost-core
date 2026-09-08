// Package combatcomponent wires the combat content battery into roost-core
// entities: the CombatDao holds the authoritative combat state behind a
// dataengine.Tracker, and the CombatComponent exposes mutators that are
// transaction-safe inside nest handlers — every mutation records its inverse
// with nest.RecordUndo and marks field-level dirty bits, so a rolled-back
// handler leaves the entity byte-identical and persistence sees exactly what
// committed.
//
// The component is deliberately owner-agnostic: generated entity factories
// construct the DAO, register it with the entity's DaoManager (it implements
// entity.DaoInterface, the nest dirty-tracker contract, and
// entity.PersistedDaoLoader), and hand it to NewCombatComponent.
package combatcomponent

import (
	"encoding/json"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/tjbdwanghaibo/roost-skill/combat"
)

// CollectionName is the default persistence collection for combat state.
const CollectionName = "combat_state"

// combatSchemaVersion versions the persisted payload for forward migration.
const combatSchemaVersion uint32 = 2

// Field-level dirty-mask bits.
const (
	FieldVitals     uint64 = 1 << 0 // health, shield, life state, avoidance facts
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
// to an entity. All mutators must run inside a nest handler (they record
// undo operations); reads are safe anywhere the entity lock is held.
type CombatComponent struct {
	dao *CombatDao
}

func NewCombatComponent(dao *CombatDao) *CombatComponent { return &CombatComponent{dao: dao} }

func (component *CombatComponent) Name() string                                           { return "combat" }
func (component *CombatComponent) OnInitFinish(_ *entity.EntityCreateParam, _ bool) error { return nil }
func (component *CombatComponent) OnDestroy(_ entity.EntityDestroyReason)                 {}
func (component *CombatComponent) Dao() *CombatDao                                        { return component.dao }

// Combatant returns a copy of the vitals block.
func (component *CombatComponent) Combatant() combat.Combatant { return component.dao.combatant }

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

func (component *CombatComponent) markDirty(mask uint64) {
	if err := nest.MarkPersist(component.dao, mask); err != nil {
		panic(fmt.Errorf("combatcomponent: mark persistence: %w", err))
	}
	component.dao.tracker.MarkSync(mask)
}

// undoVitals registers the inverse of a vitals mutation once per transaction:
// the first record per field wins, so the closure captures transaction-start
// state and later mutations in the same handler need no further records.
func (component *CombatComponent) undoVitals() {
	component.requireTransaction()
	dao := component.dao
	before := dao.combatant
	nest.RecordUndo(dao, FieldVitals, func() error {
		dao.combatant = before
		return nil
	})
}

func (component *CombatComponent) undoAttributes() {
	component.requireTransaction()
	dao := component.dao
	before := dao.attributes.BaseState()
	nest.RecordUndo(dao, FieldAttributes, func() error {
		dao.attributes.RestoreBase(before)
		return nil
	})
}

func (component *CombatComponent) undoBuffs() {
	component.requireTransaction()
	dao := component.dao
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

func (component *CombatComponent) requireTransaction() {
	if nest.CurrentRollbackTx() == nil {
		panic(fmt.Errorf("combatcomponent: persistence mutation outside transaction: %w", nest.ErrTransactionClosed))
	}
}

// InitCombatant replaces the vitals block (spawn/config load).
func (component *CombatComponent) InitCombatant(combatant combat.Combatant) {
	component.undoVitals()
	component.dao.combatant = combatant
	component.markDirty(FieldVitals)
}

// SetAttributeBase sets an attribute's base value.
func (component *CombatComponent) SetAttributeBase(id combat.AttributeID, value int64) {
	component.undoAttributes()
	component.dao.attributes.SetBase(id, value)
	component.markDirty(FieldAttributes)
}

// SetAttributeBounds sets an attribute's clamp bounds.
func (component *CombatComponent) SetAttributeBounds(id combat.AttributeID, bounds combat.AttributeBounds) {
	component.undoAttributes()
	component.dao.attributes.SetBounds(id, bounds)
	component.markDirty(FieldAttributes)
}

// ApplyBuff applies a buff at the given tick.
func (component *CombatComponent) ApplyBuff(spec combat.BuffSpec, tick, source int64) (combat.BuffInstanceID, combat.BuffApplyOutcome) {
	component.undoBuffs()
	id, outcome := component.dao.buffs.Apply(spec, tick, source)
	if outcome != combat.BuffBlockedImmune {
		component.markDirty(FieldBuffs)
	}
	return id, outcome
}

// RemoveBuff drops one buff instance by id.
func (component *CombatComponent) RemoveBuff(id combat.BuffInstanceID) (combat.BuffInstance, bool) {
	component.undoBuffs()
	instance, removed := component.dao.buffs.Remove(id)
	if removed {
		component.markDirty(FieldBuffs)
	}
	return instance, removed
}

// SetBuffStacks pins a buff instance's stack count (zero removes it).
func (component *CombatComponent) SetBuffStacks(id combat.BuffInstanceID, stacks int64) (combat.BuffInstance, bool) {
	component.undoBuffs()
	instance, ok := component.dao.buffs.SetStacks(id, stacks)
	if ok {
		component.markDirty(FieldBuffs)
	}
	return instance, ok
}

// SetBuffDueTick pins a buff instance's expiry.
func (component *CombatComponent) SetBuffDueTick(id combat.BuffInstanceID, dueTick int64) (combat.BuffInstance, bool) {
	component.undoBuffs()
	instance, ok := component.dao.buffs.SetDueTick(id, dueTick)
	if ok {
		component.markDirty(FieldBuffs)
	}
	return instance, ok
}

// AdoptBuff injects a copied or transferred instance under a fresh id.
func (component *CombatComponent) AdoptBuff(instance combat.BuffInstance) combat.BuffInstanceID {
	component.undoBuffs()
	id := component.dao.buffs.Adopt(instance)
	component.markDirty(FieldBuffs)
	return id
}

// DispelBuffs removes up to limit buffs carrying the tag, newest first.
func (component *CombatComponent) DispelBuffs(tag combat.Tag, limit int) []combat.BuffInstance {
	component.undoBuffs()
	removed := component.dao.buffs.Dispel(tag, limit)
	if len(removed) > 0 {
		component.markDirty(FieldBuffs)
	}
	return removed
}

// TickBuffs expires due buffs and returns them.
func (component *CombatComponent) TickBuffs(now int64) []combat.BuffInstance {
	component.undoBuffs()
	expired := component.dao.buffs.Tick(now)
	if len(expired) > 0 {
		component.markDirty(FieldBuffs)
	}
	return expired
}

// ApplyDamage runs one damage instance against this component. A nil source
// means world-sourced damage. Both sides' vitals are undo-protected.
func (component *CombatComponent) ApplyDamage(source *CombatComponent, input combat.DamageInput, hooks combat.Hooks) (combat.DamageOutcome, bool) {
	component.undoVitals()
	var sourceCombatant *combat.Combatant
	if source != nil {
		source.undoVitals()
		sourceCombatant = &source.dao.combatant
	}
	outcome, ok := combat.ResolveDamage(sourceCombatant, &component.dao.combatant, input, hooks)
	if !ok {
		return outcome, false
	}
	component.markDirty(FieldVitals)
	if source != nil && outcome.VampHeal > 0 {
		source.markDirty(FieldVitals)
	}
	return outcome, true
}

// Heal applies a heal capped at missing health.
func (component *CombatComponent) Heal(amount int64) (combat.HealOutcome, bool) {
	component.undoVitals()
	outcome, ok := combat.ResolveHeal(&component.dao.combatant, amount)
	if ok && outcome.Effective > 0 {
		component.markDirty(FieldVitals)
	}
	return outcome, ok
}

// AddShield grants shield points.
func (component *CombatComponent) AddShield(amount int64) (int64, bool) {
	component.undoVitals()
	added, ok := combat.AddShield(&component.dao.combatant, amount)
	if ok && added > 0 {
		component.markDirty(FieldVitals)
	}
	return added, ok
}
