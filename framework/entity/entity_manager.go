package entity

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/infra/base/container"
	"github.com/tjbdwanghaibo/roost-core/infra/base/lock"
	flog "github.com/tjbdwanghaibo/roost-core/infra/observe/log"
)

// defaultBucketCnt 是实体索引的默认分桶数。
const defaultBucketCnt = 64

// EntityManager 是实体的中心登记表：按分桶索引支撑数十万实体的高并发访问，并维护锁组成员索引、实体锁分配、
// 解锁回调与持久删除准入。
type EntityManager struct {
	entities       *container.BucketHolder[int64, IThreadSafeEntity]
	idGen          func() (uint64, error)
	locks          *lock.LockManager
	configMu       sync.RWMutex
	addMu          sync.Mutex
	removing       map[int64]struct{}
	groupMu        sync.RWMutex
	groups         map[int64]map[int64]IThreadSafeEntity
	hookMu         sync.RWMutex
	nextHookID     uint64
	releaseHooks   []entityReleaseHook
	deleteAdmitter entityDeleteAdmitter
}

var (
	ErrEntityNil            = errors.New("entity manager: nil entity")
	ErrEntityRemoved        = errors.New("entity manager: entity removed")
	ErrEntityExists         = errors.New("entity manager: entity already exists")
	ErrEntityNotManaged     = errors.New("entity manager: entity not managed")
	ErrIDGeneratorRequired  = errors.New("entity manager: id generator is required for new entities")
	ErrDeleteAdmitterExists = errors.New("entity manager: delete admitter already registered")
	ErrDeleteAdmitterNeeded = errors.New("entity manager: delete admitter is required")
	ErrDeleteIndeterminate  = errors.New("entity manager: delete admission outcome is indeterminate")
	// ErrEntityNotPointer：实体实现不是指针。Guard 按实例比较接口值，值类型实现若不可比较会在运行期 panic；
	// 实体进入框架的入口（BuildEntity、TryAdd / Add）拒绝这种实现并点名类型。
	ErrEntityNotPointer = errors.New("entity: entity implementation must be a pointer")
)

// EntityManagerOption 配置 NewEntityManager。
type EntityManagerOption func(*EntityManager)

// WithEntityIDGenerator 设置新建实体的 ID 生成器。
func WithEntityIDGenerator(generator func() (uint64, error)) EntityManagerOption {
	return func(manager *EntityManager) {
		manager.idGen = generator
	}
}

// ConfigureIDGenerator 在发布任何实体之前安装 ID 生成器。可与并发的 Create 安全共存；Manager 已有实体后拒绝替换。
func (m *EntityManager) ConfigureIDGenerator(generator func() (uint64, error)) error {
	if m == nil || generator == nil {
		return fmt.Errorf("entity manager: id generator is required")
	}
	m.configMu.Lock()
	defer m.configMu.Unlock()
	if m.Len() != 0 {
		return fmt.Errorf("entity manager: id generator cannot change after entity publication")
	}
	m.idGen = generator
	return nil
}

// NewEntityManager 以默认分桶数创建 EntityManager。
func NewEntityManager(options ...EntityManagerOption) *EntityManager {
	return NewEntityManagerWithBuckets(defaultBucketCnt, options...)
}

// NewEntityManagerWithBuckets 以指定分桶数创建 EntityManager。
func NewEntityManagerWithBuckets(bucketCnt int, options ...EntityManagerOption) *EntityManager {
	manager := &EntityManager{
		entities: container.NewBucketHolder[int64, IThreadSafeEntity](bucketCnt, nil, false),
		groups:   make(map[int64]map[int64]IThreadSafeEntity),
		locks:    lock.NewLockManager(nil),
		removing: make(map[int64]struct{}),
	}
	for _, option := range options {
		if option != nil {
			option(manager)
		}
	}
	if manager.locks == nil {
		manager.locks = lock.NewLockManager(nil)
	}
	return manager
}

