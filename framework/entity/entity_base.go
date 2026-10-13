package entity

import (
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/tjbdwanghaibo/roost-core/framework/event"
	"github.com/tjbdwanghaibo/roost-core/infra/base/lock"
)

// tryTouch 的位布局：
//
//	bit 31（符号位）：clearedBit — 实体已清理
//	bit 30：          removedBit — 实体已标记移除
//	bit 0-29：        touchCnt   — 并发引用计数
const (
	touchCntMask int32 = 0x3FFFFFFF
	removedBit   int32 = 1 << 30
	clearedBit   int32 = -1 << 31
)

// EntityBase 是实体的基础设施：身份、生命周期状态、引用计数、实体锁、锁组状态与同步状态。它不持有接口引用，
// 装配全部由生成代码完成。
type EntityBase struct {
	id             int64
	category       EntityCategory
	kind           EntityKind
	notAutoPersist bool
	lifetime       EntityLifetime
	tryTouch       atomic.Int32
	mu             lock.Mutex
	groupLockID    atomic.Int64
	groupEpoch     atomic.Uint64
	groupState     atomic.Int32
	groupTargetID  atomic.Int64
	owner          atomic.Pointer[EntityManager]
	// lastCommitLSN 是最近一次修改本实体的 pipelined 事务的 WAL LSN（入队时在实体锁内写入），是外部化门的输入：
	// checkpoint 与 sync 不得持久化或分发 LSN 高于 committer 持久水位的状态。0 表示没有 pipelined 事务修改过它。
	lastCommitLSN atomic.Uint64

	// eventBus 提供实体级的事件订阅 / 发布。
	eventBus *event.EventBus
	syncMu   sync.RWMutex
	sync     *SubjectSyncState

	// 生命周期 hook，由生成的实体工厂代码设置。
	onClear   func()
	onDestroy func(EntityDestroyReason)
}

// SetLastCommitLSN 记录最近一次修改本实体的 pipelined 事务的 WAL LSN。Nest 在实体锁内调用；启用同步时同时写入
// 同步状态。
func (e *EntityBase) SetLastCommitLSN(lsn uint64) {
	if e == nil {
		return
	}
	e.lastCommitLSN.Store(lsn)
	if state := e.Sync(); state != nil {
		state.SetLastCommitLSN(lsn)
	}
}

// LastCommitLSN 返回本实体最近的 pipelined 提交 LSN；没有时为 0。
func (e *EntityBase) LastCommitLSN() uint64 {
	if e == nil {
		return 0
	}
	return e.lastCommitLSN.Load()
}

func (e *EntityBase) setOwner(owner *EntityManager) {
	if e != nil {
		e.owner.Store(owner)
	}
}

func (e *EntityBase) owningManager() *EntityManager {
	if e == nil {
		return nil
	}
	return e.owner.Load()
}

// NewEntityBase 创建 EntityBase，使用以 id 为种子的可重入锁。id 必须是完整 EntityID；仍拿到生成的 UniqueID 的构造
// 边界先用 EntityCreateParam.NormalizeID。hook 另经 SetHooks 设置。
func NewEntityBase(id int64, category EntityCategory, notAutoPersist bool, kinds ...EntityKind) *EntityBase {
	return NewEntityBaseWithMutex(id, category, notAutoPersist, lock.NewReentrantMutex(id), kinds...)
}

// NewEntityBaseWithMutex 与 NewEntityBase 相同，但使用指定的实体锁（nil 时新建可重入锁）。
func NewEntityBaseWithMutex(id int64, category EntityCategory, notAutoPersist bool, mu lock.Mutex, kinds ...EntityKind) *EntityBase {
	if mu == nil {
		mu = lock.NewReentrantMutex(id)
	}
	kind := EntityKindNone
	if len(kinds) > 0 {
		kind = kinds[0]
	}
	return &EntityBase{
		id:             id,
		category:       category,
		kind:           kind,
		notAutoPersist: notAutoPersist,
		lifetime:       DefaultEntityLifetime(notAutoPersist, RemotePolicyNone),
		mu:             mu,
	}
}

