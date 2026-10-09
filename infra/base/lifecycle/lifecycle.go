package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

type Phase string

const (
	PhaseAppInit         Phase = "app.init"
	PhaseModsStarted     Phase = "mods.started"
	PhaseServiceStarted  Phase = "service.started"
	PhaseServiceStopping Phase = "service.stopping"
	PhaseServiceStopped  Phase = "service.stopped"
	PhaseConfigReload    Phase = "config.reload"
)

type Event struct {
	Phase   Phase
	Service string
	Name    string
	Data    map[string]any
}

type Handler func(context.Context, Event) error

type Hook struct {
	Name    string
	Phase   Phase
	Order   int
	Handler Handler
}

type Registry struct {
	mu    sync.RWMutex
	hooks map[Phase][]Hook
}

var defaultRegistry = NewRegistry()

func DefaultRegistry() *Registry { return defaultRegistry }

func Register(hook Hook) error                       { return DefaultRegistry().Register(hook) }
func Emit(ctx context.Context, event Event) error    { return DefaultRegistry().Emit(ctx, event) }
func EmitAll(ctx context.Context, event Event) error { return DefaultRegistry().EmitAll(ctx, event) }

func NewRegistry() *Registry {
	return &Registry{hooks: make(map[Phase][]Hook)}
}

func (r *Registry) Register(hook Hook) error {
	if r == nil {
		return fmt.Errorf("lifecycle: registry nil")
	}
	if hook.Phase == "" {
		return fmt.Errorf("lifecycle: phase required")
	}
	if hook.Handler == nil {
		return fmt.Errorf("lifecycle: handler required")
	}
	named := hook.Name != ""
	if !named {
		hook.Name = "anonymous"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if named {
		for i := range r.hooks[hook.Phase] {
			if r.hooks[hook.Phase][i].Name == hook.Name {
				r.hooks[hook.Phase][i] = hook
				sortHooks(r.hooks[hook.Phase])
				return nil
			}
		}
	}
	r.hooks[hook.Phase] = append(r.hooks[hook.Phase], hook)
	sortHooks(r.hooks[hook.Phase])
	return nil
}

func sortHooks(hooks []Hook) {
	sort.SliceStable(hooks, func(i, j int) bool {
		return hooks[i].Order < hooks[j].Order
	})
}

func (r *Registry) Emit(ctx context.Context, event Event) error {
	if r == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.RLock()
	hooks := append([]Hook(nil), r.hooks[event.Phase]...)
	r.mu.RUnlock()
	for _, hook := range hooks {
		if err := emitHook(ctx, event, hook); err != nil {
			return fmt.Errorf("lifecycle: %s/%s: %w", event.Phase, hook.Name, err)
		}
	}
	return nil
}

// EmitAll runs every hook and joins failures. It is intended for cleanup
// phases, where one broken hook must not prevent later resources from closing.
func (r *Registry) EmitAll(ctx context.Context, event Event) error {
	return r.EmitAllWatched(ctx, event, nil)
}

// EmitAllWatched 与 EmitAll 相同，另外在每个 hook 开始前用它的名字调用 starting（nil 时不调用）。
// 停机方在预算内等 hook、到期不能杀掉它时，靠最后一次 starting 知道卡住的是哪一个，写进停机错误
// （RR-20261006-25；之前只报阶段，同一阶段有多个 hook 时运维无从定位）。starting 在派发 hook 的
// goroutine 里同步调用，不能阻塞。
func (r *Registry) EmitAllWatched(ctx context.Context, event Event, starting func(hook string)) error {
	if r == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.RLock()
	hooks := append([]Hook(nil), r.hooks[event.Phase]...)
	r.mu.RUnlock()
	var result error
	for _, hook := range hooks {
		if starting != nil {
			starting(hook.Name)
		}
		if err := emitHook(ctx, event, hook); err != nil {
			result = errors.Join(result, fmt.Errorf("lifecycle: %s/%s: %w", event.Phase, hook.Name, err))
		}
	}
	return result
}

func emitHook(ctx context.Context, event Event, hook Hook) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return hook.Handler(ctx, event)
}
