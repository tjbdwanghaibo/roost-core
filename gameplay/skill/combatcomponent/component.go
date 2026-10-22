// Package combatcomponent 把战斗内容组件接进 roost-core 实体：CombatDao 在 dataengine.Tracker 背后持有权威战斗
// 状态，CombatComponent 提供在 nest handler 内事务安全的修改方法。每次修改都经过 DAO：DAO 记录自己字段的逆操作
// （rollback=undo）或从快照恢复（rollback=state），并标记字段级脏位；回滚后的 handler 让实体逐字节恢复原样，
// 持久化看到的正好是提交的内容。组件自己不登记任何撤销：回滚就是 DAO 的回滚。
//
// 组件刻意与拥有者无关：生成的实体工厂构造 DAO，把它注册到实体的 DaoManager（它实现 entity.DaoInterface、
// nest 脏标记契约与 entity.PersistedDaoLoader），再交给 NewCombatComponent。
//
// 属性 → 伤害字段的投影交给业务：伤害管线读 Combatant 的平铺字段（Armor、DamageTakenBP…），buff 与属性修饰只改
// AttributeSet。业务用 ProjectAttributes 给出“哪个属性写到哪个字段”，组件在每次改了属性来源（属性 base、buff）的
// 同一事务里把投影写进 DAO 的 vitals，回滚随 DAO 一起恢复（派生值规则）。
package combatcomponent

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/tjbdwanghaibo/roost-core/gameplay/skill/combat"
)

// CollectionName 是战斗状态默认的持久化集合。
const CollectionName = "combat_state"

// combatSchemaVersion 是持久化载荷的版本，用于向前迁移。
const combatSchemaVersion uint32 = 2

// 字段级脏位。
const (
	FieldVitals     uint64 = 1 << 0 // Combatant 块：生命、护盾、存活状态、闪避事实，以及 ProjectAttributes 写的字段
	FieldAttributes uint64 = 1 << 1 // 属性基础值与上下界
	FieldBuffs      uint64 = 1 << 2 // buff 容器（及其属性授予）
)

// CombatDao 是面向持久化的实体战斗状态持有者。
type CombatDao struct {
	id      int64
	dbName  string
	coll    string
	tracker dataengine.Tracker

	combatant  combat.Combatant
	attributes *combat.AttributeSet
	buffs      *combat.BuffContainer
}

// NewCombatDao 为实体的存储 ID 构造空 DAO。dbName 是存储服务解析的逻辑库名（生成的工厂默认 "game"）。
func NewCombatDao(id int64, dbName string) *CombatDao {
	dao := &CombatDao{id: id, dbName: dbName, coll: CollectionName, attributes: combat.NewAttributeSet(), buffs: combat.NewBuffContainer()}
	dao.buffs.LinkAttributes(dao.attributes)
	return dao
}

// 以下访问器实现 entity.DaoInterface 与 nest 脏标记契约。
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

// MarshalPersisted 序列化完整的战斗状态以供存储。
func (dao *CombatDao) MarshalPersisted() ([]byte, uint32, error) {
	payload, err := bson.Marshal(dao.persistedState())
	if err != nil {
		return nil, 0, err
	}
	return payload, combatSchemaVersion, nil
}

// RestorePersisted 实现 entity.PersistedDaoLoader。
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

// SchemaVersion 返回持久化载荷的当前 schema 版本。
func (*CombatDao) SchemaVersion() uint32 { return combatSchemaVersion }

// Migrate 把旧的 schema-1 JSON 载荷转换成 Data Engine 持有的 BSON 文档。之后的 schema 步骤必须显式添加。
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

// PrepareMutation 按本次变更的字段掩码生成带期望版本的补丁写入（bson-v2 编码）。
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

// AcceptMutation 在写入被存储确认后推进 tracker 的版本。
func (dao *CombatDao) AcceptMutation(mutation dataengine.Mutation) error {
	return dao.tracker.AcceptVersion(mutation.ExpectedVersion, mutation.NextVersion)
}

// CaptureRollbackState 与 RestoreRollbackState 为 RollbackState 策略的 handler 实现 nest 状态回滚契约。
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

// CombatComponent 是生成的实体工厂挂到实体上的行为包装。它唯一的状态是 DAO；可选的属性投影是业务代码，不是状态。
// 所有修改方法都必须在 nest 事务内运行：DAO 把每个改动的字段加入事务（rollback=undo 下是它自己的逆操作，
// rollback=state 下是快照），在事务外调用时在改动任何东西之前 panic。ProjectAttributes 是唯一的例外——可以在构造时、
// 事务之外安装。只要持有实体锁，读取在哪里都安全。
type CombatComponent struct {
	dao        *CombatDao
	projection AttributeProjection
	// buffStatesChanged 由 BindStates 安装：buff 集合变化后通知派生状态（D21 S6）。是接线，不是状态。
	buffStatesChanged func()
	// drive 由 DriveBuffs（或心跳驱动的 AttachBeat）安装：buff 推进的驱动接线（D20 P5，buff_stepper.go）。是装配期
	// 接线（构造时写一次、事务里不改），不是事务状态；驱动启停的状态归驱动自己（心跳登记的事务语义由 heartbeat.Reg
	// 管理）。glsvet 的字段写提示按语法认不出接线，用 roost:cache 豁免。
	drive *buffDrive //roost:cache
}