// SetHooks 设置生命周期回调，由生成的工厂代码调用。
func (e *EntityBase) SetHooks(onClear func(), onDestroy func(EntityDestroyReason)) {
	e.onClear = onClear
	e.onDestroy = onDestroy
}

// EventBus 返回实体的事件总线。
func (e *EntityBase) EventBus() *event.EventBus {
	return e.eventBus
}

// SubEvent 经实体的事件总线订阅事件类型。
func (e *EntityBase) SubEvent(eventType event.EventType) {
	if e.eventBus != nil {
		e.eventBus.SubEvent(eventType)
	}
}

// PubEvent 经实体的事件总线发布事件。
func (e *EntityBase) PubEvent(d event.EventData) {
	if e.eventBus != nil {
		e.eventBus.PubEvent(d)
	}
}

// EnableSync 装配实体级同步状态。它有意不是组件：同步可见性与脏数据刷新属于实体的基础生命周期。
func (e *EntityBase) EnableSync(param EntitySyncCreateParam) {
	if e == nil {
		return
	}
	if !param.Enabled {
		e.SetSyncState(nil)
		return
	}
	if param.EntityID == 0 {
		param.EntityID = e.id
	}
	e.SetSyncState(NewSubjectSyncState(SubjectSyncCreateParam{
		Enabled: param.Enabled, SubjectID: param.EntityID, Namespace: param.Namespace,
		SubjectKind: param.EntityKind, Packer: param.Packer,
	}))
}

// SetSyncState 替换实体的同步状态并关闭旧状态。新状态的实体锁回调在当前 Guard 已持有本实体时直接执行。
func (e *EntityBase) SetSyncState(syncState *SubjectSyncState) {
	if e == nil {
		return
	}
	e.syncMu.Lock()
	previous := e.sync
	if previous == syncState {
		e.syncMu.Unlock()
		return
	}
	if syncState != nil {
		// ID 在安装时取定：同步调度器可能在实体被销毁 / 撤销（doClear 清零 id）的同时进入这里，
		// 不能在锁外再读 EntityBase 的可变字段。
		entityID := e.ID()
		syncState.setEntityLock(func(fn func() error) error {
			if fn == nil {
				return nil
			}
			if entitySyncLockedInCurrentGuard(entityID) || e.GetMutex() == nil {
				return fn()
			}
			e.GetMutex().Lock()
			defer e.GetMutex().Unlock()
			return fn()
		})
	}
	e.sync = syncState
	e.syncMu.Unlock()
	if previous != nil && previous != syncState {
		previous.Close()
	}
}

// Sync 返回实体的同步状态；未启用时为 nil。
func (e *EntityBase) Sync() *SubjectSyncState {
	if e == nil {
		return nil
	}
	e.syncMu.RLock()
	defer e.syncMu.RUnlock()
	return e.sync
}

// MarkSyncDirty 按字段掩码标脏（只标脏，不发送）。
func (e *EntityBase) MarkSyncDirty(mask uint64) {
	if syncState := e.Sync(); syncState != nil {
		syncState.MarkDirty(mask)
	}
}

// --- 身份 ---

// ID 返回完整 EntityID；清理后为 0。
func (e *EntityBase) ID() int64 {
	if e == nil {
		return 0
	}
	return e.id
}

// UniqueID 返回 ID 中的唯一号部分。
func (e *EntityBase) UniqueID() int64 {
	if e == nil {
		return 0
	}
	return GetUniqueIDFromEntityID(e.id)
}

// StorageID 返回持久化使用的 ID（即完整 EntityID）。
func (e *EntityBase) StorageID() int64 {
	if e == nil {
		return 0
	}
	return e.id
}

func (e *EntityBase) SetID(id int64) {
	e.id = id
}

// GUId 返回完整 EntityID（锁序与 Guard 账本使用）。
func (e *EntityBase) GUId() int64 {
	if e == nil {
		return 0
	}
	return e.id
}

