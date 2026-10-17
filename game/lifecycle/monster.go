package lifecycle

import (
	"context"
	"errors"
	"fmt"

	monster "example.com/planet/game/entities/monster"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	engine "github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
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
		return nil, deletedMonsterError(err)
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
	// Create does not read the store: uniqueID must be a fresh id from the id
	// generator, never the id of a deleted Entity (its tombstone makes the
	// first write a projection conflict that fences the process). Use
	// GetOrCreate when the id may already have a record.
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

// GetOrCreate returns the existing Entity or creates it when persistence has
// no record. An Entity that was deleted (only tombstones left) is NOT
// created again: its id cannot be used again, and GetOrCreate returns an
// error carrying errcode.ErrEntityDeleted (client code entity_deleted).
// entity.ErrEntityRemoved means the previous instance with this ID
// is still finishing its removal (a revoked in-transaction create or a
// Destroy); it is transient, so the lookup is retried a bounded number of
// times before the error is returned. Inside a Nest handler the framework
// reports that window as nest.ErrCreatedEntityLockConflict instead and
// decides itself whether the whole message is rolled back and re-admitted.
func (lifecycle *MonsterLifecycle) GetOrCreate(ctx context.Context, uniqueID int64) (*monster.Monster, bool, error) {
	for attempt := 1; ; attempt++ {
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
		if errors.Is(err, entity.ErrEntityRemoved) && attempt < 3 {
			continue
		}
		return value, err == nil, err
	}
}

func (lifecycle *MonsterLifecycle) Destroy(ctx context.Context, value *monster.Monster, reason entity.EntityDestroyReason, deletePersisted bool) error {
	if value == nil {
		return ErrMonsterNotFound
	}
	return lifecycle.access.Destroy(ctx, value, reason, deletePersisted)
}

// deletedMonsterError gives a deleted Entity its stable client code
// (errcode.ErrEntityDeleted); the cause stays on the chain for errors.Is.
func deletedMonsterError(err error) error {
	if errors.Is(err, entity.ErrAuthorityEntityDeleted) {
		var coded *errcode.IntError
		if !errors.As(err, &coded) {
			return errcode.Wrap(errcode.ErrEntityDeleted, err)
		}
	}
	return err
}
