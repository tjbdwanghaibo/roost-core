package global

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// RedisStores are the stores this package needs, over Redis: one for routing
// and one for leases.
//
// There is no storage logic here, and for this package that is the entire
// point of the design. The implementation this replaces had FOUR hand-written
// stores per concern — a Redis variant whose Update used compare-and-set and a
// DAO variant whose Update read and then wrote unconditionally — satisfying
// one interface, so the type system could not tell them apart and whichever
// was configured decided whether the documented invariant held. Here there is
// one implementation, in kit, whose contract has no unconditional write.
type RedisStores struct {
	Routes versionstore.Store[int32, RouteBinding]
	Leases versionstore.Store[int32, GameLease]
}

// NewRedisStores builds them.
//
// Neither gets a TTL, and the lease in particular must not: a TTL on versioned
// state takes the version with the value, so a lease key that expired and was
// written again restarts at version 1, and the incarnation fence that a
// renewal must pass becomes a comparison against a version that just reset.
// A lease's expiry is a FIELD this package reads.
func NewRedisStores(client versionstore.RedisClient, prefix string) (RedisStores, error) {
	if strings.TrimSpace(prefix) == "" {
		return RedisStores{}, fmt.Errorf("global: redis key prefix is required")
	}
	int32Key := func(id int32) string { return strconv.FormatInt(int64(id), 10) }

	var (
		stores RedisStores
		err    error
	)
	if stores.Routes, err = versionstore.NewRedisStore(client, versionstore.RedisConfig[int32, RouteBinding]{
		Prefix: prefix + ":route:", KeyOf: int32Key, Codec: versionstore.JSONCodec[RouteBinding]{},
	}); err != nil {
		return RedisStores{}, fmt.Errorf("global: route store: %w", err)
	}
	if stores.Leases, err = versionstore.NewRedisStore(client, versionstore.RedisConfig[int32, GameLease]{
		Prefix: prefix + ":lease:", KeyOf: int32Key, Codec: versionstore.JSONCodec[GameLease]{},
	}); err != nil {
		return RedisStores{}, fmt.Errorf("global: lease store: %w", err)
	}
	return stores, nil
}
