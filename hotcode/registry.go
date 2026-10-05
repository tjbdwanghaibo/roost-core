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
}

// pointState 是一个补丁点在某一代的完整状态：当前函数、对应的 Meta 与代数。它整体
// 发布、发布后不再修改，读者一次 Load 拿到的三项总是同一次 Replace / Revert 写下的。
type pointState struct {
	fn   any
	meta Meta
	gen  uint64
}

// point 是一个补丁点。之前 current / meta / gen 是三个独立的原子值，并发的 Replace 与
// Revert 交错写入后会永久留下“当前是原函数、Meta 却是补丁版本”或反过来的组合，List
// 报出的 Patched 与 Meta 互相矛盾（RR-20261005-NC-123）。现在写者在 writeMu 下基于上一代
// 生成新的 pointState 整体替换；Resolve / List 不取锁，只读 state。
type point struct {
	name     string
	fnType   reflect.Type
	original any
	writeMu  sync.Mutex
	state    atomic.Pointer[pointState]
}

// publish 在 writeMu 下发布下一代状态，代数在上一代基础上加一。
func (p *point) publish(fn any, meta Meta) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	next := &pointState{fn: fn, meta: meta}
	if previous := p.state.Load(); previous != nil {
		next.gen = previous.gen + 1
	}
	p.state.Store(next)
}

// Registry owns all hot-code patch points for one process.
type Registry struct {
	mu     sync.RWMutex
	points map[string]*point
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

func Resolve[T any](name string, fallback T) T {
	v := Default.Resolve(name, fallback)
	typed, ok := v.(T)
	if !ok {
		return fallback
	}
	return typed
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
	p.publish(fn, meta)
	return nil
}

func (r *Registry) Revert(name string) error {
	p, err := r.point(name)
	if err != nil {
		return err
	}
	p.publish(p.original, Meta{})
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
		info := PointInfo{Name: p.name, Type: p.fnType.String()}
		if state := p.state.Load(); state != nil {
			info.Generation = state.gen
			info.Meta = state.meta
			if state.fn != nil {
				info.Patched = reflect.ValueOf(state.fn).Pointer() != reflect.ValueOf(p.original).Pointer()
			}
		}
		ret = append(ret, info)
	}
	sort.Slice(ret, func(i, j int) bool { return ret[i].Name < ret[j].Name })
	return ret
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
