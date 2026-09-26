package lifecycle

import (
	"context"
	"errors"
	"fmt"

	player "example.com/planet/game/entities/player"
	"github.com/tjbdwanghaibo/roost-core/app"
	engine "github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
)

var ErrPlayerNotFound = errors.New("player lifecycle: Entity not found")

// PlayerLifecycle owns the explicit create/load/destroy boundary for Player.
// Request handlers should use generated Nest senders for ordinary reads and
// writes; this type is for authentication/role creation and administration.
type PlayerLifecycle struct {
	access *entity.ManagerAccess
}

func NewPlayerLifecycle(access *entity.ManagerAccess) (*PlayerLifecycle, error) {
	if access == nil || access.Manager() == nil {
		return nil, fmt.Errorf("player lifecycle: Entity access is required")
	}
	return &PlayerLifecycle{access: access}, nil
}

// PlayerFromRegistry is the normal application entry point. Nest publishes the
// instance-scoped ManagerAccess as entity.runtime, so lifecycle code never
// depends on package globals or reaches into a Service implementation.
func PlayerFromRegistry(registry *app.Registry) (*PlayerLifecycle, error) {
	if registry == nil {
		return nil, fmt.Errorf("player lifecycle: app registry is required")
	}
	access, ok := app.Lookup[*entity.ManagerAccess](registry, mods.ModEntityRuntime)
	if !ok || access == nil {
		return nil, fmt.Errorf("player lifecycle: Entity runtime is unavailable")
	}
	return NewPlayerLifecycle(access)
}

func (lifecycle *PlayerLifecycle) Get(ctx context.Context, uniqueID int64) (*player.Player, error) {
	fullID, err := entity.BuildEntityID(uniqueID, player.EntityKindPlayer)
	if err != nil {
		return nil, err
	}
	value, err := lifecycle.access.Get(ctx, fullID, entity.MustEntityCategoryOfKind(player.EntityKindPlayer))
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, ErrPlayerNotFound
	}
	typed, ok := value.(*player.Player)
	if !ok {
		return nil, fmt.Errorf("player lifecycle: Entity %d has type %T", fullID, value)
	}
	return typed, nil
}

func (lifecycle *PlayerLifecycle) Create(ctx context.Context, uniqueID int64) (*player.Player, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	value, err := lifecycle.access.Create(&entity.EntityCreateParam{
		IsCreate: true,
		UniqueID: uniqueID,
		Kind:     player.EntityKindPlayer,
		Category: entity.MustEntityCategoryOfKind(player.EntityKindPlayer),
	})
	if err != nil {
		return nil, err
	}
	typed, ok := value.(*player.Player)
	if !ok {
		return nil, fmt.Errorf("player lifecycle: created Entity has type %T", value)
	}
	return typed, nil
}

func (lifecycle *PlayerLifecycle) GetOrCreate(ctx context.Context, uniqueID int64) (*player.Player, bool, error) {
	value, err := lifecycle.Get(ctx, uniqueID)
	if err == nil {
		return value, false, nil
	}
	if !errors.Is(err, engine.ErrEntityAggregateNotFound) && !errors.Is(err, ErrPlayerNotFound) {
		return nil, false, err
	}
	value, err = lifecycle.Create(ctx, uniqueID)
	if errors.Is(err, entity.ErrEntityExists) {
		value, err = lifecycle.Get(ctx, uniqueID)
		return value, false, err
	}
	return value, err == nil, err
}

func (lifecycle *PlayerLifecycle) Destroy(ctx context.Context, value *player.Player, reason entity.EntityDestroyReason, deletePersisted bool) error {
	if value == nil {
		return ErrPlayerNotFound
	}
	return lifecycle.access.Destroy(ctx, value, reason, deletePersisted)
}
