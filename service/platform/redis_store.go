package platform

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// NewRedisOrders builds the order store over Redis, with the pending index
// built in.
//
// There is no storage logic here: an order is versioned state, and
// versionstore.RedisStore already implements that contract — including the
// insert-only Create that turns a second callback into a replay instead of a
// second grant, and, since RR-20260919-04, a sorted-set index maintained in
// the same script as the value.
//
// The index is the part worth explaining. U-0234 left it to the deployment,
// and a deployment can only write it AFTER the order is stored — which leaves
// a window: the order lands, the process dies before the index write, and the
// order is in no index. The background loop's only input is that index, so the
// payment is invisible to recovery forever. Nothing outside the store can
// close that window, because the two writes are only atomic if they are one
// write. So the store does it: an order enters the index when it is created,
// moves with its backoff, and leaves the index in the write that makes it
// terminal.
//
// It takes NO ttl, and refuses to offer one. An order is the record of a
// payment: the answer to "did this player receive what they paid for". A key
// ttl on versioned state also takes the version with the value, so an order
// that expired and was written again restarts at version 1 — meaning a
// replayed callback for an aged-out order would be treated as a first arrival
// and deliver the goods again. Retention for paid orders belongs to an
// archival job that can be audited, not to a key expiry nobody sees.
func NewRedisOrders(client versionstore.RedisClient, prefix string) (*RedisOrders, error) {
	if strings.TrimSpace(prefix) == "" {
		return nil, fmt.Errorf("platform: redis key prefix is required")
	}
	store, err := versionstore.NewRedisStore(client, versionstore.RedisConfig[string, Order]{
		Prefix: prefix + ":order:",
		KeyOf:  func(orderID string) string { return orderID },
		Codec:  versionstore.JSONCodec[Order]{},
		Index: &versionstore.RedisIndex[Order]{
			Key:   PendingIndexKey(prefix),
			Entry: pendingIndexEntry,
		},
	})
	if err != nil {
		return nil, err
	}
	return &RedisOrders{store: store, now: time.Now}, nil
}

// PendingIndexKey is where the pending index lives. It is beside the order
// keys, under the same prefix, because it is an index OF those keys: a prefix
// of its own would let one be dropped without the other.
//
// On a Redis Cluster the two must hash to the same slot for the write to be
// atomic, which means the prefix needs a hash tag — `{...}` around the part
// the orders and the index share. Mod.Init refuses a cluster deployment whose
// prefix has none rather than letting the atomicity be quietly untrue.
func PendingIndexKey(prefix string) string { return prefix + ":pending" }

// pendingIndexEntry decides where an order sits in the index, and whether it
// is in it at all.
//
// The score is when the order is next worth an attempt: the service advances
// NextAttemptAtUnix inside the same compare-and-set that claims an attempt, so
// a delivery that keeps failing moves its own score forward and cannot starve
// the orders queued behind it. Before the first attempt there is no such
// moment yet, so payment time stands in.
func pendingIndexEntry(order Order) (float64, bool) {
	if order.Terminal() {
		return 0, false
	}
	due := order.NextAttemptAtUnix
	if due <= 0 {
		due = order.PaidAtUnix
	}
	if due <= 0 {
		due = order.CreatedAtUnix
	}
	return float64(due), true
}

// RedisOrders is the order store and its pending index. It satisfies
// OrderStore and PendingOrders, so a deployment that uses it needs no index of
// its own.
type RedisOrders struct {
	store *versionstore.RedisStore[string, Order]
	now   func() time.Time
}

func (o *RedisOrders) Get(ctx context.Context, key string) (versionstore.Versioned[Order], bool, error) {
	return o.store.Get(ctx, key)
}

func (o *RedisOrders) Update(ctx context.Context, key string, mutate versionstore.Mutate[Order]) (versionstore.Versioned[Order], bool, error) {
	return o.store.Update(ctx, key, mutate)
}

func (o *RedisOrders) Create(ctx context.Context, key string, value Order) (versionstore.Versioned[Order], bool, error) {
	return o.store.Create(ctx, key, value)
}

func (o *RedisOrders) Delete(ctx context.Context, key string, expect versionstore.Versioned[Order]) error {
	return o.store.Delete(ctx, key, expect)
}

// PendingOrders returns the orders that are due now, oldest first.
//
// It reads the index and nothing else: an order that reached a terminal state
// left the index in the write that made it terminal, so there is no per-order
// read here to fail, and therefore no way for one unreadable order to hold up
// the page (the shape RR-20260919-05 found in a deployment-side index).
func (o *RedisOrders) PendingOrders(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	return o.store.IndexDue(ctx, float64(o.now().Unix()), limit)
}

var (
	_ OrderStore    = (*RedisOrders)(nil)
	_ PendingOrders = (*RedisOrders)(nil)
)