func (e *EntityBase) GetEntityCategory() EntityCategory {
	if e == nil {
		return EntityCategoryNone
	}
	return e.category
}

func (e *EntityBase) GetEntityKind() EntityKind {
	if e == nil {
		return EntityKindNone
	}
	return e.kind
}

func (e *EntityBase) GetMutex() lock.Mutex {
	if e == nil {
		return nil
	}
	return e.mu
}

// AutoPersist 报告实体是否自动持久化。
func (e *EntityBase) AutoPersist() bool {
	return !e.notAutoPersist
}

func (e *EntityBase) Lifetime() EntityLifetime {
	if e == nil {
		return EntityLifetimeDefault
	}
	return e.lifetime
}

func (e *EntityBase) SetLifetime(lifetime EntityLifetime) {
	if e == nil {
		return
	}
	e.lifetime = lifetime
}

// OnInitFinish 是默认的空实现；具体实体可覆盖以执行初始化后的逻辑。
func (e *EntityBase) OnInitFinish(param *EntityCreateParam) error {
	return nil
}

// OnDestroy 是默认的空实现；具体实体可覆盖以执行移除时的清理。
func (e *EntityBase) OnDestroy(reason EntityDestroyReason) {}

// --- 引用计数 ---

// Touch 原子地增加引用计数；实体已移除或已清理时返回 false。
func (e *EntityBase) Touch() bool {
	if e == nil {
		return false
	}
	for {
		old := e.tryTouch.Load()
		if old&(removedBit|clearedBit) != 0 {
			return false
		}
		if old&touchCntMask == touchCntMask {
			panic("entity: reference count overflow")
		}
		if e.tryTouch.CompareAndSwap(old, old+1) {
			return true
		}
	}
}

// UnTouch 原子地减少引用计数；实体已移除且计数归零时由本次调用执行清理。
func (e *EntityBase) UnTouch() {
	if e == nil {
		panic("entity: nil EntityBase reference release")
	}
	var next int32
	for {
		old := e.tryTouch.Load()
		if old&touchCntMask == 0 {
			panic("entity: reference count underflow")
		}
		next = old - 1
		if e.tryTouch.CompareAndSwap(old, next) {
			break
		}
	}
	if next&removedBit != 0 && next&touchCntMask == 0 {
		for {
			old := e.tryTouch.Load()
			if old&clearedBit != 0 {
				return
			}
			if e.tryTouch.CompareAndSwap(old, old|clearedBit) {
				break
			}
		}
		e.doClear()
	}
}

// IsRemoved 报告实体是否已标记移除。
func (e *EntityBase) IsRemoved() bool {
	if e == nil {
		return true
	}
	return e.tryTouch.Load()&removedBit != 0
}

// SetRemoved 标记实体已移除；此后 Touch 失败。
func (e *EntityBase) SetRemoved() {
	for {
		old := e.tryTouch.Load()
		if e.tryTouch.CompareAndSwap(old, old|removedBit) {
			return
		}
	}
}

// IsClear 报告实体是否已清理。
func (e *EntityBase) IsClear() bool {
	if e == nil {
		return true
	}
	return e.tryTouch.Load()&clearedBit != 0
}

// ClearBase 在没有引用时清理实体（CAS 设置 clearedBit）；仍有引用时什么也不做，由最后一次 UnTouch 清理。
// 销毁实体时显式调用。
func (e *EntityBase) ClearBase() {
	for {
		old := e.tryTouch.Load()
		if old&clearedBit != 0 {
			return
		}
		if old&touchCntMask != 0 {
			return
		}
		if e.tryTouch.CompareAndSwap(old, old|clearedBit) {
			e.doClear()
			return
		}
	}
}

