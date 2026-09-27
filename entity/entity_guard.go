package entity

import (
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/goroutine"
	"github.com/tjbdwanghaibo/roost-core/lock"
)

// GetEntityGroup is the lock rank of an entity id, acquired lowest first.
//
// The rank is the kind's category, derived once at registration and read here
// with a single atomic load. Deriving it from the id alone is a requirement,
// not a convenience: nest often holds nothing but an int64. The id carries the
// kind, and the registry turns a kind into its category, so no application hook
// is involved any more.
//
// EntityCategoryRemote is the lowest rank and remote-managed kinds are pinned
// to it; see that constant for why that one ordering is a physical constraint
// rather than a business convention. A kind this process does not link ranks
// with remote when its id carries the remote bit, and last otherwise.
func GetEntityGroup(guid int64) int {
	if rank, ok := lockRankOf(GetEntityKindFromID(guid)); ok {
		return rank
	}
	if GetEntityRemoteFromID(guid) {
		return int(EntityCategoryRemote)
	}
	return int(EntityCategoryUnknown)
}

type entityReleaseHook struct {
	id uint64
	fn func(IThreadSafeEntity)
}

func (m *EntityManager) RegisterOnEntityRelease(hook func(IThreadSafeEntity)) func() {
	if m == nil || hook == nil {
		return func() {}
	}
	m.hookMu.Lock()
	m.nextHookID++
	id := m.nextHookID
	m.releaseHooks = append(m.releaseHooks, entityReleaseHook{id: id, fn: hook})
	m.hookMu.Unlock()
	return func() {
		m.hookMu.Lock()
		defer m.hookMu.Unlock()
		for i, item := range m.releaseHooks {
			if item.id == id {
				m.releaseHooks = append(m.releaseHooks[:i], m.releaseHooks[i+1:]...)
				return
			}
		}
	}
}

func runOnEntityRelease(ent IThreadSafeEntity) {
	if ent == nil || ent.Base() == nil || ent.Base().owningManager() == nil {
		return
	}
	manager := ent.Base().owningManager()
	manager.hookMu.RLock()
	hooks := append([]entityReleaseHook(nil), manager.releaseHooks...)
	manager.hookMu.RUnlock()
	for _, hook := range hooks {
		hook.fn(ent)
	}
}

// EntityGuard manages per-goroutine entity locks with priority-based deadlock avoidance.
type EntityGuard struct {
	syncMutation *SyncMutation
	// createdCapturer 是当前在本 Guard 上执行的 Nest handler（事务或 memory 快路径）；只由持有 Guard 的
	// 业务 goroutine 读写。非 nil 时 handler 内新建实体沿用本 Guard 持锁并按锁序取锁（RR-20260926-35 / 48）。
	createdCapturer CreatedEntityCapturer
	// eMap 以 ID 记当前持有的实例。同 ID 的另一实例若换了锁（handler 内 Destroy 后 LockManager 摘掉了旧锁，
	// 重建的实例拿到新锁），取得新锁后新实例进 eMap，旧实例连同它仍被本 Guard 持有的锁移到 superseded，
	// Guard 释放时一并解锁（RR-20260926-67）。
	eMap        map[int64]IThreadSafeEntity
	superseded  []heldEntity
	postRelease []func()
	// revokedCreated 是本 Guard 上撤销了发布、收尾（销毁回调、回收 LockManager 条目与 removing 标记）挂在本 Guard
	// post-release 里尚未执行的新建实体 ID（revokeCreated）。同一 handler 里再建这些 ID 会撞上自己留下的 removing，
	// 那是确定失败，不是别的持有者的暂时状态（RR-20260927-21）。取走 post-release 回调时清空。
	revokedCreated []int64
}

// heldEntity 是被同 ID 新实例取代、但锁仍由本 Guard 持有的旧实例。id 单独保存：旧实例可能已被清理（ID 归零）。
type heldEntity struct {
	id  int64
	ent IThreadSafeEntity
}

type GuardScope struct {
	name  string
	guard *EntityGuard
	prev  *GuardScope
}

var guardPool = sync.Pool{
	New: func() interface{} {
		return newEntityGuard()
	},
}

var guardScopes sync.Map // map[int64]*GuardScope

func NewGuardScope(name string) (*GuardScope, func()) {
	prev := CurrentGuardScope()
	scope := &GuardScope{
		name:  name,
		guard: guardPool.Get().(*EntityGuard),
		prev:  prev,
	}
	storeGuardScope(scope)
	return scope, func() {
		releaseGuardScope(scope)
	}
}

