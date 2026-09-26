package lifecycle

import (
	"context"
	"errors"
	"fmt"

	scene "example.com/planet/game/entities/scene"
	"github.com/tjbdwanghaibo/roost-core/app"
	engine "github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
)

var ErrSceneNotFound = errors.New("scene lifecycle: Entity not found")

// SceneLifecycle owns the explicit create/load/destroy boundary for Scene.
// Request handlers should use generated Nest senders for ordinary reads and
// writes; this type is for authentication/role creation and administration.
type SceneLifecycle struct {
	access *entity.ManagerAccess
}

func NewSceneLifecycle(access *entity.ManagerAccess) (*SceneLifecycle, error) {
	if access == nil || access.Manager() == nil {
		return nil, fmt.Errorf("scene lifecycle: Entity access is required")
	}
	return &SceneLifecycle{access: access}, nil
}

// SceneFromRegistry is the normal application entry point. Nest publishes the
// instance-scoped ManagerAccess as entity.runtime, so lifecycle code never
// depends on package globals or reaches into a Service implementation.
func SceneFromRegistry(registry *app.Registry) (*SceneLifecycle, error) {
	if registry == nil {
		return nil, fmt.Errorf("scene lifecycle: app registry is required")
	}
	access, ok := app.Lookup[*entity.ManagerAccess](registry, mods.ModEntityRuntime)
	if !ok || access == nil {
		return nil, fmt.Errorf("scene lifecycle: Entity runtime is unavailable")
	}
	return NewSceneLifecycle(access)
}

func (lifecycle *SceneLifecycle) Get(ctx context.Context, uniqueID int64) (*scene.Scene, error) {
	fullID, err := entity.BuildEntityID(uniqueID, scene.EntityKindScene)
	if err != nil {
		return nil, err
	}
	value, err := lifecycle.access.Get(ctx, fullID, entity.MustEntityCategoryOfKind(scene.EntityKindScene))
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, ErrSceneNotFound
	}
	typed, ok := value.(*scene.Scene)
	if !ok {
		return nil, fmt.Errorf("scene lifecycle: Entity %d has type %T", fullID, value)
	}
	return typed, nil
}

func (lifecycle *SceneLifecycle) Create(ctx context.Context, uniqueID int64) (*scene.Scene, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	value, err := lifecycle.access.Create(&entity.EntityCreateParam{
		IsCreate: true,
		UniqueID: uniqueID,
		Kind:     scene.EntityKindScene,
		Category: entity.MustEntityCategoryOfKind(scene.EntityKindScene),
	})
	if err != nil {
		return nil, err
	}
	typed, ok := value.(*scene.Scene)
	if !ok {
		return nil, fmt.Errorf("scene lifecycle: created Entity has type %T", value)
	}
	return typed, nil
}

func (lifecycle *SceneLifecycle) GetOrCreate(ctx context.Context, uniqueID int64) (*scene.Scene, bool, error) {
	value, err := lifecycle.Get(ctx, uniqueID)
	if err == nil {
		return value, false, nil
	}
	if !errors.Is(err, engine.ErrEntityAggregateNotFound) && !errors.Is(err, ErrSceneNotFound) {
		return nil, false, err
	}
	value, err = lifecycle.Create(ctx, uniqueID)
	if errors.Is(err, entity.ErrEntityExists) {
		value, err = lifecycle.Get(ctx, uniqueID)
		return value, false, err
	}
	return value, err == nil, err
}

func (lifecycle *SceneLifecycle) Destroy(ctx context.Context, value *scene.Scene, reason entity.EntityDestroyReason, deletePersisted bool) error {
	if value == nil {
		return ErrSceneNotFound
	}
	return lifecycle.access.Destroy(ctx, value, reason, deletePersisted)
}
