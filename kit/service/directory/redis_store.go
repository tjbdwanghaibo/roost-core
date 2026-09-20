package directory

import (
	"fmt"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// NewRedisState builds the versioned state a Directory runs on.
//
// There is no storage logic here. A directory entry is versioned state, and
// kit's versionstore.RedisStore already implements that contract; what this
// adds is the key namespacing, which must not be each caller's to invent.
//
// The keys are already normalized by the time they reach the store — that is
// what Config.Normalize is for — so KeyOf is the identity. Normalizing twice,
// or normalizing here instead of there, would give two answers to "which key
// is this" depending on the code path.
func NewRedisState(client versionstore.RedisClient, prefix string, ttl time.Duration) (versionstore.Store[string, Entry], error) {
	if strings.TrimSpace(prefix) == "" {
		return nil, fmt.Errorf("directory: redis key prefix is required")
	}
	// No TTL on the entry itself, even though reservations expire: a
	// reservation's expiry is a FIELD this package reads and acts on, not a
	// key expiry. A key TTL would take the version with the value, so a
	// committed entry that lapsed and was rewritten would restart at version
	// 1 — and a version that restarts is a compare that stops comparing.
	// Expiry here is Reserve's business, and a committed entry has none.
	if ttl != 0 {
		return nil, fmt.Errorf("directory: a key ttl is not supported; reservation expiry is a " +
			"field this package reads, and a key ttl would reset the version instead")
	}
	return versionstore.NewRedisStore(client, versionstore.RedisConfig[string, Entry]{
		Prefix: prefix + ":name:",
		KeyOf:  func(key string) string { return key },
		Codec:  versionstore.JSONCodec[Entry]{},
	})
}