func WithGuardScope(name string, fn func(*GuardScope) error) error {
	scope, release := NewGuardScope(name)
	defer release()
	if fn == nil {
		return nil
	}
	return fn(scope)
}

func CurrentGuardScope() *GuardScope {
	if value, ok := guardScopes.Load(goroutine.GoID()); ok {
		if scope, ok := value.(*GuardScope); ok {
			return scope
		}
	}
	return nil
}

func (s *GuardScope) Name() string {
	if s == nil {
		return ""
	}
	return s.name
}

func (s *GuardScope) Guard() *EntityGuard {
	if s == nil {
		return nil
	}
	return s.guard
}

func storeGuardScope(scope *GuardScope) {
	if scope == nil {
		guardScopes.Delete(goroutine.GoID())
		return
	}
	guardScopes.Store(goroutine.GoID(), scope)
}

func releaseGuardScope(scope *GuardScope) {
	if scope == nil {
		return
	}
	cur := CurrentGuardScope()
	if cur != scope {
		slog.Warn("entity guard scope release out of order", "scope", scope.name)
		return
	}
	guard := scope.guard
	callbacks := guard.releaseEntities()
	if scope.prev != nil {
		storeGuardScope(scope.prev)
	} else {
		guardScopes.Delete(goroutine.GoID())
	}
	scope.guard = nil
	scope.prev = nil
	guard.runPostRelease(callbacks)
	guard.clean()
	guardPool.Put(guard)
}

// GetEntityGuard returns the EntityGuard for the current entity guard scope.
// Without a scope, callers receive a standalone guard and must release it.
func GetEntityGuard() *EntityGuard {
	if scope := CurrentGuardScope(); scope != nil {
		return scope.guard
	}
	return guardPool.Get().(*EntityGuard)
}

// EntityGuardRelease releases the guard.
// If the guard is owned by the current scope, it's a no-op (scope release handles it).
// Otherwise, releases immediately.
func EntityGuardRelease(guard *EntityGuard) {
	if guard == nil {
		return
	}
	if scope := CurrentGuardScope(); scope != nil && scope.guard == guard {
		return
	}
	doGuardReleaseInCurrentGoroutine(guard)
}

func doGuardReleaseInCurrentGoroutine(guard *EntityGuard) {
	prev := CurrentGuardScope()
	temp := &GuardScope{
		name:  "release",
		guard: guard,
		prev:  prev,
	}
	storeGuardScope(temp)
	releaseGuardScope(temp)
}

func newEntityGuard() *EntityGuard {
	return &EntityGuard{
		eMap: make(map[int64]IThreadSafeEntity),
	}
}

func (e *EntityGuard) clean() {
	clear(e.eMap)
	e.superseded = nil
	e.postRelease = nil
	e.revokedCreated = nil
	e.syncMutation = nil
	e.createdCapturer = nil
}

// BindCreatedEntityCapturer 让本 Guard 上后续的 CreateInScope 把新实体交给 capturer，
// 返回的函数恢复先前的值（嵌套事务各自捕获自己的新实体）。只由持有该 Guard 的
// 业务 goroutine 调用；Nest 在调用业务 handler 期间绑定当前事务。
func (e *EntityGuard) BindCreatedEntityCapturer(capturer CreatedEntityCapturer) (restore func()) {
	if e == nil {
		return func() {}
	}
	previous := e.SwapCreatedEntityCapturer(capturer)
	return func() { e.createdCapturer = previous }
}

// SwapCreatedEntityCapturer 是 BindCreatedEntityCapturer 的无分配形式：换入 capturer 并返回先前的值，
// 调用方用返回值再换回。Nest 的 memory 快路径每条消息都要绑定，不为此分配恢复闭包。
func (e *EntityGuard) SwapCreatedEntityCapturer(capturer CreatedEntityCapturer) (previous CreatedEntityCapturer) {
	if e == nil {
		return nil
	}
	previous = e.createdCapturer
	e.createdCapturer = capturer
	return previous
}

// RequireEntity acquires the entity lock. Returns true on success.
func (e *EntityGuard) RequireEntity(ent IThreadSafeEntity) bool {
	if ent == nil {
		return false
	}
	gId := ent.GUId()
	mu := ent.GetMutex()
	if gId == 0 || mu == nil {
		return false
	}

	held, stale := e.holding(ent)
	if held {
		return true
	}

	mu.Lock()
	if ent.IsClear() || ent.IsRemoved() {
		mu.Unlock()
		return false
	}
	e.hold(gId, ent, stale)
	return true
}

