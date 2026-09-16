package session

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// RedisStores are the three stores this package needs, over Redis.
//
// There is no storage logic here, and that is the point: runs, claims and
// ledger entries are all versioned state, and kit's versionstore.RedisStore
// already implements that contract — compare-and-set through
// roost-core/redis.CompareAndSet, jittered exponential backoff, insert-only
// Create, version-checked Delete. Reimplementing any of it per service is how
// four hand-written stores in the implementation this replaces ended up with
// four different answers to "does Update compare versions".
//
// What this constructor contributes is the part that must NOT be left to each
// caller: key rendering and namespacing. Three stores share one prefix and
// must not collide with each other, and a caller assembling them by hand gets
// that right until the day it does not.
type RedisStores struct {
	Runs     RunStore
	Claims   ClaimStore
	Requests RequestLedger
}

// RedisConfig configures the three stores.
type RedisConfig struct {
	// Prefix namespaces every key. Required and non-empty, so two services
	// sharing a Redis cannot silently share state.
	Prefix string
	// RequestTTL expires ledger entries. It must be positive and must exceed
	// the longest client retry horizon: past it a retry is indistinguishable
	// from a new enter, and the caller gets a second run.
	//
	// Runs and claims deliberately have NO ttl. A ttl on versioned state
	// loses the version with the value, so a key that expires and is written
	// again restarts at version 1 — and for a claim that is exactly the
	// exclusion silently lapsing. Their lifecycle is Sweep's job, which is
	// visible and counted.
	RequestTTL time.Duration
	// MaxAttempts and RetryBackoff are passed through to the underlying
	// stores; zero selects their defaults.
	MaxAttempts  int
	RetryBackoff time.Duration
}

// NewRedisStores builds the three stores. The client is the narrow capability
// versionstore needs, so a caller passes its redis client unchanged.
func NewRedisStores(client versionstore.RedisClient, cfg RedisConfig) (RedisStores, error) {
	if strings.TrimSpace(cfg.Prefix) == "" {
		return RedisStores{}, fmt.Errorf("session: redis key prefix is required")
	}
	if cfg.RequestTTL <= 0 {
		return RedisStores{}, fmt.Errorf("session: request ttl must be positive; without one the " +
			"enter ledger grows without bound, and with a short one a retry becomes a second run")
	}

	runs, err := versionstore.NewRedisStore(client, versionstore.RedisConfig[string, Run]{
		Prefix:       cfg.Prefix + ":run:",
		KeyOf:        func(id string) string { return id },
		Codec:        versionstore.JSONCodec[Run]{},
		MaxAttempts:  cfg.MaxAttempts,
		RetryBackoff: cfg.RetryBackoff,
	})
	if err != nil {
		return RedisStores{}, fmt.Errorf("session: run store: %w", err)
	}
	claims, err := versionstore.NewRedisStore(client, versionstore.RedisConfig[int64, Claim]{
		Prefix:       cfg.Prefix + ":claim:",
		KeyOf:        func(ownerID int64) string { return strconv.FormatInt(ownerID, 10) },
		Codec:        versionstore.JSONCodec[Claim]{},
		MaxAttempts:  cfg.MaxAttempts,
		RetryBackoff: cfg.RetryBackoff,
	})
	if err != nil {
		return RedisStores{}, fmt.Errorf("session: claim store: %w", err)
	}
	requests, err := versionstore.NewRedisStore(client, versionstore.RedisConfig[string, LedgerEntry]{
		Prefix:       cfg.Prefix + ":req:",
		KeyOf:        func(requestID string) string { return requestID },
		Codec:        versionstore.JSONCodec[LedgerEntry]{},
		TTL:          cfg.RequestTTL,
		MaxAttempts:  cfg.MaxAttempts,
		RetryBackoff: cfg.RetryBackoff,
	})
	if err != nil {
		return RedisStores{}, fmt.Errorf("session: request ledger: %w", err)
	}
	return RedisStores{Runs: runs, Claims: claims, Requests: requests}, nil
}
