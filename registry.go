package actionflow

import (
	"errors"
	"fmt"
	"sync"

	coreflow "github.com/tjbdwanghaibo/cube-core/actionflow"
)

var (
	ErrRegistrySealed          = errors.New("taskflow: registry is sealed")
	ErrKindInvalid             = errors.New("taskflow: kind is invalid")
	ErrBuilderNil              = errors.New("taskflow: builder is nil")
	ErrActionBuilderDuplicate  = errors.New("taskflow: duplicate action builder")
	ErrActionBuilderNotFound   = errors.New("taskflow: action builder not found")
	ErrMissionBuilderDuplicate = errors.New("taskflow: duplicate mission builder")
	ErrMissionBuilderNotFound  = errors.New("taskflow: mission builder not found")
)

type ActionBuilder func(param any) (coreflow.Action, error)
type MissionBuilder func() coreflow.Mission

// Registry is instance-scoped and can be sealed after process bootstrap.
// This avoids hidden mutable global registration during tests and hot reloads.
type Registry struct {
	mu       sync.RWMutex
	sealed   bool
	actions  map[coreflow.ActionKind]ActionBuilder
	missions map[coreflow.MissionKind]MissionBuilder
}

func NewRegistry() *Registry {
	return &Registry{
		actions:  make(map[coreflow.ActionKind]ActionBuilder),
		missions: make(map[coreflow.MissionKind]MissionBuilder),
	}
}

func (r *Registry) Seal() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.sealed = true
	r.mu.Unlock()
}

func (r *Registry) RegisterAction(kind coreflow.ActionKind, builder ActionBuilder) error {
	if r == nil || kind == 0 {
		return ErrKindInvalid
	}
	if builder == nil {
		return ErrBuilderNil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sealed {
		return ErrRegistrySealed
	}
	if _, exists := r.actions[kind]; exists {
		return fmt.Errorf("%w: %d", ErrActionBuilderDuplicate, kind)
	}
	r.actions[kind] = builder
	return nil
}

func (r *Registry) RegisterMission(kind coreflow.MissionKind, builder MissionBuilder) error {
	if r == nil || kind == 0 {
		return ErrKindInvalid
	}
	if builder == nil {
		return ErrBuilderNil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sealed {
		return ErrRegistrySealed
	}
	if _, exists := r.missions[kind]; exists {
		return fmt.Errorf("%w: %d", ErrMissionBuilderDuplicate, kind)
	}
	r.missions[kind] = builder
	return nil
}

func (r *Registry) BuildAction(kind coreflow.ActionKind, param any) (action coreflow.Action, err error) {
	if r == nil {
		return nil, ErrActionBuilderNotFound
	}
	r.mu.RLock()
	builder := r.actions[kind]
	r.mu.RUnlock()
	if builder == nil {
		return nil, fmt.Errorf("%w: %d", ErrActionBuilderNotFound, kind)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			action = nil
			err = fmt.Errorf("taskflow: action builder %d panic: %v", kind, recovered)
		}
	}()
	action, err = builder(param)
	if err == nil && action == nil {
		err = ErrBuilderNil
	}
	return action, err
}

func (r *Registry) BuildMission(kind coreflow.MissionKind) (mission coreflow.Mission, err error) {
	if r == nil {
		return nil, ErrMissionBuilderNotFound
	}
	r.mu.RLock()
	builder := r.missions[kind]
	r.mu.RUnlock()
	if builder == nil {
		return nil, fmt.Errorf("%w: %d", ErrMissionBuilderNotFound, kind)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			mission = nil
			err = fmt.Errorf("taskflow: mission builder %d panic: %v", kind, recovered)
		}
	}()
	mission = builder()
	if mission == nil {
		return nil, ErrBuilderNil
	}
	return mission, nil
}
