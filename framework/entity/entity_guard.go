package entity

import (
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/base/goroutine"
	"github.com/tjbdwanghaibo/roost-core/infra/base/lock"
)

// GetEntityGroup 返回实体 ID 的锁序等级，取锁按等级从低到高。
//
// 等级就是 kind 的分类，注册时算好，这里一次原子读取。只凭 ID 算出等级是硬性要求：Nest 常常手里只有一个 int64；
// ID 携带 kind，注册表把 kind 映射为分类，不经任何应用 hook。
//
// EntityCategoryRemote 是最低等级，Remote 托管 kind 固定在这一级（原因见该常量：这一条顺序是物理约束而非业务约定）。
// 本进程未链接的 kind：ID 带 Remote 位时与 Remote 同级，否则排在最后。
func GetEntityGroup(guid int64) int {
	if rank, ok := lockRankOf(GetEntityKindFromID(guid)); ok {
		return rank
	}
	if GetEntityRemoteFromID(guid) {
		return int(EntityCategoryRemote)
	}
	return int(EntityCategoryUnknown)
}

// entityReleaseHook 是一个已注册的实体解锁回调。
type entityReleaseHook struct {
	id uint64
	fn func(IThreadSafeEntity)
}

// RegisterOnEntityRelease 注册一个在实体解锁前（仍持锁时）执行的回调，返回注销函数。
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

// runOnEntityRelease 按注册顺序执行实体所属 Manager 的解锁回调（回调列表先复制，回调内可注册 / 注销）。
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

// EntityGuard 记录一个 goroutine 在 Guard 作用域内持有的实体锁，按锁序等级避免死锁。只由持有它的业务 goroutine 使用；
// 作用域结束时释放全部锁、执行 post-release 回调并归还池。
type EntityGuard struct {
	syncMutation *SyncMutation
	// createdCapturer 是当前在本 Guard 上执行的 Nest handler（事务或 memory 快路径）；只由持有 Guard 的
	// 业务 goroutine 读写。非 nil 时 handler 内新建实体沿用本 Guard 持锁并按锁序取锁。
	createdCapturer CreatedEntityCapturer
	// eMap 以 ID 记当前持有的实例。同 ID 的另一实例若换了锁（handler 内 Destroy 后 LockManager 摘掉了旧锁，
	// 重建的实例拿到新锁），取得新锁后新实例进 eMap，旧实例连同它仍被本 Guard 持有的锁移到 superseded，
	// Guard 释放时一并解锁。
	// 契约（维护者 2026-09-30 定）：账本只按 ID、不按 EntityManager——一个 handler 不跨 Manager 持有同 ID 的
	// 实体。同一 Guard 先持有 Manager A 上的 X、再对 Manager B 上同 ID 的 X 取锁时，按“同 ID 换了实例”处理（B 上的 X 进 eMap、
	// A 上的转入 superseded），锁序判断同样按 eMap 条目；这不是支持的用法，框架不为它收紧也不为它扩大记账。
	eMap        map[int64]IThreadSafeEntity
	superseded  []heldEntity
	postRelease []func()
	// revokedCreated 是本 Guard 上撤销了发布、收尾（销毁回调、回收 LockManager 条目与 removing 标记）挂在本 Guard
	// post-release 里尚未执行的新建实体（revokeCreated）。同一 handler 里在同一个 EntityManager 上再建这些 ID 会撞上
	// 自己留下的 removing，那是确定失败，不是别的持有者的暂时状态。取走 post-release 回调时清空。
	// 以（EntityManager, ID）为键：removing 按 EntityManager 记，Manager A 上的撤销与 Manager B 上同 ID 的 removing
	// 无关，不能把 B 上别的持有者的收尾窗口误判成本 handler 自己撤销。
	revokedCreated []revokedEntity
}

// revokedEntity 是本 Guard 上撤销、收尾未执行的一次新建：removing 标记留在 manager 上。
type revokedEntity struct {
	manager *EntityManager
	id      int64
}

// heldEntity 是被同 ID 新实例取代、但锁仍由本 Guard 持有的旧实例。id 单独保存：旧实例可能已被清理（ID 归零）。
type heldEntity struct {
	id  int64
	ent IThreadSafeEntity
}

// GuardScope 是一个 goroutine 上的 Guard 作用域：独占一个池化 Guard，嵌套时记住上一层。
type GuardScope struct {
	name  string
	guard *EntityGuard
	prev  *GuardScope
}