// doClear 执行清理：依次调用 onClear、销毁事件总线、关闭同步状态（各自恢复 panic），清零身份与锁组状态；
// 全部完成后重新抛出第一个 panic。
func (e *EntityBase) doClear() {
	var firstPanic any
	runCleanup := func(fn func()) {
		if fn == nil {
			return
		}
		defer func() {
			if recovered := recover(); recovered != nil && firstPanic == nil {
				firstPanic = recovered
			}
		}()
		fn()
	}
	onClear := e.onClear
	e.onClear = nil
	runCleanup(onClear)
	eventBus := e.eventBus
	e.eventBus = nil
	if eventBus != nil {
		runCleanup(eventBus.Destroy)
	}
	e.syncMu.Lock()
	syncState := e.sync
	e.sync = nil
	e.syncMu.Unlock()
	if syncState != nil {
		runCleanup(syncState.Close)
	}
	e.id = 0
	e.category = EntityCategoryNone
	e.kind = EntityKindNone
	e.notAutoPersist = false
	e.lifetime = EntityLifetimeDefault
	e.groupLockID.Store(0)
	e.groupEpoch.Store(0)
	e.groupState.Store(int32(EntityGroupTransitionNone))
	e.groupTargetID.Store(0)
	e.owner.Store(nil)
	e.onDestroy = nil
	if firstPanic != nil {
		panic(firstPanic)
	}
}

// DestroyAll 调用销毁 hook（清理组件）。
func (e *EntityBase) DestroyAll(reason EntityDestroyReason) {
	if e.onDestroy != nil {
		e.onDestroy(reason)
	}
}

// --- 组件登记（只有定义，不持有 owner 指针）---

// ComponentInterfaceBase 是组件的最小接口。
type ComponentInterfaceBase interface {
	Name() string
	OnInitFinish(param *EntityCreateParam, isCreate bool) error
	OnDestroy(rea EntityDestroyReason)
}

// ComponentManager 是组件容器，不持有 owner 引用，装配由生成代码完成。
type ComponentManager struct {
	Comps     map[ComponentType]ComponentInterfaceBase
	initOrder []ComponentType
}

// NewComponentManager 创建空的组件容器。
func NewComponentManager() ComponentManager {
	return ComponentManager{
		Comps: make(map[ComponentType]ComponentInterfaceBase),
	}
}

// Set 登记组件，由生成代码调用。
func (c *ComponentManager) Set(compType ComponentType, comp ComponentInterfaceBase) {
	c.Comps[compType] = comp
}

// Get 按类型取组件。
func (c *ComponentManager) Get(compType ComponentType) ComponentInterfaceBase {
	return c.Comps[compType]
}

// InitAll 按依赖拓扑序初始化组件，不在依赖图中的组件随后按类型号顺序初始化；记录实际初始化顺序供 DestroyAll 逆序销毁。
func (c *ComponentManager) InitAll(param *EntityCreateParam, isCreate bool) error {
	c.initOrder = c.initOrder[:0]
	sorted, err := GetTopologicalSortedComponents()
	if err != nil {
		return err
	}

	if len(sorted) == 0 {
		for _, ct := range sortedComponentTypes(c.Comps) {
			comp := c.Comps[ct]
			if err := comp.OnInitFinish(param, isCreate); err != nil {
				return err
			}
			c.initOrder = append(c.initOrder, ct)
		}
		return nil
	}

	initialized := make(map[ComponentType]bool)
	for _, ct := range sorted {
		if comp, ok := c.Comps[ct]; ok {
			if err := comp.OnInitFinish(param, isCreate); err != nil {
				return err
			}
			initialized[ct] = true
			c.initOrder = append(c.initOrder, ct)
		}
	}

	// 初始化不在依赖图中的其余组件。
	for _, ct := range sortedComponentTypes(c.Comps) {
		if !initialized[ct] {
			comp := c.Comps[ct]
			if err := comp.OnInitFinish(param, isCreate); err != nil {
				return err
			}
			c.initOrder = append(c.initOrder, ct)
		}
	}
	return nil
}

