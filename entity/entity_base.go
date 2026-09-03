package entity

import (
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/event"
	"github.com/tjbdwanghaibo/roost-core/lock"
	"sort"
	"sync"
	"sync/atomic"
)

// tryTouch bit layout:
//
//	bit 31 (sign): clearedBit — entity cleanup done
//	bit 30:        removedBit — entity marked for removal
//	bits 0-29:     touchCnt   — concurrent reference count
const (
	touchCntMask int32 = 0x3FFFFFFF
	removedBit   int32 = 1 << 30
	clearedBit   int32 = -1 << 31
)

// EntityBase is the core entity infrastructure.
// It holds identity, lifecycle state, and reference counting.
// No interface references — all wiring is done via generated code.
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
	// lastCommitLSN is the WAL LSN of the newest pipelined transaction that
	// mutated this entity (set under the entity lock at enqueue time). It is
	// the externalization gate input: checkpoint and sync must not persist or
	// distribute state whose LSN is above the committer's durable watermark.
	// Zero means no pipelined transaction touched the entity.
	lastCommitLSN atomic.Uint64

	// Event bus for pub/sub capability.
	eventBus *event.EventBus
	syncMu   sync.RWMutex
	sync     *SubjectSyncState

	// Lifecycle hooks — set by generated entity factory code.
	onClear   func()
	onDestroy func(EntityDestroyReason)
}

// SetLastCommitLSN records the WAL LSN of the newest pipelined transaction
// that mutated this entity. Called by nest under the entity lock; the value
// is also mirrored into the subject sync state when sync is enabled.
func (e *EntityBase) SetLastCommitLSN(lsn uint64) {
	if e == nil {
		return
	}
	e.lastCommitLSN.Store(lsn)
	if state := e.Sync(); state != nil {
		state.SetLastCommitLSN(lsn)
	}
}

// LastCommitLSN returns the newest pipelined-commit LSN for this entity, or
// zero when no pipelined transaction touched it.
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

// NewEntityBase creates an EntityBase. The id must be a full EntityID; use
// EntityCreateParam.NormalizeID at construction boundaries that still receive a
// generated unique ID. Hooks are set separately via SetHooks.
func NewEntityBase(id int64, category EntityCategory, notAutoPersist bool, kinds ...EntityKind) *EntityBase {
	return NewEntityBaseWithMutex(id, category, notAutoPersist, lock.NewReentrantMutex(id), kinds...)
}

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

// SetHooks configures lifecycle callbacks. Called by generated factory code.
func (e *EntityBase) SetHooks(onClear func(), onDestroy func(EntityDestroyReason)) {
	e.onClear = onClear
	e.onDestroy = onDestroy
}

// SetEventBus wires the event bus. Called by generated factory code or application layer.
func (e *EntityBase) SetEventBus(bus *event.EventBus) {
	e.eventBus = bus
}

// EventBus returns the entity's event bus.
func (e *EntityBase) EventBus() *event.EventBus {
	return e.eventBus
}

// SubEvent subscribes to an event type via the entity's event bus.
func (e *EntityBase) SubEvent(eventType event.EventType) {
	if e.eventBus != nil {
		e.eventBus.SubEvent(eventType)
	}
}

// PubEvent publishes an event via the entity's event bus.
func (e *EntityBase) PubEvent(d event.EventData) {
	if e.eventBus != nil {
		e.eventBus.PubEvent(d)
	}
}

