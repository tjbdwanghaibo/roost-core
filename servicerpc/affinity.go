package servicerpc

import (
	"context"
	"fmt"
	"hash/fnv"

	fetcd "github.com/tjbdwanghaibo/roost-core/etcd"
	"github.com/tjbdwanghaibo/roost-core/misc"
)

// KeyAffinityPicker routes every call carrying the same key to the same
// instance, as long as the discovered set is unchanged.
//
// RoundRobinPicker is the right default for a stateless read, and the wrong
// one for a call that mutates shared state under a per-instance lock: it sends
// consecutive operations on one logical key to different replicas, which turns
// an in-process mutex into no mutual exclusion at all. That is how a
// matchmaking queue ended up with two replicas selecting the same head of the
// same queue and each committing its own match.
//
// Affinity is not a correctness mechanism on its own — instances come and go,
// and the mapping moves with them — so it does not remove the need for
// compare-and-set on the shared state. What it removes is the steady-state
// contention that makes the failure routine.
type KeyAffinityPicker struct {
	// Key selects the affinity key for this call. Required.
	Key func(ctx context.Context) (string, bool)
	// Fallback picks when Key reports no key; nil means RoundRobinPicker.
	Fallback DiscoveryPicker
}

func (p KeyAffinityPicker) Pick(ctx context.Context, serviceType string, infos []*fetcd.ServiceInfo, sequence uint64) (int32, error) {
	if len(infos) == 0 {
		return 0, fmt.Errorf("servicerpc: no %s service discovered", serviceType)
	}
	key := ""
	ok := false
	if p.Key != nil {
		key, ok = p.Key(ctx)
	}
	if !ok || key == "" {
		fallback := p.Fallback
		if fallback == nil {
			fallback = RoundRobinPicker{}
		}
		return fallback.Pick(ctx, serviceType, infos, sequence)
	}
	// Order by sid so the mapping does not depend on the order discovery
	// happened to return; otherwise two callers with the same key can still
	// disagree.
	sorted := make([]*fetcd.ServiceInfo, len(infos))
	copy(sorted, infos)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j-1].Sid > sorted[j].Sid; j-- {
			sorted[j-1], sorted[j] = sorted[j], sorted[j-1]
		}
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(key))
	index := misc.Hash64(hash.Sum64()) % uint64(len(sorted))
	return sorted[index].Sid, nil
}

// WithKeyAffinity routes calls by an affinity key taken from the call context.
func WithKeyAffinity(key func(ctx context.Context) (string, bool)) Option {
	return func(c *BusClient) {
		c.picker = KeyAffinityPicker{Key: key}
	}
}

// affinityContextKey carries an affinity key through a call.
type affinityContextKey struct{}

// WithAffinityKey returns a context whose calls route by key.
func WithAffinityKey(ctx context.Context, key string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, affinityContextKey{}, key)
}

// AffinityKeyFromContext reads the key installed by WithAffinityKey. It is the
// default Key function for WithKeyAffinity callers that route per call rather
// than per client.
func AffinityKeyFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	key, ok := ctx.Value(affinityContextKey{}).(string)
	return key, ok && key != ""
}

var _ DiscoveryPicker = KeyAffinityPicker{}
