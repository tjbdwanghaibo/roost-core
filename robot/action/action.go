// Package action hosts the robot's named blocking actions. Ported from the
// cube robot service; the generic RegisterCall replaces cube's ~170
// hand-written per-protocol actions with one convention-driven line each.
package action

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/tjbdwanghaibo/cube-core/robot"
)

type Action interface {
	Name() string
	Run(context.Context, *robot.Context, any) error
}

type Func struct {
	ActionName string
	Handle     func(context.Context, *robot.Context, any) error
}

func (f Func) Name() string { return f.ActionName }

func (f Func) Run(ctx context.Context, r *robot.Context, param any) error {
	if f.Handle == nil {
		return nil
	}
	return f.Handle(ctx, r, param)
}

type Registry struct {
	mu      sync.RWMutex
	actions map[string]Action
}

func NewRegistry() *Registry {
	r := &Registry{actions: make(map[string]Action)}
	registerBuiltins(r)
	return r
}

func (r *Registry) Register(a Action) error {
	if r == nil {
		return fmt.Errorf("robot action: registry is nil")
	}
	if a == nil {
		return fmt.Errorf("robot action: action is nil")
	}
	name := normalizeName(a.Name())
	if name == "" {
		return fmt.Errorf("robot action: name is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.actions[name]; ok {
		return fmt.Errorf("robot action: duplicate %q", name)
	}
	r.actions[name] = a
	return nil
}

func (r *Registry) MustRegister(a Action) {
	if err := r.Register(a); err != nil {
		panic(err)
	}
}

func (r *Registry) Run(ctx context.Context, rb *robot.Context, name string, param any) error {
	if r == nil {
		return fmt.Errorf("robot action: registry is nil")
	}
	name = normalizeName(name)
	r.mu.RLock()
	a := r.actions[name]
	r.mu.RUnlock()
	if a == nil {
		return fmt.Errorf("robot action: %q not found (registered: %v)", name, r.Names())
	}
	if err := a.Run(ctx, rb, param); err != nil {
		if strings.HasPrefix(err.Error(), "robot action ") {
			return err
		}
		return fmt.Errorf("robot action %s: %w", name, err)
	}
	return nil
}

func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	names := make([]string, 0, len(r.actions))
	for name := range r.actions {
		names = append(names, name)
	}
	r.mu.RUnlock()
	sort.Strings(names)
	return names
}

func normalizeName(name string) string {
	return strings.TrimSpace(strings.ToLower(name))
}
