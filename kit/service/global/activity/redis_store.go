package activity

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// RedisStores are the six stores this service needs, over Redis.
//
// There is no storage logic here, and that is the point. The implementation
// this replaces had a Redis store whose Update used compare-and-set and a
// DAO-backed store whose Update read and then wrote unconditionally, both
// satisfying one interface — so which one was configured decided whether the
// documented CAS invariant held. Here there is one implementation, in kit,
// whose contract has no unconditional write.
type RedisStores struct {
	Activities   versionstore.Store[Key, Activity]
	Participants versionstore.Store[ParticipantKey, Participant]
	Ledger       versionstore.Store[RequestKey, ProgressReservation]
	Audits       versionstore.Store[Key, NotifyAuditLog]
	Dispatches   versionstore.Store[DispatchKey, Dispatch]
	Windows      versionstore.Store[string, Window]
}

// NewRedisStores builds them.
//
// The ledger is the only store with a key TTL, and it must have one: its
// entries are progress reservations, which are unbounded in number and whose
// job is finished once no client will retry. reservationTTL has to exceed the
// longest client retry horizon — past it a replay is indistinguishable from a
// new request, and the progress is applied twice.
//
// Nothing else gets a TTL, because a TTL on versioned state takes the version
// with the value: a key that expired and was written again restarts at version
// 1, and every fence a caller passes becomes a comparison against a version
// that just reset.
func NewRedisStores(client versionstore.RedisClient, prefix string, reservationTTL time.Duration) (RedisStores, error) {
	if strings.TrimSpace(prefix) == "" {
		return RedisStores{}, fmt.Errorf("activity: redis key prefix is required")
	}
	if reservationTTL <= 0 {
		return RedisStores{}, fmt.Errorf("activity: reservation ttl must be positive; it must also " +
			"exceed the longest client retry horizon, or a replay becomes a second apply")
	}
	var (
		stores RedisStores
		err    error
	)
	if stores.Activities, err = versionstore.NewRedisStore(client, versionstore.RedisConfig[Key, Activity]{
		Prefix: prefix + ":act:", KeyOf: Key.String, Codec: versionstore.JSONCodec[Activity]{},
	}); err != nil {
		return RedisStores{}, fmt.Errorf("activity: activity store: %w", err)
	}
	if stores.Participants, err = versionstore.NewRedisStore(client, versionstore.RedisConfig[ParticipantKey, Participant]{
		Prefix: prefix + ":part:", KeyOf: ParticipantKey.String, Codec: versionstore.JSONCodec[Participant]{},
	}); err != nil {
		return RedisStores{}, fmt.Errorf("activity: participant store: %w", err)
	}
	if stores.Ledger, err = versionstore.NewRedisStore(client, versionstore.RedisConfig[RequestKey, ProgressReservation]{
		Prefix: prefix + ":req:", KeyOf: RequestKey.String, Codec: versionstore.JSONCodec[ProgressReservation]{},
		TTL: reservationTTL,
	}); err != nil {
		return RedisStores{}, fmt.Errorf("activity: progress ledger: %w", err)
	}
	if stores.Audits, err = versionstore.NewRedisStore(client, versionstore.RedisConfig[Key, NotifyAuditLog]{
		Prefix: prefix + ":audit:", KeyOf: Key.String, Codec: versionstore.JSONCodec[NotifyAuditLog]{},
	}); err != nil {
		return RedisStores{}, fmt.Errorf("activity: audit store: %w", err)
	}
	// The dispatch store carries a per-game index of what is owed, maintained
	// in the same write as the dispatch itself.
	//
	// It is what lets a game server ask "what do I still owe an ack for"
	// instead of guessing activity ids from its own clock — the thing that
	// breaks the moment a server is down longer than one window
	// (RR-20260919-10). Per game rather than one global set because the
	// question is per game, and a global set would have to be filtered, which
	// is the shape that pages badly.
	dispatchStore, err := versionstore.NewRedisStore(client, versionstore.RedisConfig[DispatchKey, Dispatch]{
		Prefix: prefix + ":disp:", KeyOf: DispatchKey.String, Codec: versionstore.JSONCodec[Dispatch]{},
		Index: &versionstore.RedisIndex[Dispatch]{
			KeyOf: func(dispatch Dispatch) string {
				return OwedDispatchKey(prefix, dispatch.Key.GroupID, dispatch.GameSID)
			},
			Entry: owedDispatchEntry,
		},
	})
	if err != nil {
		return RedisStores{}, fmt.Errorf("activity: dispatch store: %w", err)
	}
	stores.Dispatches = &RedisDispatches{store: dispatchStore, prefix: prefix}
	if stores.Windows, err = versionstore.NewRedisStore(client, versionstore.RedisConfig[string, Window]{
		Prefix: prefix + ":win:", KeyOf: func(groupID string) string { return groupID },
		Codec: versionstore.JSONCodec[Window]{},
	}); err != nil {
		return RedisStores{}, fmt.Errorf("activity: window store: %w", err)
	}
	return stores, nil
}