var guardPool = sync.Pool{
	New: func() any {
		return newEntityGuard()
	},
}

// guardScopes 按 goroutine 记录当前 Guard 作用域。
var guardScopes sync.Map // map[int64]*GuardScope

// NewGuardScope 在当前 goroutine 上开启一个新的 Guard 作用域，返回释放函数：释放全部锁、执行 post-release 回调、
// 恢复上一层作用域并归还 Guard。I/O worker 上调用会 panic（I/O 池不得取实体锁）。
func NewGuardScope(name string) (*GuardScope, func()) {
	if fctx.InIOWorker() {
		panic(fctx.ErrGuardInIOWorker)
	}
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

// WithGuardScope 在新的 Guard 作用域内执行 fn，结束时释放作用域。
func WithGuardScope(name string, fn func(*GuardScope) error) error {
	scope, release := NewGuardScope(name)
	defer release()
	if fn == nil {
		return nil
	}
	return fn(scope)
}

// CurrentGuardScope 返回当前 goroutine 的 Guard 作用域；没有时为 nil。
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

// storeGuardScope 把 scope 设为当前 goroutine 的作用域；nil 表示清除。
func storeGuardScope(scope *GuardScope) {
	if scope == nil {
		guardScopes.Delete(goroutine.GoID())
		return
	}
	guardScopes.Store(goroutine.GoID(), scope)
}

// releaseGuardScope 结束 scope：释放 Guard 持有的全部锁、恢复上一层作用域，然后执行 post-release 回调并归还 Guard。
// scope 不是当前作用域（乱序释放）时只告警不释放。
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

// GetEntityGuard 返回当前 Guard 作用域的 Guard；没有作用域时返回一个独立 Guard，调用方必须用 EntityGuardRelease 释放。
func GetEntityGuard() *EntityGuard {
	if scope := CurrentGuardScope(); scope != nil {
		return scope.guard
	}
	return guardPool.Get().(*EntityGuard)
}

// EntityGuardRelease 释放 guard。它属于当前作用域时什么也不做（由作用域释放）；否则立即释放。
func EntityGuardRelease(guard *EntityGuard) {
	if guard == nil {
		return
	}
	if scope := CurrentGuardScope(); scope != nil && scope.guard == guard {
		return
	}
	doGuardReleaseInCurrentGoroutine(guard)
}

// doGuardReleaseInCurrentGoroutine 用一个临时作用域包住独立 Guard，走与作用域相同的释放流程。
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

// clean 清空 Guard 的全部状态，准备归还池。
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

// RequireEntity 等待取得实体锁，成功返回 true。已持有时直接返回 true；取锁后发现实体已清理或已摘除时解锁并返回 false。
// I/O worker 上调用会 panic。
func (e *EntityGuard) RequireEntity(ent IThreadSafeEntity) bool {
	if fctx.InIOWorker() {
		panic(fctx.ErrGuardInIOWorker)
	}
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
	if fctx.InIOWorker() {
		panic(fctx.ErrGuardInIOWorker)
	}
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

// requirePointerEntity 校验实体实现是指针（契约）。holding 与 doReleaseEntity 直接比较接口值（current == ent）：指针恒可
// 比较；值类型实现若含 func / map / slice 字段，同 ID 两个实例在同一 Guard 相遇时 == 会在运行期 panic。维护者决定不改热路径
// 的比较，改为在实体进入框架的入口（BuildEntity、EntityManager.TryAdd）校验一次：只看动态类型的 Kind，不做逐字段反射；
// 可比较的值类型同样拒绝——值类型实体每次经接口传递都是一份拷贝，“同一实例”没有意义。
func requirePointerEntity(ent IThreadSafeEntity) error {
	if ent == nil {
		return ErrEntityNil
	}
	if reflect.TypeOf(ent).Kind() != reflect.Pointer {
		return fmt.Errorf("%w: %T is a value type (the guard compares entity instances)", ErrEntityNotPointer, ent)
	}
	return nil
}

// holding 按实例判断本 Guard 是否已持有 ent 的锁：同一实例，或同 ID 且共用同一把锁的另一实例
// （Destroy 之前重复构建的实例）都算已持有。同 ID 但锁不同的旧实例作为 stale 返回——它的锁仍由本 Guard 持有，
// 但不代表 ent 的锁：只按 ID 判断会让新实例在未加锁的情况下被当作已持有。
// current == ent 直接比较接口值：实体实现必须是指针（契约，入口由 requirePointerEntity 校验），
// 这里不再做可比较性判断。
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

// sameMutex 报告 a、b 是否是同一把锁。直接用 == 比较两个 lock.Mutex 接口值不安全：自定义 Mutex 若是不可比较的值类型
// （含 func / slice / map 字段、值接收者），== 会在运行期 panic。
// 框架自己的锁都是 *lock.ReentrantMutex，先按指针比较，不经反射；只有其他类型才用 reflect 判断值是否可比较。
// 可比较性按值判定（reflect.Value.Comparable），不按类型：含接口字段的结构体在类型层面可比较，但接口里装着 func 等
// 不可比较的动态值时 == 仍会 panic；Value.Comparable 检查接口字段的动态值，为 true 时与任意值做 == 都不 panic。
// 不可比较的值无法判定是否同一把锁，按“不是同一把锁”处理：新实例自己加锁，被取代的
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
	return ta == nil || (reflect.ValueOf(a).Comparable() && a == b)
}

// hold 记下刚取得锁的实例；stale 非 nil 时它仍被持有的锁转入 superseded，Guard 释放时解锁。
func (e *EntityGuard) hold(id int64, ent, stale IThreadSafeEntity) {
	if stale != nil {
		e.superseded = append(e.superseded, heldEntity{id: id, ent: stale})
	}
	e.eMap[id] = ent
}

// lockCreated 取得新建实体的锁。Nest handler 内（capturer 非 nil）与 Cast 相同的锁序：
// 新实体的锁组高于本 Guard 已持有的全部锁组时可以等待——与 Cast 一样是按全序的有序等待，不会成环，属于
// Guard/本地锁豁免；否则等待可能与持有者成环（交叉创建、创建后再 Cast 更高锁组），只 try-lock，被占用时
// 由 capturer 给出交给业务的错误（可回滚事务整条回滚后重新准入；不能回滚的 handler 不重排），
// 不在业务 worker 上等待。
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

// CheckContainAllLock 报告 es 中的实体是否都已持有或按锁序可以等待。锁序按分类等级从低到高
// （Remote → Player → Alliance → Other）；已持有更高等级后，不能再经 Cast 取更低或同级的锁。
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

// CheckContainAllIDs 在加载实体之前按 ID 做同样的锁序检查。
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

// maxLockedGroup 返回本 Guard 持有的锁中最高的锁序等级；没有持有时为 -1。
func (e *EntityGuard) maxLockedGroup() int {
	maxLockedGroup := -1
	for id := range e.eMap {
		if group := GetEntityGroup(id); group > maxLockedGroup {
			maxLockedGroup = group
		}
	}
	return maxLockedGroup
}

// mayLock 报告现在取 id 的锁是否保持锁序：已持有，或它的等级高于已持有的全部锁。它按 ID 判断，因为在实体加载之前运行
// （CheckContainAllIDs）；拿到实例后由 mayLockEntity 按实例判断。
func (e *EntityGuard) mayLock(id int64, maxLockedGroup int) bool {
	if _, held := e.eMap[id]; held {
		return true
	}
	return GetEntityGroup(id) > maxLockedGroup
}

// mayLockEntity 是按实例的 mayLock：同 ID 但换了锁的新实例不是“已持有”，而它的锁组与
// 本 Guard 持有的旧实例相同，所以不满足锁序——只能 try-lock 或拒绝，不能等待，更不能跳过加锁。
func (e *EntityGuard) mayLockEntity(ent IThreadSafeEntity, maxLockedGroup int) bool {
	if held, _ := e.holding(ent); held {
		return true
	}
	return GetEntityGroup(ent.GUId()) > maxLockedGroup
}

// AppendPostRelease 登记在 Guard 释放全部锁之后执行的回调（每个回调独立恢复 panic）。
func (e *EntityGuard) AppendPostRelease(f func()) {
	if f != nil {
		e.postRelease = append(e.postRelease, f)
	}
}

// ReleaseEntity 按 ID 提前释放 id 当前登记的实例，不论调用方手里是哪个实例。同 ID 被它取代、锁仍由本 Guard 持有的旧实例
// 随后回到 eMap：锁序判断与 Guarded 仍要算上这把锁，Guard 释放时解锁。
// 调用方持有实例指针、要释放的是“这个实例的锁”时用 ReleaseEntityInstance：handler 内 Destroy 后同 ID 重建，
// 按 ID 释放放掉的是新实例的锁。
func (e *EntityGuard) ReleaseEntity(id int64) {
	ent := e.eMap[id]
	if ent != nil {
		e.doReleaseEntity(id, ent)
		e.restoreSuperseded(id)
	}
}

// ReleaseEntityInstance 提前释放本 Guard 为 ent 这个实例持有的锁，别的实例的锁不动：
//   - ent 是 id 当前登记的实例（或与它共用同一把锁）：同 ReleaseEntity；
//   - ent 已被同 ID 的新实例取代、锁在 superseded 里：只释放这把旧锁，新实例仍被持有；
//   - 本 Guard 没有为 ent 登记锁：什么也不做。
//
// 按 ID 释放不够：handler 内 Destroy X 再新建同 ID 的 X 后，旧 X 的清理若被别的访问者的 Touch 推迟（最后一次 UnTouch
// 才清零 ID），旧 X 的 GUId 仍非零，按它的 ID 释放会在 handler 中途放掉新 X 的锁。
// ent 的 ID 已清零时它不会是 eMap 的当前登记（按 ID 查不到），只查 superseded。
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

// restoreSuperseded 把 id 最近一个被取代、锁仍持有的旧实例放回 eMap。
func (e *EntityGuard) restoreSuperseded(id int64) {
	for i := len(e.superseded) - 1; i >= 0; i-- {
		if held := e.superseded[i]; held.id == id {
			e.superseded = append(e.superseded[:i], e.superseded[i+1:]...)
			e.eMap[id] = held.ent
			return
		}
	}
}

// ReleaseAll 释放本 Guard 持有的全部锁并执行 post-release 回调；Guard 本身不归还池。
func (e *EntityGuard) ReleaseAll() {
	callbacks := e.releaseEntities()
	e.runPostRelease(callbacks)
}

// releaseEntities 解锁全部持有的实例（含被取代的旧实例，每把独立恢复 panic），取走 post-release 回调返回给调用方。
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

// revokedInThisGuard 报告 manager 上的 id 是否是本 Guard 上撤销、收尾尚未执行的新建实体。只认同一个 EntityManager 上的撤销。
func (e *EntityGuard) revokedInThisGuard(manager *EntityManager, id int64) bool {
	return e != nil && slices.Contains(e.revokedCreated, revokedEntity{manager: manager, id: id})
}

// runPostRelease 依次执行回调，每个回调独立恢复 panic 并记录日志。
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

// safeReleaseEntity 是带 panic 恢复的 doReleaseEntity（解锁回调 panic 时锁仍释放）。
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

	// 解锁回调在仍持锁时执行。
	runOnEntityRelease(ent)
}