// AttributeProjection 把属性当前值（base 加 buff / 属性修饰）写到伤害管线读的 Combatant
// 字段，由业务提供：哪个属性对应 Armor、MagicResistance、DamageTakenBP… 是游戏设计。
// attribute 返回属性当前值；combatant 是即将写回 DAO 的 vitals 副本，投影只写由属性决定
// 的字段，不要改 Health / Shield / Alive 这类战斗过程状态。投影必须是纯函数：同样的
// 属性值写出同样的字段，不读外部可变状态。
type AttributeProjection func(attribute func(combat.AttributeID) int64, combatant *combat.Combatant)

// NewCombatComponent 用已注册到实体 DaoManager 的 DAO 构造组件。
func NewCombatComponent(dao *CombatDao) *CombatComponent { return &CombatComponent{dao: dao} }

func (component *CombatComponent) Name() string { return "combat" }

// OnInitFinish 在实体建好（新建或从存储加载）后投影一次：存储里的 vitals 是上次提交时的投影结果，投影函数可能已随
// 版本改变（“加载”触发点）。装了 buff 驱动（DriveBuffs / AttachBeat）时按加载出的 buff 通知它（心跳驱动下启动心跳），
// 停服期间到期的记录在驱动的第一步清掉。
func (component *CombatComponent) OnInitFinish(_ *entity.EntityCreateParam, _ bool) error {
	component.deriveProjection()
	component.buffsChanged()
	return nil
}

// OnDestroy 在实体离开内存（卸载、删档、撤销新建）时卸下 buff 的驱动（CloseBuffDriver：心跳驱动撤销登记、释放名字）。
// 组件由别的组件持有、不直接挂在实体上时（例如 game-demo 怪物的 ActorComponent），由持有者的 OnDestroy 转调。
func (component *CombatComponent) OnDestroy(_ entity.EntityDestroyReason) {
	component.CloseBuffDriver()
}
func (component *CombatComponent) Dao() *CombatDao { return component.dao }

// ProjectAttributes 安装属性投影并立刻投影一次。在实体工厂里构造组件后调用一次即可；
// 之后组件在每个改属性来源的 mutator（InitCombatant、SetAttributeBase / Bounds、
// ApplyBuff、RemoveBuff、SetBuffStacks、AdoptBuff、DispelBuffs、TickBuffs）末尾、同一事务里
// 重新投影，伤害读到的字段总是当前属性的结果。nil 卸下投影，已写的字段保持原值。
func (component *CombatComponent) ProjectAttributes(projection AttributeProjection) {
	component.projection = projection
	component.deriveProjection()
}

// deriveProjection 是写投影字段的唯一入口（派生值是 DAO 字段，由组件里唯一的 derive 在加载与改源字段的事务里写；
// 回滚不是触发点）。投影写在 vitals 上：事务里经 beginChange / markChanged，与源字段同一笔逆操作 / 快照，handler
// 失败或提交被拒时随 DAO 回到事务开始时的值，不需要重算。不在事务里（加载、构造时安装）直接写内存，不登记逆操作、
// 不标脏——与存储里的值只在投影函数变了时不同，下一次 vitals 提交写回。结果与现值相同时什么都不做，不产生持久写。
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

// Combatant 返回 vitals 块的副本，元素倍率 map 也复制：共享 map 会让调用方绕过事务、撤销与脏标记修改权威状态。
func (component *CombatComponent) Combatant() combat.Combatant {
	return cloneCombatant(component.dao.combatant)
}

// cloneCombatant 复制 vitals 块里唯一的引用类型字段。DAO 从不原地修改 ElementMultipliersBP，每个跨过组件边界的值都
// 持有自己的 map，存储的 map 因此不可变——这也保证 beginChange 对 vitals 做的浅拷贝“修改前”副本是精确的。
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

// AttributeCurrent 返回属性的有效值。
func (component *CombatComponent) AttributeCurrent(id combat.AttributeID) int64 {
	return component.dao.attributes.Current(id)
}

// AttributeBase 读取属性的基础值。
func (component *CombatComponent) AttributeBase(id combat.AttributeID) int64 {
	return component.dao.attributes.Base(id)
}