// Add 登记实体，失败（含 ID 重复）时 panic。
func (m *EntityManager) Add(e IThreadSafeEntity) {
	if err := m.TryAdd(e); err != nil {
		panic(fmt.Sprintf("entity manager: add failed: %v", err))
	}
}

// TryAdd 登记实体：ID 重复返回 ErrEntityExists，同 ID 实例仍在移除收尾返回 ErrEntityRemoved。
func (m *EntityManager) TryAdd(e IThreadSafeEntity) error {
	if e == nil || e.Base() == nil {
		return ErrEntityNil
	}
	// 手工构造、不经注册 builder 的实体在这里进入框架：同样要求是指针。
	if err := requirePointerEntity(e); err != nil {
		return err
	}
	id := e.ID()
	m.addMu.Lock()
	defer m.addMu.Unlock()
	if _, removing := m.removing[id]; removing {
		return fmt.Errorf("%w: %d is being removed", ErrEntityRemoved, id)
	}
	existing := m.entities.Get(id)
	if existing != nil {
		return fmt.Errorf("%w: %d", ErrEntityExists, id)
	}
	m.entities.Add(id, e)
	e.Base().setOwner(m)
	m.addGroupIndexLockedByManager(e)
	return nil
}

// entityDeleteAdmitter 是已注册的唯一持久删除准入门。
type entityDeleteAdmitter struct {
	id uint64
	fn DeleteAdmitter
}

// DeleteAdmission 告诉 EntityManager 持久删除是在本次调用中完成准入，还是挂到了当前事务上。延迟准入只在该事务越过
// 提交点后由准入方收尾；回滚时实体必须保持存活。
type DeleteAdmission uint8

const (
	DeleteAdmissionImmediate DeleteAdmission = iota
	DeleteAdmissionDeferred
	// DeleteAdmissionIndeterminate 表示持久层可能已接受删除：实体必须从内存移除并返回错误，调用方不能继续提供可能
	// 已被删除的状态。
	DeleteAdmissionIndeterminate
)

// DeleteAdmitter 负责持久层的删除事务。传入 reason，让延迟准入能在持久提交点之后执行同样的内存生命周期转换。
type DeleteAdmitter func(context.Context, IThreadSafeEntity, EntityDestroyReason) (DeleteAdmission, error)

// RegisterDeleteAdmitter 安装持久删除使用的唯一准入门，返回注销函数。多个独立的门无法形成一个原子的删除决定，
// 所以重复注册返回 ErrDeleteAdmitterExists。
func (m *EntityManager) RegisterDeleteAdmitter(admitter DeleteAdmitter) (func(), error) {
	if m == nil || admitter == nil {
		return nil, ErrDeleteAdmitterNeeded
	}
	m.hookMu.Lock()
	defer m.hookMu.Unlock()
	if m.deleteAdmitter.fn != nil {
		return nil, ErrDeleteAdmitterExists
	}
	m.nextHookID++
	id := m.nextHookID
	m.deleteAdmitter = entityDeleteAdmitter{id: id, fn: admitter}
	return func() {
		m.hookMu.Lock()
		if m.deleteAdmitter.id == id {
			m.deleteAdmitter = entityDeleteAdmitter{}
		}
		m.hookMu.Unlock()
	}, nil
}

// admitDelete 调用已注册的删除准入门；没有时返回 ErrDeleteAdmitterNeeded。
func (m *EntityManager) admitDelete(ctx context.Context, e IThreadSafeEntity, reason EntityDestroyReason) (DeleteAdmission, error) {
	m.hookMu.RLock()
	admitter := m.deleteAdmitter.fn
	m.hookMu.RUnlock()
	if admitter == nil {
		return DeleteAdmissionImmediate, ErrDeleteAdmitterNeeded
	}
	return admitter(ctx, e, reason)
}