// TryRequireEntity 不等待地取得实体锁：锁被占用、实体已清理或已摘除时返回 false。
func (e *EntityGuard) TryRequireEntity(ent IThreadSafeEntity) bool {
	if ent == nil {
		return false
	}
	gId := ent.GUId()
	mu := ent.GetMutex()
	if gId == 0 || mu == nil {
		return false
	}
	held, stale := e.holding(ent)
	if held {
		return true
	}
	if !mu.TryLock() {
		return false
	}
	if ent.IsClear() || ent.IsRemoved() {
		mu.Unlock()
		return false
	}
	e.hold(gId, ent, stale)
	return true
}

// holding 按实例判断本 Guard 是否已持有 ent 的锁（RR-20260926-67）：同一实例，或同 ID 且共用同一把锁的另一实例
// （Destroy 之前重复构建的实例）都算已持有。同 ID 但锁不同的旧实例作为 stale 返回——它的锁仍由本 Guard 持有，
// 但不代表 ent 的锁：只按 ID 判断会让新实例在未加锁的情况下被当作已持有。
func (e *EntityGuard) holding(ent IThreadSafeEntity) (held bool, stale IThreadSafeEntity) {
	current, ok := e.eMap[ent.GUId()]
	if !ok {
		return false, nil
	}
	if current == ent || sameMutex(current.GetMutex(), ent.GetMutex()) {
		return true, nil
	}
	return false, current
}

// sameMutex 报告 a、b 是否是同一把锁（RR-20260927-25）。之前直接用 == 比较两个 lock.Mutex 接口值：自定义 Mutex
// 若是不可比较的值类型（含 func / slice / map 字段、值接收者），同 ID 两个实例各持一份时 == 在运行期 panic。
// 框架自己的锁都是 *lock.ReentrantMutex，先按指针比较，不经反射；只有其他类型才用 reflect 判断动态类型是否可比较。
// 不可比较的值无法判定是否同一把锁，按“不是同一把锁”处理：与 RR-20260926-67 一致，新实例自己加锁，被取代的
// 旧实例连同它的锁转入 superseded，Guard 释放时一并解锁。这种值类型若内部共用同一把不可重入的锁，第二次加锁会
// 自锁——自定义 Mutex 应当用指针或可比较的值实现。
func sameMutex(a, b lock.Mutex) bool {
	if ra, ok := a.(*lock.ReentrantMutex); ok {
		rb, ok := b.(*lock.ReentrantMutex)
		return ok && ra == rb
	}
	ta := reflect.TypeOf(a)
	if ta != reflect.TypeOf(b) {
		return false
	}
	return ta == nil || (ta.Comparable() && a == b)
}

// hold 记下刚取得锁的实例；stale 非 nil 时它仍被持有的锁转入 superseded，Guard 释放时解锁。
func (e *EntityGuard) hold(id int64, ent, stale IThreadSafeEntity) {
	if stale != nil {
		e.superseded = append(e.superseded, heldEntity{id: id, ent: stale})
	}
	e.eMap[id] = ent
}

// lockCreated 取得新建实体的锁（RR-20260926-48）。Nest handler 内（capturer 非 nil）与 Cast 相同的锁序：
// 新实体的锁组高于本 Guard 已持有的全部锁组时可以等待——与 Cast 一样是按全序的有序等待，不会成环，属于
// Guard/本地锁豁免；否则等待可能与持有者成环（交叉创建、创建后再 Cast 更高锁组），只 try-lock，被占用时
// 由 capturer 给出交给业务的错误（可回滚事务整条回滚后重新准入；不能回滚的 handler 不重排，RR-20260926-64），
// 不在快 worker 上等待。
// Nest 之外（Create 自建的短作用域、独立 WithGuardScope）沿用原来的等待取锁。
func (e *EntityGuard) lockCreated(ent IThreadSafeEntity, capturer CreatedEntityCapturer) error {
	if capturer != nil && !e.mayLockEntity(ent, e.maxLockedGroup()) {
		if e.TryRequireEntity(ent) {
			return nil
		}
		if ent.GetMutex() == nil || ent.GUId() == 0 {
			return fmt.Errorf("entity guard scope lock failed: %d", ent.ID())
		}
		return capturer.CreatedEntityLockBusy(ent.ID())
	}
	if !e.RequireEntity(ent) {
		return fmt.Errorf("entity guard scope lock failed: %d", ent.ID())
	}
	return nil
}

