package lifecycle

import (
	"context"
	"fmt"

	world "example.com/planet/game/entities/world"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/entity"
)

// WorldUniqueID is the unique id of a server's one World: its sid. There is
// exactly one World per game server, and a server is one process, so this is
// what keeps two processes sharing a database from writing one document as
// two entities.
func WorldUniqueID(registry *app.Registry) (int64, error) {
	if registry == nil {
		return 0, fmt.Errorf("world: registry is required to know which server's World this is")
	}
	sid := registry.Config().GetInt64("sid")
	if sid <= 0 {
		return 0, fmt.Errorf("world: sid is %d; a World belongs to a server and needs its id", sid)
	}
	return sid, nil
}

// WorldID is WorldUniqueID as the full entity id the Nest senders address.
func WorldID(registry *app.Registry) (int64, error) {
	unique, err := WorldUniqueID(registry)
	if err != nil {
		return 0, err
	}
	id, err := entity.BuildEntityID(unique, world.EntityKindWorld)
	if err != nil {
		return 0, fmt.Errorf("world: build entity id: %w", err)
	}
	return id, nil
}

// EnsureWorld returns this server's World, creating it on the first start
// and loading it on every later one. The game Service calls it from Init, so
// the World exists before any request is served.
func EnsureWorld(ctx context.Context, registry *app.Registry) (*world.World, error) {
	lifecycle, err := WorldFromRegistry(registry)
	if err != nil {
		return nil, err
	}
	unique, err := WorldUniqueID(registry)
	if err != nil {
		return nil, err
	}
	value, _, err := lifecycle.GetOrCreate(ctx, unique)
	if err != nil {
		return nil, fmt.Errorf("world: ensure singleton: %w", err)
	}
	return value, nil
}