// OwedDispatchKey is the sorted set of dispatches one game server still owes
// an ack for. It lives under the same prefix as the dispatches themselves,
// because it is an index OF them.
//
// On a Redis Cluster the index and the dispatch keys must hash to one slot for
// the write to be atomic, which needs a hash tag in the prefix; Mod.Init
// refuses a cluster deployment without one rather than letting the atomicity
// be quietly untrue.
func OwedDispatchKey(prefix, groupID string, gameSID int32) string {
	return prefix + ":owed:" + groupID + ":" + strconv.FormatInt(int64(gameSID), 10)
}

// owedDispatchEntry places a dispatch in its game's owed set, scored by when
// it is next worth taking. A dispatch that reached a terminal state — acked,
// or exhausted after the game took it too many times without acking — leaves
// the set in the write that made it terminal.
func owedDispatchEntry(dispatch Dispatch) (float64, bool) {
	if dispatch.State != DispatchPending {
		return 0, false
	}
	due := dispatch.NextAttemptAtUnix
	if due <= 0 {
		due = dispatch.CreatedAtUnix
	}
	return float64(due), true
}

// RedisDispatches is the dispatch store plus the per-game owed index.
type RedisDispatches struct {
	store  *versionstore.RedisStore[DispatchKey, Dispatch]
	prefix string
}

func (d *RedisDispatches) Get(ctx context.Context, key DispatchKey) (versionstore.Versioned[Dispatch], bool, error) {
	return d.store.Get(ctx, key)
}

func (d *RedisDispatches) Update(ctx context.Context, key DispatchKey, mutate versionstore.Mutate[Dispatch]) (versionstore.Versioned[Dispatch], bool, error) {
	return d.store.Update(ctx, key, mutate)
}

func (d *RedisDispatches) Create(ctx context.Context, key DispatchKey, value Dispatch) (versionstore.Versioned[Dispatch], bool, error) {
	return d.store.Create(ctx, key, value)
}

func (d *RedisDispatches) Delete(ctx context.Context, key DispatchKey, expect versionstore.Versioned[Dispatch]) error {
	return d.store.Delete(ctx, key, expect)
}

// OwedDispatches implements OwedDispatchIndex: the keys this game server still
// owes an ack for, due now, oldest first.
func (d *RedisDispatches) OwedDispatches(ctx context.Context, groupID string, gameSID int32, nowUnix int64, limit int) ([]DispatchKey, error) {
	if groupID == "" || gameSID <= 0 || limit <= 0 {
		return nil, nil
	}
	members, err := d.store.IndexDueIn(ctx, OwedDispatchKey(d.prefix, groupID, gameSID), float64(nowUnix), limit)
	if err != nil {
		return nil, err
	}
	out := make([]DispatchKey, 0, len(members))
	for _, member := range members {
		key, err := ParseDispatchKey(member)
		if err != nil {
			// A member the current build cannot read is left in place — its
			// dispatch may still be real — and reported rather than silently
			// skipped forever.
			return nil, fmt.Errorf("activity: owed index member %q: %w", member, err)
		}
		out = append(out, key)
	}
	return out, nil
}

var (
	_ versionstore.Store[DispatchKey, Dispatch] = (*RedisDispatches)(nil)
	_ OwedDispatchIndex                         = (*RedisDispatches)(nil)
)