// CheckContainAllLock checks if all entities in es are already locked or safe to lock.
// Lock order follows the application group order from lower value to higher
// value: Remote -> Player -> Alliance -> Other. Once a later group is held,
// callers must not acquire an earlier or same-level group through cast.
func (e *EntityGuard) CheckContainAllLock(es []IThreadSafeEntity) bool {
	if len(es) == 0 || len(e.eMap) == 0 {
		return true
	}
	maxLockedGroup := e.maxLockedGroup()
	for _, en := range es {
		if !e.mayLockEntity(en, maxLockedGroup) {
			return false
		}
	}
	return true
}

// CheckContainAllIDs performs the same lock-order validation before entities
// are loaded. Remote cast must use this preflight before acquiring a
// distributed lock.
func (e *EntityGuard) CheckContainAllIDs(ids []int64) bool {
	if len(ids) == 0 || len(e.eMap) == 0 {
		return true
	}
	maxLockedGroup := e.maxLockedGroup()
	for _, id := range ids {
		if !e.mayLock(id, maxLockedGroup) {
			return false
		}
	}
	return true
}

// maxLockedGroup is the highest application group among the locks this guard
// holds, or -1 when it holds none.
func (e *EntityGuard) maxLockedGroup() int {
	maxLockedGroup := -1
	for id := range e.eMap {
		if group := GetEntityGroup(id); group > maxLockedGroup {
			maxLockedGroup = group
		}
	}
	return maxLockedGroup
}

// mayLock reports whether acquiring id now keeps the lock order: it is already
// held, or it belongs to a later group than everything held so far. It judges
// by id because it runs before the entities are loaded (CheckContainAllIDs);
// once an instance is at hand, mayLockEntity decides by the instance.
func (e *EntityGuard) mayLock(id int64, maxLockedGroup int) bool {
	if _, held := e.eMap[id]; held {
		return true
	}
	return GetEntityGroup(id) > maxLockedGroup
}

// mayLockEntity 是按实例的 mayLock（RR-20260926-67）：同 ID 但换了锁的新实例不是“已持有”，而它的锁组与
// 本 Guard 持有的旧实例相同，所以不满足锁序——只能 try-lock 或拒绝，不能等待，更不能跳过加锁。
func (e *EntityGuard) mayLockEntity(ent IThreadSafeEntity, maxLockedGroup int) bool {
	if held, _ := e.holding(ent); held {
		return true
	}
	return GetEntityGroup(ent.GUId()) > maxLockedGroup
}

func (e *EntityGuard) AppendPostRelease(f func()) {
	if f != nil {
		e.postRelease = append(e.postRelease, f)
	}
}

func (e *EntityGuard) GuardEntity(ent IThreadSafeEntity) {
	e.eMap[ent.GUId()] = ent
}

// ReleaseEntity 按 ID 提前释放 id 当前登记的实例，不论调用方手里是哪个实例。同 ID 被它取代、锁仍由本 Guard 持有的旧实例
// 随后回到 eMap（RR-20260926-67）：锁序判断与 Guarded 仍要算上这把锁，Guard 释放时解锁。
// 调用方持有实例指针、要释放的是“这个实例的锁”时用 ReleaseEntityInstance：handler 内 Destroy 后同 ID 重建，
// 按 ID 释放放掉的是新实例的锁（RR-20260927-26）。
func (e *EntityGuard) ReleaseEntity(id int64) {
	ent := e.eMap[id]
	if ent != nil {
		e.doReleaseEntity(id, ent)
		e.restoreSuperseded(id)
	}
}

// ReleaseEntityInstance 提前释放本 Guard 为 ent 这个实例持有的锁，别的实例的锁不动（RR-20260927-26）：
//   - ent 是 id 当前登记的实例（或与它共用同一把锁）：同 ReleaseEntity；
//   - ent 已被同 ID 的新实例取代、锁在 superseded 里：只释放这把旧锁，新实例仍被持有；
//   - 本 Guard 没有为 ent 登记锁：什么也不做。
//
// 之前 ReleaseCast 按 ent.GUId() 调 ReleaseEntity。handler 内 Destroy X 再新建同 ID 的 X 后，旧 X 的清理若被别的访问者的
// Touch 推迟（最后一次 UnTouch 才清零 ID），旧 X 的 GUId 仍非零，ReleaseCast(旧 X) 就把新 X 的锁在 handler 中途放掉了。
// ent 的 ID 已清零时它不会是 eMap 的当前登记（按 ID 查不到），只查 superseded，与原来的空操作一致。
func (e *EntityGuard) ReleaseEntityInstance(ent IThreadSafeEntity) {
	if e == nil || ent == nil {
		return
	}
	if id := ent.GUId(); id != 0 {
		if held, _ := e.holding(ent); held {
			e.ReleaseEntity(id)
			return
		}
	}
	for i := len(e.superseded) - 1; i >= 0; i-- {
		if held := e.superseded[i]; held.ent == ent || sameMutex(held.ent.GetMutex(), ent.GetMutex()) {
			e.superseded = append(e.superseded[:i], e.superseded[i+1:]...)
			e.doReleaseEntity(held.id, held.ent)
			return
		}
	}
}

