package actionflow

import (
	"errors"
	"fmt"
	"sync"
)

var (
	ErrRegistrySealed          = errors.New("actionflow: registry is sealed")
	ErrKindInvalid             = errors.New("actionflow: kind is invalid")
	ErrBuilderNil              = errors.New("actionflow: builder is nil")
	ErrActionBuilderDuplicate  = errors.New("actionflow: duplicate action builder")
	ErrActionBuilderNotFound   = errors.New("actionflow: action builder not found")
	ErrMissionBuilderDuplicate = errors.New("actionflow: duplicate mission builder")
	ErrMissionBuilderNotFound  = errors.New("actionflow: mission builder not found")
)

// ActionBuilder 按参数构造动作，MissionBuilder 构造任务。
type ActionBuilder func(param any) (Action, error)
type MissionBuilder func() Mission

// Registry 是实例级的，可以在进程启动完成后封存，测试与热重载期间不存在隐藏的可变全局注册表。
type Registry struct {
	mu       sync.RWMutex
	sealed   bool
	actions  map[ActionKind]ActionBuilder
	missions map[MissionKind]MissionBuilder
}

// NewRegistry 返回空的注册表。
func NewRegistry() *Registry {
	return &Registry{
		actions:  make(map[ActionKind]ActionBuilder),
		missions: make(map[MissionKind]MissionBuilder),
	}
}

// Seal 封存注册表，之后的注册返回错误。
func (r *Registry) Seal() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.sealed = true
	r.mu.Unlock()
}

// RegisterAction 注册动作构造器。
func (r *Registry) RegisterAction(kind ActionKind, builder ActionBuilder) error {
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

// RegisterMission 注册任务构造器。
func (r *Registry) RegisterMission(kind MissionKind, builder MissionBuilder) error {
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

// BuildAction 按种类构造动作；构造器的 panic 恢复成错误。
func (r *Registry) BuildAction(kind ActionKind, param any) (action Action, err error) {
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
			err = fmt.Errorf("actionflow: action builder %d panic: %v", kind, recovered)
		}
	}()
	action, err = builder(param)
	if err == nil && action == nil {
		err = ErrBuilderNil
	}
	return action, err
}

// BuildMission 按种类构造任务；构造器的 panic 恢复成错误。
func (r *Registry) BuildMission(kind MissionKind) (mission Mission, err error) {
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
			err = fmt.Errorf("actionflow: mission builder %d panic: %v", kind, recovered)
		}
	}()
	mission = builder()
	if mission == nil {
		return nil, ErrBuilderNil
	}
	return mission, nil
}