// Destroy 在Entity锁内准入持久删除，再移出内存。等锁和准入前可取消；
// 已准入或结果未知后仍完成隔离与生命周期收尾，不能用取消撤销可能已持久的删除。
// 确定拒绝保留活实体；用户OnDestroy回调不支持强制中断。
func (m *EntityManager) Destroy(ctx context.Context, e IThreadSafeEntity, reason EntityDestroyReason, deleteFromDB bool) error {
	if m == nil || e == nil || e.Base() == nil {
		return ErrEntityNil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	mu, err := m.lockManagedForDestroy(ctx, e)
	if err != nil {
		return err
	}
	var admissionErr error
	if deleteFromDB {
		admission, err := m.admitDelete(ctx, e, reason)
		if admission == DeleteAdmissionIndeterminate && err == nil {
			err = ErrDeleteIndeterminate
		}
		if err != nil && admission != DeleteAdmissionIndeterminate {
			mu.Unlock()
			e.UnTouch()
			flog.Warn("entity manager: durable delete admission failed", "id", e.ID(), "category", e.GetEntityCategory(), "kind", e.GetEntityKind(), "reason", reason, "err", err)
			return err
		}
		admissionErr = err
		if admission == DeleteAdmissionDeferred {
			// 准入方已把生命周期转换登记到当前事务上，持久准入后的收尾归它。
			mu.Unlock()
			e.UnTouch()
			return nil
		}
		if admission != DeleteAdmissionImmediate && admission != DeleteAdmissionIndeterminate {
			mu.Unlock()
			e.UnTouch()
			return fmt.Errorf("entity manager: invalid delete admission %d", admission)
		}
	}
	id := e.ID()
	category := e.GetEntityCategory()
	kind := e.GetEntityKind()

	// 持实体锁时阻止新读者并把实体移出全局索引。生命周期回调在解锁后执行，以便经 EntityGuard 的锁序安全地与其他实体协作。
	m.addMu.Lock()
	if m.entities.Get(id) != e {
		m.addMu.Unlock()
		mu.Unlock()
		e.UnTouch()
		return ErrEntityNotManaged
	}
	m.removing[id] = struct{}{}
	e.SetRemoved()
	m.entities.Del(e.ID())
	m.removeGroupIndex(e)
	m.addMu.Unlock()
	defer func() {
		if m.locks != nil {
			m.locks.ReleaseLock(id)
		}
		m.addMu.Lock()
		delete(m.removing, id)
		m.addMu.Unlock()
	}()
	mu.Unlock()
	defer func() {
		e.UnTouch()
		e.ClearBase()
		flog.Debug("entity manager: removed", "id", id, "category", category, "kind", kind, "reason", reason, "delete_db", deleteFromDB)
	}()

	e.Base().DestroyAll(reason)

	e.OnDestroy(reason)
	return admissionErr
}

// lockManagedForDestroy 为销毁取得 e 的引用与实体锁，并确认它仍是本 Manager 托管的当前实例、ctx 未取消。成功时调用方
// 持有锁与引用；失败时两者都已归还。
func (m *EntityManager) lockManagedForDestroy(ctx context.Context, e IThreadSafeEntity) (lock.Mutex, error) {
	if !e.Touch() {
		return nil, ErrEntityRemoved
	}
	mu := e.GetMutex()
	if mu == nil {
		e.UnTouch()
		return nil, ErrEntityNil
	}
	if err := lock.LockContext(ctx, mu); err != nil {
		e.UnTouch()
		return nil, err
	}
	release := func(err error) (lock.Mutex, error) {
		mu.Unlock()
		e.UnTouch()
		return nil, err
	}
	if e.IsRemoved() || e.IsClear() {
		return release(ErrEntityRemoved)
	}
	m.addMu.Lock()
	managed := m.entities.Get(e.ID()) == e
	m.addMu.Unlock()
	if !managed {
		return release(ErrEntityNotManaged)
	}
	if err := ctx.Err(); err != nil {
		return release(err)
	}
	return mu, nil
}

// Get 返回 ID 对应的已加载实体，没有时为 nil。
func (m *EntityManager) Get(id int64) IThreadSafeEntity {
	return m.entities.Get(id)
}

// GetWithCategory 返回 ID 对应且分类相符的实体；未找到或分类不符时为 nil（category 为 None 时不检查）。
func (m *EntityManager) GetWithCategory(id int64, category EntityCategory) IThreadSafeEntity {
	e := m.entities.Get(id)
	if e != nil && category != EntityCategoryNone && e.GetEntityCategory() != category {
		return nil
	}
	return e
}

// GetMany 返回已加载的那些实体，缺失的跳过（结果与 ids 不按位置对应）。
func (m *EntityManager) GetMany(ids []int64) []IThreadSafeEntity {
	result := make([]IThreadSafeEntity, 0, len(ids))
	for _, id := range ids {
		if e := m.entities.Get(id); e != nil {
			result = append(result, e)
		}
	}
	return result
}

// Exists 报告 ID 对应的实体是否已加载。
func (m *EntityManager) Exists(id int64) bool {
	return m.entities.Get(id) != nil
}

// Len 返回托管实体总数。
func (m *EntityManager) Len() int {
	return m.entities.Count()
}

// Range 遍历全部实体，fn 返回 false 时停止。
//
// 遍历按桶快照进行、回调在桶锁外，fn 里可以 Add / Destroy。
// 交给 fn 的实体在 fn 返回前持有引用（Touch），并发或 fn 自己的 Destroy 把清理
// 推迟到引用归还，fn 读到的 ID / 分类 / 种类始终有效；遍历到达前已被摘除的实体
// 不交出。实体仍可能在 fn 期间被摘除：要改它，照常在实体锁内复核 IsRemoved。
func (m *EntityManager) Range(fn func(IThreadSafeEntity) bool) {
	m.rangeHeld(fn)
}

// RangeByCategory 遍历指定分类的实体，快照与引用规则同 Range。
func (m *EntityManager) RangeByCategory(category EntityCategory, fn func(IThreadSafeEntity) bool) {
	m.rangeHeld(func(e IThreadSafeEntity) bool {
		if e.GetEntityCategory() == category {
			return fn(e)
		}
		return true
	})
}

// CountByCategory 返回指定分类的实体数。
func (m *EntityManager) CountByCategory(category EntityCategory) int {
	count := 0
	m.rangeHeld(func(e IThreadSafeEntity) bool {
		if e.GetEntityCategory() == category {
			count++
		}
		return true
	})
	return count
}

// rangeHeld 遍历桶快照，对每个实体先 Touch 再调用 fn、返回后 UnTouch。
//
// 桶快照在锁外回调，快照里的实体可能已被 Destroy 摘除、甚至已被 doClear 清零；
// Touch 失败（已摘除）就跳过。持有引用期间 Destroy 的 ClearBase 不会清理，
// 最后一个 UnTouch 才清理——与 nest 分发持有实体引用是同一协议。
func (m *EntityManager) rangeHeld(fn func(IThreadSafeEntity) bool) {
	m.entities.RangeAll(func(_ int64, e IThreadSafeEntity) bool { return visitHeld(e, fn) })
}

// addGroupIndexLockedByManager 在 addMu 内把新登记的实体加入它所属锁组的成员索引。
func (m *EntityManager) addGroupIndexLockedByManager(e IThreadSafeEntity) {
	if m == nil || e == nil || e.Base() == nil {
		return
	}
	groupID := e.Base().GroupLockID()
	if groupID == 0 {
		return
	}
	m.groupMu.Lock()
	defer m.groupMu.Unlock()
	m.addGroupIndexLocked(groupID, e)
}

// addGroupIndexLocked 把 e 加入 groupID 的成员索引。调用方持 groupMu。
func (m *EntityManager) addGroupIndexLocked(groupID int64, e IThreadSafeEntity) {
	if groupID == 0 || e == nil {
		return
	}
	bucket := m.groups[groupID]
	if bucket == nil {
		bucket = make(map[int64]IThreadSafeEntity)
		m.groups[groupID] = bucket
	}
	bucket[e.ID()] = e
}

// removeGroupIndex 把 e 移出它所属锁组的成员索引。
func (m *EntityManager) removeGroupIndex(e IThreadSafeEntity) {
	if m == nil || e == nil || e.Base() == nil {
		return
	}
	groupID := e.Base().GroupLockID()
	if groupID == 0 {
		return
	}
	m.groupMu.Lock()
	defer m.groupMu.Unlock()
	m.removeGroupIndexLocked(groupID, e.ID())
}

// removeGroupIndexLocked 把 entityID 移出 groupID 的成员索引，组空时删除。调用方持 groupMu。
func (m *EntityManager) removeGroupIndexLocked(groupID int64, entityID int64) {
	if groupID == 0 {
		return
	}
	bucket := m.groups[groupID]
	if bucket == nil {
		return
	}
	delete(bucket, entityID)
	if len(bucket) == 0 {
		delete(m.groups, groupID)
	}
}

// UpdateEntityGroup 更新实体的锁组与 Manager 的成员索引。调用方负责持有相应的实体锁 / 组锁。
func (m *EntityManager) UpdateEntityGroup(e IThreadSafeEntity, groupID int64) error {
	if m == nil || e == nil || e.Base() == nil {
		return ErrEntityNil
	}
	if m.entities.Get(e.ID()) != e {
		return ErrEntityNotManaged
	}
	oldGroupID := e.Base().GroupLockID()
	if oldGroupID == groupID {
		return nil
	}
	m.groupMu.Lock()
	defer m.groupMu.Unlock()
	m.removeGroupIndexLocked(oldGroupID, e.ID())
	if groupID != 0 {
		m.addGroupIndexLocked(groupID, e)
	}
	e.Base().setGroupLockID(groupID)
	return nil
}

// GetGroupEntity 返回锁组 groupID 中的实体，不在组内时为 nil。
func (m *EntityManager) GetGroupEntity(groupID int64, entityID int64) IThreadSafeEntity {
	if m == nil || groupID == 0 || entityID == 0 {
		return nil
	}
	m.groupMu.RLock()
	defer m.groupMu.RUnlock()
	bucket := m.groups[groupID]
	if bucket == nil {
		return nil
	}
	return bucket[entityID]
}

// GetGroupEntities 返回锁组 groupID 成员的快照（无序）。
func (m *EntityManager) GetGroupEntities(groupID int64) []IThreadSafeEntity {
	if m == nil || groupID == 0 {
		return nil
	}
	m.groupMu.RLock()
	defer m.groupMu.RUnlock()
	bucket := m.groups[groupID]
	if len(bucket) == 0 {
		return nil
	}
	ret := make([]IThreadSafeEntity, 0, len(bucket))
	for _, e := range bucket {
		ret = append(ret, e)
	}
	return ret
}

// RangeGroupEntities 遍历组索引的快照，规则与 Range 相同：回调在锁外、可以 Add / Destroy，
// 返回 false 立即停止；每个实体先持引用（Touch）再交给 fn，到达前已被摘除的实体不交出，
// fn 期间被销毁的实体推迟到引用归还才清理。
// 不持引用直接遍历快照不行：回调里 Destroy 同组还没走到的实体，doClear 会立刻把它清零，随后以 ID 0 交给 fn。
// nest 的 EntityLockGroupScope.Range 走 GetGroupEntities 并按 GroupLockID 过滤，不经这里。
func (m *EntityManager) RangeGroupEntities(groupID int64, fn func(IThreadSafeEntity) bool) {
	if m == nil || groupID == 0 || fn == nil {
		return
	}
	for _, e := range m.GetGroupEntities(groupID) {
		if !visitHeld(e, fn) {
			return
		}
	}
}

// visitHeld 持引用调用 fn：Touch 失败（已摘除 / 已清理）时跳过并继续遍历。
func visitHeld(e IThreadSafeEntity, fn func(IThreadSafeEntity) bool) bool {
	if e == nil || !e.Touch() {
		return true
	}
	defer e.UnTouch()
	return fn(e)
}
