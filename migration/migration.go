package migration

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

var (
	ErrStepInvalid = errors.New("migration: step invalid")
	ErrPathMissing = errors.New("migration: path missing")
)

type Versioned interface {
	DataVersion() int32
	SetDataVersion(int32)
}

type Step struct {
	Name  string
	From  int32
	To    int32
	Apply func(context.Context, any) error
}

type Registry struct {
	mu    sync.RWMutex
	steps map[int32]Step
}

func NewRegistry() *Registry {
	return &Registry{steps: make(map[int32]Step)}
}

func (r *Registry) Register(step Step) error {
	if r == nil {
		return fmt.Errorf("%w: registry nil", ErrStepInvalid)
	}
	if step.From < 0 || step.To <= step.From {
		return fmt.Errorf("%w: invalid version %d -> %d", ErrStepInvalid, step.From, step.To)
	}
	if step.Apply == nil {
		return fmt.Errorf("%w: apply nil %d -> %d", ErrStepInvalid, step.From, step.To)
	}
	if step.Name == "" {
		step.Name = fmt.Sprintf("%d_to_%d", step.From, step.To)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.steps[step.From]; exists {
		return fmt.Errorf("%w: duplicate from %d", ErrStepInvalid, step.From)
	}
	r.steps[step.From] = step
	return nil
}

func (r *Registry) MustRegister(step Step) {
	if err := r.Register(step); err != nil {
		panic(err)
	}
}

func (r *Registry) Run(ctx context.Context, data Versioned, target int32) error {
	if data == nil {
		return fmt.Errorf("%w: data nil", ErrStepInvalid)
	}
	return r.RunFrom(ctx, data, data.DataVersion(), target)
}

func (r *Registry) RunFrom(ctx context.Context, data Versioned, from int32, target int32) error {
	if r == nil {
		return fmt.Errorf("%w: registry nil", ErrStepInvalid)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if data == nil {
		return fmt.Errorf("%w: data nil", ErrStepInvalid)
	}
	if target < from {
		return fmt.Errorf("%w: downgrade %d -> %d", ErrStepInvalid, from, target)
	}
	cur := from
	for cur < target {
		if err := ctx.Err(); err != nil {
			return err
		}
		step, ok := r.step(cur)
		if !ok {
			return fmt.Errorf("%w: %d -> %d", ErrPathMissing, cur, target)
		}
		if step.To > target {
			return fmt.Errorf("%w: step %s overshoots target %d", ErrPathMissing, step.Name, target)
		}
		if err := step.Apply(ctx, data); err != nil {
			return fmt.Errorf("migration: apply %s: %w", step.Name, err)
		}
		cur = step.To
		data.SetDataVersion(cur)
	}
	return nil
}

func (r *Registry) Steps() []Step {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	steps := make([]Step, 0, len(r.steps))
	for _, step := range r.steps {
		steps = append(steps, step)
	}
	sort.Slice(steps, func(i, j int) bool { return steps[i].From < steps[j].From })
	return steps
}

func (r *Registry) step(from int32) (Step, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	step, ok := r.steps[from]
	return step, ok
}
