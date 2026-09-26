package lifecycle

import (
	"context"
	"errors"
	"fmt"

	world "example.com/planet/game/entities/world"
	"github.com/tjbdwanghaibo/roost-core/app"
	engine "github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
)

var ErrWorldNotFound = errors.New("world lifecycle: Entity not found")

// WorldLifecycle owns the explicit create/load/destroy boundary for World.
// Request handlers should use generated Nest senders for ordinary reads and
// writes; this type is for authentication/role creation and administration.
type WorldLifecycle struct {
	access *entity.ManagerAccess
}

func NewWorldLifecycle(access *entity.ManagerAccess) (*WorldLifecycle, error) {
	if access == nil || access.Manager() == nil {
		return nil, fmt.Errorf("world lifecycle: Entity access is required")
	}
	return &WorldLifecycle{access: access}, nil
}

// WorldFromRegistry is the normal application entry point. Nest publishes the
// instance-scoped ManagerAccess as entity.runtime, so lifecycle code never
// depends on package globals or reaches into a Service implementation.
func WorldFromRegistry(registry *app.Registry) (*WorldLifecycle, error) {
	if registry == nil {
		return nil, fmt.Errorf("world lifecycle: app registry is required")
	}
	access, ok := app.Lookup[*entity.ManagerAccess](registry, mods.ModEntityRuntime)
	if !ok || access == nil {
		return nil, fmt.Errorf("world lifecycle: Entity runtime is unavailable")
	}
	return NewWorldLifecycle(access)
}

func (lifecycle *WorldLifecycle) Get(ctx context.Context, uniqueID int64) (*world.World, error) {
	fullID, err := entity.BuildEntityID(uniqueID, world.EntityKindWorld)
	if err != nil {
		return nil, err
	}
	value, err := lifecycle.access.Get(ctx, fullID, entity.MustEntityCategoryOfKind(world.EntityKindWorld))
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, ErrWorldNotFound
	}
	typed, ok := value.(*world.World)
	if !ok {
		return nil, fmt.Errorf("world lifecycle: Entity %d has type %T", fullID, value)
	}
	return typed, nil
}

func (lifecycle *WorldLifecycle) Create(ctx context.Context, uniqueID int64) (*world.World, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	value, err := lifecycle.access.Create(&entity.EntityCreateParam{
		IsCreate: true,
		UniqueID: uniqueID,
		Kind:     world.EntityKindWorld,
		Category: entity.MustEntityCategoryOfKind(world.EntityKindWorld),
	})
	if err != nil {
		return nil, err
	}
	typed, ok := value.(*world.World)
	if !ok {
		return nil, fmt.Errorf("world lifecycle: created Entity has type %T", value)
	}
	return typed, nil
}

func (lifecycle *WorldLifecycle) GetOrCreate(ctx context.Context, uniqueID int64) (*world.World, bool, error) {
	value, err := lifecycle.Get(ctx, uniqueID)
	if err == nil {
		return value, false, nil
	}
	if !errors.Is(err, engine.ErrEntityAggregateNotFound) && !errors.Is(err, ErrWorldNotFound) {
		return nil, false, err
	}
	value, err = lifecycle.Create(ctx, uniqueID)
	if errors.Is(err, entity.ErrEntityExists) {
		value, err = lifecycle.Get(ctx, uniqueID)
		return value, false, err
	}
	return value, err == nil, err
}

func (lifecycle *WorldLifecycle) Destroy(ctx context.Context, value *world.World, reason entity.EntityDestroyReason, deletePersisted bool) error {
	if value == nil {
		return ErrWorldNotFound
	}
	return lifecycle.access.Destroy(ctx, value, reason, deletePersisted)
}