// Guarded 按 ID 报告本 Guard 是否持有该实体的锁。这是框架热路径使用的查询：不暴露 Guard 的账本，也不分配。
func (e *EntityGuard) Guarded(id int64) bool {
	if e == nil {
		return false
	}
	_, held := e.eMap[id]
	return held
}

// GuardedEntity 报告本 Guard 是否持有 ent 自己的锁：同一实例，或同 ID 且共用所持锁的另一实例。Guarded 只按 ID 回答；
// handler 销毁并重建同一 ID 后，重建的实例有新锁，加锁之前不算持有。要对手里的实例指针跳过加锁时必须问这个，而不是 Guarded。
func (e *EntityGuard) GuardedEntity(ent IThreadSafeEntity) bool {
	if e == nil || ent == nil {
		return false
	}
	held, _ := e.holding(ent)
	return held
}

// GuardedCount 返回本 Guard 当前持有的实体数。
func (e *EntityGuard) GuardedCount() int {
	if e == nil {
		return 0
	}
	return len(e.eMap)
}

// Entities 返回当前持有实体的快照。
//
// 有意返回副本：eMap 是驱动 Nest 锁序与重入判断的账本，交出活的 map 会让任何调用方一次误删就破坏死锁预防。只需要
// 成员判断或计数的调用方应使用不分配的 Guarded / GuardedCount；这个用于诊断与遍历。
func (e *EntityGuard) Entities() map[int64]IThreadSafeEntity {
	if e == nil {
		return nil
	}
	// 不用 maps.Clone：池化 Guard 的 eMap 经 clear 复用会保留历史容量，克隆会连同容量一起复制。
	out := make(map[int64]IThreadSafeEntity, len(e.eMap))
	for id, ent := range e.eMap {
		out[id] = ent
	}
	return out
}