// DestroyAll 按初始化的逆序调用组件的 OnDestroy，未初始化的组件随后按类型号逆序销毁；每个组件各自恢复 panic，
// 全部完成后重新抛出第一个 panic。
func (c *ComponentManager) DestroyAll(reason EntityDestroyReason) {
	var firstPanic any
	destroy := func(comp ComponentInterfaceBase) {
		defer func() {
			if recovered := recover(); recovered != nil && firstPanic == nil {
				firstPanic = recovered
			}
		}()
		comp.OnDestroy(reason)
	}
	destroyed := make(map[ComponentType]bool, len(c.initOrder))
	for i := len(c.initOrder) - 1; i >= 0; i-- {
		ct := c.initOrder[i]
		comp := c.Comps[ct]
		if comp == nil || destroyed[ct] {
			continue
		}
		destroyed[ct] = true
		destroy(comp)
	}
	types := sortedComponentTypes(c.Comps)
	for i := len(types) - 1; i >= 0; i-- {
		ct := types[i]
		if destroyed[ct] {
			continue
		}
		if comp := c.Comps[ct]; comp != nil {
			destroyed[ct] = true
			destroy(comp)
		}
	}
	if firstPanic != nil {
		panic(firstPanic)
	}
}

// Clear 释放全部组件引用。
func (c *ComponentManager) Clear() {
	clear(c.Comps)
	c.initOrder = c.initOrder[:0]
}

// sortedComponentTypes 返回按类型号升序的组件类型。
func sortedComponentTypes(comps map[ComponentType]ComponentInterfaceBase) []ComponentType {
	return slices.Sorted(maps.Keys(comps))
}

// --- 全局组件依赖登记 ---

var globalCompDependLock sync.RWMutex
var globalCompDependencies = newCompTopologicalSort()

// GetTopologicalSortedComponents 按依赖顺序返回组件类型；存在环时返回错误。
func GetTopologicalSortedComponents() ([]ComponentType, error) {
	globalCompDependLock.RLock()
	defer globalCompDependLock.RUnlock()
	return globalCompDependencies.GetTopologicalSortedComponents()
}

// --- 全局组件工厂 ---

// ComponentFactory 创建组件实例。owner 以 any 传入（具体实体指针），由工厂内部做类型断言。
type ComponentFactory func(owner any, param *EntityCreateParam) (ComponentInterfaceBase, error)

var globalCompFactoryLock sync.RWMutex
var globalCompFactories = make(map[ComponentType]ComponentFactory)

// RegisterComponentFactory 为组件类型登记全局工厂。在服务启动路径调用，重复登记 panic。
func RegisterComponentFactory(compType ComponentType, factory ComponentFactory) {
	globalCompFactoryLock.Lock()
	defer globalCompFactoryLock.Unlock()
	if _, exists := globalCompFactories[compType]; exists {
		panic("duplicate component factory registration")
	}
	globalCompFactories[compType] = factory
}

// CreateComponent 调用组件类型登记的工厂。
func CreateComponent(compType ComponentType, owner any, param *EntityCreateParam) (ComponentInterfaceBase, error) {
	globalCompFactoryLock.RLock()
	factory, ok := globalCompFactories[compType]
	globalCompFactoryLock.RUnlock()
	if !ok {
		return nil, fmt.Errorf("no component factory registered for type %d", compType)
	}
	return factory(owner, param)
}

// --- DAO 容器（纯容器，不持有 owner）---

// DaoManager 按集合名持有 DAO 实例，不持有 owner 引用，装配由生成代码完成。
type DaoManager struct {
	Daos map[string]DaoInterface
}

// NewDaoManager 创建空的 DAO 容器。
func NewDaoManager() DaoManager {
	return DaoManager{
		Daos: make(map[string]DaoInterface),
	}
}

// Set 登记 DAO，由生成代码调用。
func (d *DaoManager) Set(collName string, dao DaoInterface) {
	d.Daos[collName] = dao
}

// Get 按集合名取 DAO。
func (d *DaoManager) Get(collName string) DaoInterface {
	return d.Daos[collName]
}

// RangeDao 遍历全部 DAO（顺序不定）。
func (d *DaoManager) RangeDao(f func(DaoInterface)) {
	for _, dao := range d.Daos {
		f(dao)
	}
}

// Clear 释放全部 DAO 引用。
func (d *DaoManager) Clear() {
	clear(d.Daos)
}
