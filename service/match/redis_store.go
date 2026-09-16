package match

import (
	"fmt"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// NewRedisStore builds a queue Store over Redis.
//
// No storage logic: the whole queue is one versioned entry, which is what
// makes a group commit a single compare-and-set, and kit's
// versionstore.RedisStore already implements that contract.
//
// One key per queue is deliberate and is the cost of that design: throughput
// on a single queue is bounded by contention on one key. That is why
// servicerpc.KeyAffinityPicker exists — route a queue's traffic to one
// replica and the contention is in-process rather than cross-replica.
// Round-robin routing over a shared key is the design cause of the contention
// the implementation this replaces suffered, not a tuning problem.
// It returns a ready Store rather than the state store, because the state's
// value type is unexported: handing back a versionstore.Store over an
// unnameable type would be a value a caller can hold and cannot declare.
func NewRedisStore(client versionstore.RedisClient, prefix string, cfg Config) (Store, error) {
	if strings.TrimSpace(prefix) == "" {
		return nil, fmt.Errorf("match: redis key prefix is required")
	}
	state, err := versionstore.NewRedisStore(client, versionstore.RedisConfig[string, queueState]{
		Prefix: prefix + ":queue:",
		KeyOf:  func(key string) string { return key },
		Codec:  versionstore.JSONCodec[queueState]{},
	})
	if err != nil {
		return nil, fmt.Errorf("match: queue state: %w", err)
	}
	return NewStore(state, cfg)
}
