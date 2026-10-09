package hotcode

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrNameRequired = errors.New("hotcode: name required")
	ErrFuncRequired = errors.New("hotcode: function required")
	ErrNotFound     = errors.New("hotcode: patch point not found")
	ErrTypeMismatch = errors.New("hotcode: function signature mismatch")
	ErrDuplicate    = errors.New("hotcode: duplicate patch point")
)

// Meta describes an active hot-code patch.
type Meta struct {
	Version  string
	Author   string
	Reason   string
	Source   string
	LoadedAt time.Time
}

// PointInfo is a read-only view of one registered patch point.
type PointInfo struct {
	Name       string
	Type       string
	Generation uint64
	Patched    bool
	Meta       Meta
	// ResolveMismatches 是 Resolve[T] 因 T 与注册签名不符而回落到 fallback 的次数：非零表示
	// 有调用方用错了类型，这个点的补丁对它永远不生效（RR-20261005-NC-247）。
	ResolveMismatches uint64
}

// pointState 是一个补丁点在某一代的完整状态：当前函数、对应的 Meta、是否打了补丁与代数。
// 它整体发布、发布后不再修改，读者一次 Load 拿到的各项总是同一次 Replace / Revert 写下的。
//
// patched 由写者记录（Replace 为真、Revert 为假）。之前 List 用函数代码指针与原函数比较，
// 同一函数字面量 / 工厂产生、捕获值不同的闭包代码指针相同，补丁在生效却报成未打补丁
// （RR-20261005-NC-246）。
type pointState struct {
	fn      any
	meta    Meta
	patched bool
	gen     uint64
}

// point 是一个补丁点。之前 current / meta / gen 是三个独立的原子值，并发的 Replace 与
// Revert 交错写入后会永久留下“当前是原函数、Meta 却是补丁版本”或反过来的组合，List
// 报出的 Patched 与 Meta 互相矛盾（RR-20261005-NC-123）。现在写者在 writeMu 下基于上一代
// 生成新的 pointState 整体替换；Resolve / List 不取锁，只读 state。
type point struct {
	name       string
	fnType     reflect.Type
	original   any
	writeMu    sync.Mutex
	state      atomic.Pointer[pointState]
	mismatches atomic.Uint64
}

// publish 在 writeMu 下发布下一代状态，代数在上一代基础上加一。
func (p *point) publish(fn any, meta Meta, patched bool) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	p.storeNext(fn, meta, patched)
}

func (p *point) storeNext(fn any, meta Meta, patched bool) {
	next := &pointState{fn: fn, meta: meta, patched: patched}
	if previous := p.state.Load(); previous != nil {
		next.gen = previous.gen + 1
	}
	p.state.Store(next)
}

// restore 把点恢复成 saved 那一代的函数、Meta 与补丁标记（作为新的一代发布）。当前已经
// 是 saved 时不动，返回是否改动。
func (p *point) restore(saved *pointState) bool {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if saved == nil || p.state.Load() == saved {
		return false
	}
	p.storeNext(saved.fn, saved.meta, saved.patched)
	return true
}

// Registry owns all hot-code patch points for one process.
type Registry struct {
	mu     sync.RWMutex
	points map[string]*point
	// applyMu 串行 ApplyBundle（插件加载）。
	applyMu sync.Mutex
}

// Default is the process-wide hot-code registry.
var Default = NewRegistry()

func NewRegistry() *Registry {
	return &Registry{points: make(map[string]*point)}
}

func Register(name string, fn any) error {
	return Default.Register(name, fn)
}

func MustRegister(name string, fn any) {
	if err := Register(name, fn); err != nil {
		panic(err)
	}
}

func Replace(name string, fn any, meta Meta) error {
	return Default.Replace(name, fn, meta)
}

func Revert(name string) error {
	return Default.Revert(name)
}

// Resolve 返回补丁点当前的函数，没有注册这个点时返回 fallback。
//
// 注册的签名与 T 只差命名（func(int) int 与 type F func(int) int）时转换成 T 照常返回；
// 之前类型断言失败就静默回落 fallback，Replace 照样成功、hotcode.list 照样显示已打补丁，
// 这个调用方却永远拿不到补丁（RR-20261005-NC-247）。签名确实不同、无法转换时仍回落
// fallback，并计入该点的 PointInfo.ResolveMismatches。
func Resolve[T any](name string, fallback T) T {
	return resolveTyped(Default, name, fallback)
}

func resolveTyped[T any](r *Registry, name string, fallback T) T {
	p, err := r.point(name)
	if err != nil {
		return fallback
	}
	state := p.state.Load()
	if state == nil || state.fn == nil {
		return fallback
	}
	if typed, ok := state.fn.(T); ok {
		return typed
	}
	target := reflect.TypeFor[T]()
	value := reflect.ValueOf(state.fn)
	if target.Kind() == reflect.Func && value.Type().ConvertibleTo(target) {
		if typed, ok := value.Convert(target).Interface().(T); ok {
			return typed
		}
	}
	p.mismatches.Add(1)
	return fallback
}