// ActiveBuffs 按施加顺序列出生效中的 buff 实例。
func (component *CombatComponent) ActiveBuffs() []combat.BuffInstance {
	return component.dao.buffs.Active()
}

// beginChange 在 mask 里的字段第一次改动之前把它们加入当前事务。它是每次修改里 DAO 自己的那一半，形状与生成的
// DAO setter 相同：DAO 记录自己状态的逆操作，驱动它的组件不登记任何东西（回滚就是 DAO 的回滚）。rollback=undo 下
// 每个字段每个事务只记录一次（Nest 对每个 DAO 与字段保留第一个逆操作），无论之后有多少次修改，逆操作都恢复到事务
// 开始时的状态；rollback=state 下由 DAO 快照覆盖，不记录任何东西。在事务外调用时，在改动任何东西之前 panic。
func (dao *CombatDao) beginChange(mask uint64) {
	tx := nest.CurrentRollbackTx()
	if tx == nil {
		panic(fmt.Errorf("combatcomponent: persistence mutation outside transaction: %w", nest.ErrTransactionClosed))
	}
	if tx.Policy() != nest.RollbackUndo {
		return
	}
	if mask&FieldVitals != 0 && !tx.HasUndo(dao, FieldVitals) {
		before := dao.combatant
		nest.RecordUndo(dao, FieldVitals, func() error {
			dao.combatant = before
			return nil
		})
	}
	if mask&FieldAttributes != 0 && !tx.HasUndo(dao, FieldAttributes) {
		before := dao.attributes.BaseState()
		nest.RecordUndo(dao, FieldAttributes, func() error {
			dao.attributes.RestoreBase(before)
			return nil
		})
	}
	if mask&FieldBuffs != 0 && !tx.HasUndo(dao, FieldBuffs) {
		before := dao.buffs.State()
		nest.RecordUndo(dao, FieldBuffs, func() error {
			// 先撤销当前生效 buff 的授予，再重建容器；重新关联时会重新授予恢复出的实例。
			restored, err := combat.RestoreBuffContainer(before)
			if err != nil {
				// before 是活容器的 State()，按构造满足不变量；这里失败意味着内存损坏。
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

// markChanged 标记 mask 里的字段待持久化与复制。
func (dao *CombatDao) markChanged(mask uint64) {
	if err := nest.MarkPersist(dao, mask); err != nil {
		panic(fmt.Errorf("combatcomponent: mark persistence: %w", err))
	}
	dao.tracker.MarkSync(mask)
}

// InitCombatant 替换 vitals 块（生成 / 加载配置时）。调用方的元素倍率 map 被复制，跨实体共享的模板与每个存储的
// 战斗单位相互独立。
func (component *CombatComponent) InitCombatant(combatant combat.Combatant) {
	component.dao.beginChange(FieldVitals)
	component.dao.combatant = cloneCombatant(combatant)
	component.dao.markChanged(FieldVitals)
	component.deriveProjection()
}

// SetAttributeBase 设置属性的基础值。
func (component *CombatComponent) SetAttributeBase(id combat.AttributeID, value int64) {
	component.dao.beginChange(FieldAttributes)
	component.dao.attributes.SetBase(id, value)
	component.dao.markChanged(FieldAttributes)
	component.deriveProjection()
}

// SetAttributeBounds 设置属性的夹取上下界。
func (component *CombatComponent) SetAttributeBounds(id combat.AttributeID, bounds combat.AttributeBounds) {
	component.dao.beginChange(FieldAttributes)
	component.dao.attributes.SetBounds(id, bounds)
	component.dao.markChanged(FieldAttributes)
	component.deriveProjection()
}

// ApplyBuff 在给定 tick 施加一个 buff。
func (component *CombatComponent) ApplyBuff(spec combat.BuffSpec, tick, source int64) (combat.BuffInstanceID, combat.BuffApplyOutcome) {
	component.dao.beginChange(FieldBuffs)
	id, outcome := component.dao.buffs.Apply(spec, tick, source)
	if outcome != combat.BuffBlockedImmune && outcome != combat.BuffIgnored {
		component.dao.markChanged(FieldBuffs)
		component.deriveProjection()
		component.buffsChanged()
	}
	return id, outcome
}

// RemoveBuff 按 ID 移除一个 buff 实例。
func (component *CombatComponent) RemoveBuff(id combat.BuffInstanceID) (combat.BuffInstance, bool) {
	component.dao.beginChange(FieldBuffs)
	instance, removed := component.dao.buffs.Remove(id)
	if removed {
		component.dao.markChanged(FieldBuffs)
		component.deriveProjection()
		component.buffsChanged()
	}
	return instance, removed
}

// SetBuffStacks 固定 buff 实例的层数（0 表示移除）。
func (component *CombatComponent) SetBuffStacks(id combat.BuffInstanceID, stacks int64) (combat.BuffInstance, bool) {
	component.dao.beginChange(FieldBuffs)
	instance, ok := component.dao.buffs.SetStacks(id, stacks)
	if ok {
		component.dao.markChanged(FieldBuffs)
		component.deriveProjection()
		component.buffsChanged()
	}
	return instance, ok
}

// SetBuffDueTick 固定 buff 实例的到期 tick。到期时间不影响属性，不需要重新投影；集合不变，不通知派生状态，但
// 限时与否可能变了（0 表示永久），要通知 buff 的驱动。
func (component *CombatComponent) SetBuffDueTick(id combat.BuffInstanceID, dueTick int64) (combat.BuffInstance, bool) {
	component.dao.beginChange(FieldBuffs)
	instance, ok := component.dao.buffs.SetDueTick(id, dueTick)
	if ok {
		component.dao.markChanged(FieldBuffs)
		component.alignBuffDriver()
	}
	return instance, ok
}

// AdoptBuff 以新 ID 注入一个复制或转移来的实例。
func (component *CombatComponent) AdoptBuff(instance combat.BuffInstance) combat.BuffInstanceID {
	component.dao.beginChange(FieldBuffs)
	id := component.dao.buffs.Adopt(instance)
	component.dao.markChanged(FieldBuffs)
	component.deriveProjection()
	component.buffsChanged()
	return id
}

// DispelBuffs 移除至多 limit 个带该标签的 buff，最新的先移除。
func (component *CombatComponent) DispelBuffs(tag combat.Tag, limit int) []combat.BuffInstance {
	component.dao.beginChange(FieldBuffs)
	removed := component.dao.buffs.Dispel(tag, limit)
	if len(removed) > 0 {
		component.dao.markChanged(FieldBuffs)
		component.deriveProjection()
		component.buffsChanged()
	}
	return removed
}

// TickBuffs 让到期 tick 不晚于 now 的 buff 过期并返回它们（推进一步的原语）。驱动不直接调它：驱动的一步是
// StepBuffs / StepBuffsToTick（buff_stepper.go：TickBuffs、OnStep、通知驱动），快池上的实体用心跳驱动 AttachBeat
// （D20 P5），定步长循环（Lockstep 房间）用 DriveBuffs。直接调用只用于原语本身的测试。now 与 StatusBridge.CurrentTick
// 同源。没有到期的 buff 时什么都不登记、不改。
func (component *CombatComponent) TickBuffs(now int64) []combat.BuffInstance {
	if due, ok := component.dao.buffs.NextDue(); !ok || due > now {
		return nil
	}
	component.dao.beginChange(FieldBuffs)
	expired := component.dao.buffs.Tick(now)
	if len(expired) > 0 {
		component.dao.markChanged(FieldBuffs)
		component.deriveProjection()
		component.buffsChanged()
	}
	return expired
}

// ApplyDamage 让一次伤害实例作用于这个组件。source 为 nil 表示世界来源的伤害。管线运行前双方的 vitals 都加入事务；
// 伤害结算后标记目标为脏，来源只在吸血治疗改变了它时才标脏。伤害不改属性，不需要重新投影。
func (component *CombatComponent) ApplyDamage(source *CombatComponent, input combat.DamageInput, hooks combat.Hooks) (combat.DamageOutcome, bool) {
	component.dao.beginChange(FieldVitals)
	before := component.dao.combatant
	var sourceCombatant *combat.Combatant
	if source != nil {
		source.dao.beginChange(FieldVitals)
		sourceCombatant = &source.dao.combatant
	}
	outcome, ok := combat.ResolveDamage(sourceCombatant, &component.dao.combatant, input, hooks)
	if !ok {
		return outcome, false
	}
	if after := component.dao.combatant; after.Health != before.Health || after.Shield != before.Shield || after.Alive != before.Alive || after.SpellShield != before.SpellShield {
		component.dao.markChanged(FieldVitals)
	}
	if source != nil && outcome.VampHeal > 0 {
		source.dao.markChanged(FieldVitals)
	}
	return outcome, true
}

// Heal 施加以缺失生命为上限的治疗。
func (component *CombatComponent) Heal(amount int64) (combat.HealOutcome, bool) {
	component.dao.beginChange(FieldVitals)
	outcome, ok := combat.ResolveHeal(&component.dao.combatant, amount)
	if ok && outcome.Effective > 0 {
		component.dao.markChanged(FieldVitals)
	}
	return outcome, ok
}

// AddShield 增加护盾值。
func (component *CombatComponent) AddShield(amount int64) (int64, bool) {
	component.dao.beginChange(FieldVitals)
	added, ok := combat.AddShield(&component.dao.combatant, amount)
	if ok && added > 0 {
		component.dao.markChanged(FieldVitals)
	}
	return added, ok
}