// EnableSync wires entity-level sync state. It is intentionally not a component:
// sync visibility and dirty flushing belong to the entity's base lifecycle.
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
		Enabled: param.Enabled, SubjectID: param.EntityID, Namespace: param.Topic,
		SubjectKind: param.EntityKind, Packer: param.Packer,
	}))
}

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
		syncState.setEntityLock(func(fn func() error) error {
			if fn == nil {
				return nil
			}
			if entitySyncLockedInCurrentGuard(e.ID()) || e.GetMutex() == nil {
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

func (e *EntityBase) Sync() *SubjectSyncState {
	if e == nil {
		return nil
	}
	e.syncMu.RLock()
	defer e.syncMu.RUnlock()
	return e.sync
}

func (e *EntityBase) SyncEnabled() bool {
	if syncState := e.Sync(); syncState != nil {
		return syncState.Enabled()
	}
	return false
}

func (e *EntityBase) MarkSyncDirty(mask uint64) {
	if syncState := e.Sync(); syncState != nil {
		syncState.MarkDirty(mask)
	}
}

func (e *EntityBase) MarkSyncFullDirty(reason uint32) {
	if syncState := e.Sync(); syncState != nil {
		syncState.MarkFullDirty(reason)
	}
}

// --- Identity ---

func (e *EntityBase) ID() int64 {
	if e == nil {
		return 0
	}
	return e.id
}

func (e *EntityBase) UniqueID() int64 {
	if e == nil {
		return 0
	}
	return GetUniqueIDFromEntityID(e.id)
}

func (e *EntityBase) StorageID() int64 {
	if e == nil {
		return 0
	}
	return e.id
}

func (e *EntityBase) SetID(id int64) {
	e.id = id
}

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

// OnInitFinish is the default no-op implementation.
// Override in concrete entity for post-initialization logic.
func (e *EntityBase) OnInitFinish(param *EntityCreateParam) error {
	return nil
}

// OnDestroy is the default no-op implementation.
// Override in concrete entity for cleanup logic on removal.
func (e *EntityBase) OnDestroy(reason EntityDestroyReason) {}

// --- Reference counting ---

// Touch atomically increments reference count. Returns false if removed or cleared.
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

// UnTouch atomically decrements reference count. Triggers clear if removed and count reaches zero.
func (e *EntityBase) UnTouch() {
	if e == nil {
		panic("entity: nil EntityBase reference release")
	}
	var new int32
	for {
		old := e.tryTouch.Load()
		if old&touchCntMask == 0 {
			panic("entity: reference count underflow")
		}
		new = old - 1
		if e.tryTouch.CompareAndSwap(old, new) {
			break
		}
	}
	if new&removedBit != 0 && new&touchCntMask == 0 {
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

func (e *EntityBase) IsRemoved() bool {
	if e == nil {
		return true
	}
	return e.tryTouch.Load()&removedBit != 0
}

func (e *EntityBase) SetRemoved() {
	for {
		old := e.tryTouch.Load()
		if e.tryTouch.CompareAndSwap(old, old|removedBit) {
			return
		}
	}
}

func (e *EntityBase) IsClear() bool {
	if e == nil {
		return true
	}
	return e.tryTouch.Load()&clearedBit != 0
}

// ClearBase tries to clear internal state via CAS on clearedBit.
// Called explicitly when an entity is destroyed without going through UnTouch.
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

// DestroyAll invokes the destroy hook (for components cleanup).
func (e *EntityBase) DestroyAll(reason EntityDestroyReason) {
	if e.onDestroy != nil {
		e.onDestroy(reason)
	}
}

// --- Component Registry (definitions only, no owner pointer) ---

// ComponentInterfaceBase is the minimal component interface.
type ComponentInterfaceBase interface {
	Name() string
	OnInitFinish(param *EntityCreateParam, isCreate bool) error
	OnDestroy(rea EntityDestroyReason)
}

// ComponentManager is a simple container for components.
// No owner reference — wiring is done by generated code.
type ComponentManager struct {
	Comps     map[ComponentType]ComponentInterfaceBase
	initOrder []ComponentType
}

func NewComponentManager() ComponentManager {
	return ComponentManager{
		Comps: make(map[ComponentType]ComponentInterfaceBase),
	}
}

// Set registers a component. Called by generated code.
func (c *ComponentManager) Set(compType ComponentType, comp ComponentInterfaceBase) {
	c.Comps[compType] = comp
}

// Get retrieves a component by type.
func (c *ComponentManager) Get(compType ComponentType) ComponentInterfaceBase {
	return c.Comps[compType]
}

// InitAll initializes all components in topological order.
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

	// Init any remaining (not in dependency graph)
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

// DestroyAll calls OnDestroy on all components.
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

// Clear releases all component references.
func (c *ComponentManager) Clear() {
	clear(c.Comps)
	c.initOrder = c.initOrder[:0]
}

func sortedComponentTypes(comps map[ComponentType]ComponentInterfaceBase) []ComponentType {
	types := make([]ComponentType, 0, len(comps))
	for ct := range comps {
		types = append(types, ct)
	}
	sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })
	return types
}

// --- Global component dependency registration ---

var globalCompDependLock sync.RWMutex
var globalCompDependencies = newCompTopologicalSort()

// RegisterComponentDependency declares that a component type depends on others.
// This determines initialization order. Call from the service bootstrap path.
func RegisterComponentDependency(compType ComponentType, dependencies ...ComponentType) {
	globalCompDependLock.Lock()
	defer globalCompDependLock.Unlock()
	globalCompDependencies.RegisterCompDependency(compType, dependencies...)
}

// GetTopologicalSortedComponents returns components in dependency order.
func GetTopologicalSortedComponents() ([]ComponentType, error) {
	globalCompDependLock.RLock()
	defer globalCompDependLock.RUnlock()
	return globalCompDependencies.GetTopologicalSortedComponents()
}

// --- Global component factory ---

// ComponentFactory creates a component instance. The owner is passed as `any`
// (concrete entity pointer); the factory does type assertion internally.
type ComponentFactory func(owner any, param *EntityCreateParam) (ComponentInterfaceBase, error)

var globalCompFactoryLock sync.RWMutex
var globalCompFactories = make(map[ComponentType]ComponentFactory)

// RegisterComponentFactory registers a global factory for a component type.
// Call from the service bootstrap path. Panics on duplicate registration.
func RegisterComponentFactory(compType ComponentType, factory ComponentFactory) {
	globalCompFactoryLock.Lock()
	defer globalCompFactoryLock.Unlock()
	if _, exists := globalCompFactories[compType]; exists {
		panic("duplicate component factory registration")
	}
	globalCompFactories[compType] = factory
}

func ResetComponentRegistryForTest() {
	globalCompDependLock.Lock()
	globalCompDependencies = newCompTopologicalSort()
	globalCompDependLock.Unlock()

	globalCompFactoryLock.Lock()
	globalCompFactories = make(map[ComponentType]ComponentFactory)
	globalCompFactoryLock.Unlock()
}

// CreateComponent invokes the registered factory for the given component type.
func CreateComponent(compType ComponentType, owner any, param *EntityCreateParam) (ComponentInterfaceBase, error) {
	globalCompFactoryLock.RLock()
	factory, ok := globalCompFactories[compType]
	globalCompFactoryLock.RUnlock()
	if !ok {
		return nil, fmt.Errorf("no component factory registered for type %d", compType)
	}
	return factory(owner, param)
}

// --- DAO Manager (pure container, no owner) ---

// DaoManager holds DAO instances by collection name.
// No owner reference — wiring is done by generated code.
type DaoManager struct {
	Daos map[string]DaoInterface
}

func NewDaoManager() DaoManager {
	return DaoManager{
		Daos: make(map[string]DaoInterface),
	}
}

// Set registers a DAO. Called by generated code.
func (d *DaoManager) Set(collName string, dao DaoInterface) {
	d.Daos[collName] = dao
}

// Get retrieves a DAO by collection name.
func (d *DaoManager) Get(collName string) DaoInterface {
	return d.Daos[collName]
}

// RangeDao iterates all DAOs.
func (d *DaoManager) RangeDao(f func(DaoInterface)) {
	for _, dao := range d.Daos {
		f(dao)
	}
}

// Clear releases all DAO references.
func (d *DaoManager) Clear() {
	clear(d.Daos)
}