func (e *EntityGuard) restoreSuperseded(id int64) {
	for i := len(e.superseded) - 1; i >= 0; i-- {
		if held := e.superseded[i]; held.id == id {
			e.superseded = append(e.superseded[:i], e.superseded[i+1:]...)
			e.eMap[id] = held.ent
			return
		}
	}
}

func (e *EntityGuard) ReleaseAll() {
	callbacks := e.releaseEntities()
	e.runPostRelease(callbacks)
}

func (e *EntityGuard) releaseEntities() []func() {
	for id, ent := range e.eMap {
		e.safeReleaseEntity(id, ent)
	}
	superseded := e.superseded
	e.superseded = nil
	for _, held := range superseded {
		e.safeReleaseEntity(held.id, held.ent)
	}
	callbacks := e.postRelease
	e.postRelease = nil
	// 撤销收尾就在 callbacks 里，随后执行；之后同 ID 不再有本 Guard 留下的 removing。
	e.revokedCreated = nil
	return callbacks
}

// revokedInThisGuard 报告 id 是否是本 Guard 上撤销、收尾尚未执行的新建实体（RR-20260927-21）。
func (e *EntityGuard) revokedInThisGuard(id int64) bool {
	return e != nil && slices.Contains(e.revokedCreated, id)
}

func (e *EntityGuard) runPostRelease(callbacks []func()) {
	for _, f := range callbacks {
		if f != nil {
			func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Error("entity guard post-release callback panic", "err", r)
					}
				}()
				f()
			}()
		}
	}
}

func (e *EntityGuard) safeReleaseEntity(id int64, ent IThreadSafeEntity) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("entity release hook panic", "id", id, "err", r)
		}
	}()
	e.doReleaseEntity(id, ent)
}

// doReleaseEntity 解锁 ent，并在它仍是 id 的当前实例时从 eMap 删除（被取代的旧实例不在 eMap 里）。
func (e *EntityGuard) doReleaseEntity(id int64, ent IThreadSafeEntity) {
	mu := ent.GetMutex()
	defer func() {
		if current, ok := e.eMap[id]; ok && current == ent {
			delete(e.eMap, id)
		}
		mu.Unlock()
	}()

	// Trigger release hooks while lock is held.
	runOnEntityRelease(ent)
}

// Guarded reports whether this guard currently holds the entity's lock. This
// is the accessor the framework's hot paths use: it answers the only question
// they ask, and it does so without handing out the guard's bookkeeping or
// allocating anything.
func (e *EntityGuard) Guarded(id int64) bool {
	if e == nil {
		return false
	}
	_, held := e.eMap[id]
	return held
}

// GuardedEntity reports whether this guard holds ent's own lock: the same
// instance, or another instance with the same id that shares the held mutex.
// Guarded answers by id only; after a handler destroyed and re-created an id,
// the re-created instance has a new mutex and is not held until it is locked
// (RR-20260926-67). Code that is about to skip locking an instance it holds a
// pointer to must ask this, not Guarded.
func (e *EntityGuard) GuardedEntity(ent IThreadSafeEntity) bool {
	if e == nil || ent == nil {
		return false
	}
	held, _ := e.holding(ent)
	return held
}

// GuardedCount is the number of entities this guard currently holds.
func (e *EntityGuard) GuardedCount() int {
	if e == nil {
		return 0
	}
	return len(e.eMap)
}

// Entities returns a snapshot of the currently guarded entities.
//
// It is a copy on purpose. eMap is the lock-scope ledger that drives lock
// ordering and reentrancy decisions in nest, so handing out the live map let
// any caller corrupt deadlock prevention with a stray delete. Callers that
// only need membership or a count should use Guarded/GuardedCount, which
// allocate nothing; this one exists for diagnostics and iteration.
func (e *EntityGuard) Entities() map[int64]IThreadSafeEntity {
	if e == nil {
		return nil
	}
	out := make(map[int64]IThreadSafeEntity, len(e.eMap))
	for id, ent := range e.eMap {
		out[id] = ent
	}
	return out
}