func List() []PointInfo {
	return Default.List()
}

// Register creates a patch point. The original function remains available for
// revert and is used until Replace is called.
func (r *Registry) Register(name string, fn any) error {
	if name == "" {
		return ErrNameRequired
	}
	if fn == nil || reflect.TypeOf(fn).Kind() != reflect.Func {
		return ErrFuncRequired
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.points[name]; ok {
		return fmt.Errorf("%w: %s", ErrDuplicate, name)
	}
	p := &point{
		name:     name,
		fnType:   reflect.TypeOf(fn),
		original: fn,
	}
	p.state.Store(&pointState{fn: fn})
	r.points[name] = p
	return nil
}

func (r *Registry) Replace(name string, fn any, meta Meta) error {
	if name == "" {
		return ErrNameRequired
	}
	if fn == nil || reflect.TypeOf(fn).Kind() != reflect.Func {
		return ErrFuncRequired
	}

	p, err := r.point(name)
	if err != nil {
		return err
	}
	if got := reflect.TypeOf(fn); got != p.fnType {
		return fmt.Errorf("%w: %s want=%s got=%s", ErrTypeMismatch, name, p.fnType, got)
	}
	if meta.LoadedAt.IsZero() {
		meta.LoadedAt = time.Now()
	}
	p.publish(fn, meta, true)
	return nil
}

func (r *Registry) Revert(name string) error {
	p, err := r.point(name)
	if err != nil {
		return err
	}
	p.publish(p.original, Meta{}, false)
	return nil
}

func (r *Registry) Resolve(name string, fallback any) any {
	p, err := r.point(name)
	if err != nil {
		return fallback
	}
	state := p.state.Load()
	if state == nil || state.fn == nil {
		return fallback
	}
	return state.fn
}

func (r *Registry) List() []PointInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ret := make([]PointInfo, 0, len(r.points))
	for _, p := range r.points {
		info := PointInfo{Name: p.name, Type: p.fnType.String(), ResolveMismatches: p.mismatches.Load()}
		if state := p.state.Load(); state != nil {
			info.Generation = state.gen
			info.Meta = state.meta
			info.Patched = state.patched
		}
		ret = append(ret, info)
	}
	sort.Slice(ret, func(i, j int) bool { return ret[i].Name < ret[j].Name })
	return ret
}

// ApplyBundle 应用一个补丁包，失败（返回错误或 panic）时把 Apply 期间改过的补丁点恢复成
// 应用前的那一代（函数、Meta、补丁标记；不是原函数——Revert 只能回到原函数，会把应用前
// 已生效的补丁一起撤掉），返回恢复的点数与错误。成功时返回 0, nil。
//
// 之前 LoadPlugin 直接调 Bundle.Apply，中途失败只返回错误、丢掉 Bundle：已替换的点留在
// 插件版本，运维看到“加载失败”，hotcode.list 里却有点被替换了，也拿不到 Bundle 去调它的
// Revert（RR-20261005-NC-245，N10 观察 O-H1）。
//
// 同一 Registry 上的 ApplyBundle 互相串行。恢复按“应用前快照”判断改动，Apply 期间别处
// 对同一 Registry 的 Replace / Revert（例如直接调用的业务代码）也会被一起恢复；admin 的
// hotcode.revert 与 hotcode.load_plugin 已串行，不会交错。Apply 期间新注册的点保持原样。
func (r *Registry) ApplyBundle(bundle Bundle) (restored int, err error) {
	if bundle == nil {
		return 0, errors.New("hotcode: bundle is nil")
	}
	r.applyMu.Lock()
	defer r.applyMu.Unlock()
	before := r.snapshot()
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("hotcode: bundle apply panic: %v", recovered)
		}
		if err != nil {
			for p, saved := range before {
				if p.restore(saved) {
					restored++
				}
			}
		}
	}()
	return 0, bundle.Apply(r)
}

// revertBetweenApplies 在两次 ApplyBundle 之间执行 Revert（admin 的 hotcode.revert 用）。
func (r *Registry) revertBetweenApplies(name string) error {
	r.applyMu.Lock()
	defer r.applyMu.Unlock()
	return r.Revert(name)
}

// snapshot 记下每个补丁点当前那一代的状态指针。
func (r *Registry) snapshot() map[*point]*pointState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	saved := make(map[*point]*pointState, len(r.points))
	for _, p := range r.points {
		saved[p] = p.state.Load()
	}
	return saved
}

func (r *Registry) point(name string) (*point, error) {
	r.mu.RLock()
	p := r.points[name]
	r.mu.RUnlock()
	if p == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return p, nil
}

func (r *Registry) resetForTest() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.points = make(map[string]*point)
}

func ResetForTest() {
	Default.resetForTest()
}
