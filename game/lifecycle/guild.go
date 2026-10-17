package lifecycle

import (
	"context"
	"errors"
	"fmt"

	guild "example.com/planet/game/entities/guild"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	engine "github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

var ErrGuildNotFound = errors.New("guild lifecycle: Entity not found")

// GuildLifecycle owns the explicit create/load/destroy boundary for Guild.
// Request handlers should use generated Nest senders for ordinary reads and
// writes; this type is for authentication/role creation and administration.
type GuildLifecycle struct {
	access *entity.ManagerAccess
}

func NewGuildLifecycle(access *entity.ManagerAccess) (*GuildLifecycle, error) {
	if access == nil || access.Manager() == nil {
		return nil, fmt.Errorf("guild lifecycle: Entity access is required")
	}
	return &GuildLifecycle{access: access}, nil
}

// GuildFromRegistry is the normal application entry point. Nest publishes the
// instance-scoped ManagerAccess as entity.runtime, so lifecycle code never
// depends on package globals or reaches into a Service implementation.
func GuildFromRegistry(registry *app.Registry) (*GuildLifecycle, error) {
	if registry == nil {
		return nil, fmt.Errorf("guild lifecycle: app registry is required")
	}
	access, ok := app.Lookup[*entity.ManagerAccess](registry, mods.ModEntityRuntime)
	if !ok || access == nil {
		return nil, fmt.Errorf("guild lifecycle: Entity runtime is unavailable")
	}
	return NewGuildLifecycle(access)
}

func (lifecycle *GuildLifecycle) Get(ctx context.Context, uniqueID int64) (*guild.Guild, error) {
	fullID, err := entity.BuildEntityID(uniqueID, guild.EntityKindGuild)
	if err != nil {
		return nil, err
	}
	value, err := lifecycle.access.Get(ctx, fullID, entity.MustEntityCategoryOfKind(guild.EntityKindGuild))
	if err != nil {
		return nil, deletedGuildError(err)
	}
	if value == nil {
		return nil, ErrGuildNotFound
	}
	typed, ok := value.(*guild.Guild)
	if !ok {
		return nil, fmt.Errorf("guild lifecycle: Entity %d has type %T", fullID, value)
	}
	return typed, nil
}

func (lifecycle *GuildLifecycle) Create(ctx context.Context, uniqueID int64) (*guild.Guild, error) {
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
		Kind:     guild.EntityKindGuild,
		Category: entity.MustEntityCategoryOfKind(guild.EntityKindGuild),
	})
	if err != nil {
		return nil, err
	}
	typed, ok := value.(*guild.Guild)
	if !ok {
		return nil, fmt.Errorf("guild lifecycle: created Entity has type %T", value)
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
func (lifecycle *GuildLifecycle) GetOrCreate(ctx context.Context, uniqueID int64) (*guild.Guild, bool, error) {
	for attempt := 1; ; attempt++ {
		value, err := lifecycle.Get(ctx, uniqueID)
		if err == nil {
			return value, false, nil
		}
		if !errors.Is(err, engine.ErrEntityAggregateNotFound) && !errors.Is(err, ErrGuildNotFound) {
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

func (lifecycle *GuildLifecycle) Destroy(ctx context.Context, value *guild.Guild, reason entity.EntityDestroyReason, deletePersisted bool) error {
	if value == nil {
		return ErrGuildNotFound
	}
	return lifecycle.access.Destroy(ctx, value, reason, deletePersisted)
}

// deletedGuildError gives a deleted Entity its stable client code
// (errcode.ErrEntityDeleted); the cause stays on the chain for errors.Is.
func deletedGuildError(err error) error {
	if errors.Is(err, entity.ErrAuthorityEntityDeleted) {
		var coded *errcode.IntError
		if !errors.As(err, &coded) {
			return errcode.Wrap(errcode.ErrEntityDeleted, err)
		}
	}
	return err
}
