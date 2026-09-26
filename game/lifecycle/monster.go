package lifecycle

import (
	"context"
	"errors"
	"fmt"

	monster "example.com/planet/game/entities/monster"
	"github.com/tjbdwanghaibo/roost-core/app"
	engine "github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
)

var ErrMonsterNotFound = errors.New("monster lifecycle: Entity not found")

// MonsterLifecycle owns the explicit create/load/destroy boundary for Monster.
// Request handlers should use generated Nest senders for ordinary reads and
// writes; this type is for authentication/role creation and administration.
type MonsterLifecycle struct {
	access *entity.ManagerAccess
}

func NewMonsterLifecycle(access *entity.ManagerAccess) (*MonsterLifecycle, error) {
	if access == nil || access.Manager() == nil {
		return nil, fmt.Errorf("monster lifecycle: Entity access is required")
	}
	return &MonsterLifecycle{access: access}, nil
}

// MonsterFromRegistry is the normal application entry point. Nest publishes the
// instance-scoped ManagerAccess as entity.runtime, so lifecycle code never
// depends on package globals or reaches into a Service implementation.
func MonsterFromRegistry(registry *app.Registry) (*MonsterLifecycle, error) {
	if registry == nil {
		return nil, fmt.Errorf("monster lifecycle: app registry is required")
	}
	access, ok := app.Lookup[*entity.ManagerAccess](registry, mods.ModEntityRuntime)
	if !ok || access == nil {
		return nil, fmt.Errorf("monster lifecycle: Entity runtime is unavailable")
	}
	return NewMonsterLifecycle(access)
}

func (lifecycle *MonsterLifecycle) Get(ctx context.Context, uniqueID int64) (*monster.Monster, error) {
	fullID, err := entity.BuildEntityID(uniqueID, monster.EntityKindMonster)
	if err != nil {
		return nil, err
	}
	value, err := lifecycle.access.Get(ctx, fullID, entity.MustEntityCategoryOfKind(monster.EntityKindMonster))
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, ErrMonsterNotFound
	}
	typed, ok := value.(*monster.Monster)
	if !ok {
		return nil, fmt.Errorf("monster lifecycle: Entity %d has type %T", fullID, value)
	}
	return typed, nil
}

func (lifecycle *MonsterLifecycle) Create(ctx context.Context, uniqueID int64) (*monster.Monster, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	value, err := lifecycle.access.Create(&entity.EntityCreateParam{
		IsCreate: true,
		UniqueID: uniqueID,
		Kind:     monster.EntityKindMonster,
		Category: entity.MustEntityCategoryOfKind(monster.EntityKindMonster),
	})
	if err != nil {
		return nil, err
	}
	typed, ok := value.(*monster.Monster)
	if !ok {
		return nil, fmt.Errorf("monster lifecycle: created Entity has type %T", value)
	}
	return typed, nil
}

func (lifecycle *MonsterLifecycle) GetOrCreate(ctx context.Context, uniqueID int64) (*monster.Monster, bool, error) {
	value, err := lifecycle.Get(ctx, uniqueID)
	if err == nil {
		return value, false, nil
	}
	if !errors.Is(err, engine.ErrEntityAggregateNotFound) && !errors.Is(err, ErrMonsterNotFound) {
		return nil, false, err
	}
	value, err = lifecycle.Create(ctx, uniqueID)
	if errors.Is(err, entity.ErrEntityExists) {
		value, err = lifecycle.Get(ctx, uniqueID)
		return value, false, err
	}
	return value, err == nil, err
}

func (lifecycle *MonsterLifecycle) Destroy(ctx context.Context, value *monster.Monster, reason entity.EntityDestroyReason, deletePersisted bool) error {
	if value == nil {
		return ErrMonsterNotFound
	}
	return lifecycle.access.Destroy(ctx, value, reason, deletePersisted)
}
