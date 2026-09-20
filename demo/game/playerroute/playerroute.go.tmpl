// Package playerroute answers one question: which game process owns this
// player right now.
//
// It exists because two game processes sharing one database are not two
// independent games — they are two writers of the same documents. A Player is
// loaded into the process its connection landed on, and that process's entity
// runtime is the only thing serialising writes to it. A second process that
// loads the same Player has its own lock, its own version counter and its own
// idea of the document; the first write from each of them that lands is fine
// and the second is a `fatal projection version conflict`, which takes the
// process down. That is not a theoretical risk: it is what the demo did the
// first time it was started twice (see GAME_DEMO_TEMPLATE §9.10.3).
//
// So ownership has to be a FACT somewhere both processes can read, and this is
// it: a Redis key per player, holding the sid that owns them, with a lease so
// that a process which dies does not own players forever.
//
// What this is NOT: a lock. Nothing here prevents a process from loading a
// player it does not own — it only lets code ASK, and the callers are
// responsible for acting on the answer. A hard guarantee is what remote-managed
// entities give (a versioned distributed lock, see game/entities/guild), and
// the reason players do not use one is cost: a player's every action would
// take a Redis round trip before its own lock.
package playerroute

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

// Lease is how long an ownership claim survives without a refresh.
//
// It is the window in which a crashed process still appears to own its
// players — long enough that a normal GC pause or a slow tick does not drop a
// live claim, short enough that a restart is not locked out of its own
// players for minutes. The refresh interval must be comfortably shorter.
const (
	Lease           = 30 * time.Second
	RefreshInterval = 10 * time.Second
)

// Route is who owns a player. It satisfies core's ownerroute.OwnerRoute.
//
// Token identifies the PROCESS, not the server: a process that restarts keeps
// its sid and gets a new token. Without it, "is this lease still mine" cannot
// distinguish the instance that took the lease from the one that replaced it,
// and a restarted process would happily refresh — or release — a lease its
// predecessor left behind and somebody else has since taken
// (RR-20260920-03).
type Route struct {
	SID   int32
	Token string
}

// OwnerRouteSid implements ownerroute.OwnerRoute.
func (r Route) OwnerRouteSid() int32 { return r.SID }

// held reports whether anybody owns the player.
func (r Route) held() bool { return r.SID != 0 }

// encode is the stored form: "<sid>:<token>".
func (r Route) encode() string { return strconv.FormatInt(int64(r.SID), 10) + ":" + r.Token }

// decodeRoute reads the stored form. A value without a token is refused
// rather than read as "sid, no token": that shape is a pre-token key left by
// an older build, and treating it as ownable would let two processes agree
// they both hold it. It expires within Lease on its own.
func decodeRoute(raw string) (Route, error) {
	sidText, token, found := strings.Cut(raw, ":")
	if !found || token == "" {
		return Route{}, fmt.Errorf("playerroute: %q is not a <sid>:<token> owner", raw)
	}
	sid, err := strconv.ParseInt(sidText, 10, 32)
	if err != nil || sid <= 0 {
		return Route{}, fmt.Errorf("playerroute: %q is not a <sid>:<token> owner", raw)
	}
	return Route{SID: int32(sid), Token: token}, nil
}

// keyspace is the part of Redis this package uses. Every operation that
// changes a lease is atomic in one round trip, and that is the whole content
// of this interface:
//
//	SetNX             take a lease nobody holds — first caller wins
//	CompareAndExpire  extend a lease, only while it is still exactly ours
//	CompareAndDelete  give up a lease, only while it is still exactly ours
//	Get               read who holds it (a hint; never the basis of a write)
//
// The compare-and-X pair replaced "GET, check the sid, then EXPIRE/DEL".
// Between those two commands a lease can expire and be taken by another
// process, so the second command lands on THEIR key: the old owner would
// extend somebody else's lease, or delete it (RR-20260920-03). "Only if it is
// ours" has to be one operation to be true at all.
//
// Declaring the narrow interface here rather than taking fredis.IRedis whole
// is what lets these rules be tested without a Redis.
type keyspace interface {
	SetNX(ctx context.Context, key string, value any, expiration time.Duration) (bool, error)
	Get(ctx context.Context, key string) ([]byte, error)
	CompareAndExpire(ctx context.Context, key, expect string, expiration time.Duration) (bool, error)
	CompareAndDelete(ctx context.Context, key, expect string) (bool, error)
}

// redisKeyspace is keyspace over a real Redis. The two compare-and-X
// operations are core's Lua scripts; nothing here re-implements them.
type redisKeyspace struct{ client fredis.IRedis }

func (k redisKeyspace) SetNX(ctx context.Context, key string, value any, expiration time.Duration) (bool, error) {
	return k.client.SetNX(ctx, key, value, expiration)
}

func (k redisKeyspace) Get(ctx context.Context, key string) ([]byte, error) {
	return k.client.Get(ctx, key)
}

// CompareAndExpire is CompareAndSet writing the value back unchanged: the
// script compares and then PSETEXs, which is exactly "extend if still mine".
func (k redisKeyspace) CompareAndExpire(ctx context.Context, key, expect string, expiration time.Duration) (bool, error) {
	result, err := fredis.CompareAndSet(ctx, k.client, fredis.CompareAndSetCommand{
		Key: key, Expected: []byte(expect), Next: []byte(expect), TTL: expiration,
	})
	if err != nil {
		return false, err
	}
	return result.Applied, nil
}

func (k redisKeyspace) CompareAndDelete(ctx context.Context, key, expect string) (bool, error) {
	result, err := fredis.CompareAndDelete(ctx, k.client, key, []byte(expect))
	if err != nil {
		return false, err
	}
	return result.Applied, nil
}

// Store is the shared ownership table.
type Store struct {
	redis  keyspace
	prefix string
	sid    int32
	// token is THIS process's incarnation. It is minted per Store, so a
	// restart on the same sid is a different owner as far as every compare
	// here is concerned.
	token string
}

// NewStore builds it. The prefix is the game's own Redis namespace, because
// this is the game's fact and not a framework service's.
func NewStore(client fredis.IRedis, prefix string, sid int32) (*Store, error) {
	if client == nil {
		return nil, fmt.Errorf("playerroute: redis client is required")
	}
	if prefix == "" {
		return nil, fmt.Errorf("playerroute: key prefix is required")
	}
	if sid <= 0 {
		return nil, fmt.Errorf("playerroute: sid must be positive, got %d", sid)
	}
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	return newStore(redisKeyspace{client: client}, prefix, sid, token)
}

// newToken mints this process's incarnation. Randomness rather than pid or
// start time: two containers can share both.
func newToken() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("playerroute: mint owner token: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// newStore is NewStore over the narrow seam; the tests build a Store from a
// fake keyspace through it.
func newStore(client keyspace, prefix string, sid int32, token string) (*Store, error) {
	if client == nil {
		return nil, fmt.Errorf("playerroute: redis client is required")
	}
	if prefix == "" {
		return nil, fmt.Errorf("playerroute: key prefix is required")
	}
	if sid <= 0 {
		return nil, fmt.Errorf("playerroute: sid must be positive, got %d", sid)
	}
	if token == "" {
		return nil, fmt.Errorf("playerroute: owner token is required")
	}
	return &Store{redis: client, prefix: prefix, sid: sid, token: token}, nil
}

// self is this process as an owner.
func (s *Store) self() Route { return Route{SID: s.sid, Token: s.token} }

func (s *Store) key(playerID int64) string {
	return s.prefix + ":owner:" + strconv.FormatInt(playerID, 10)
}

// Claim takes ownership of a player for this process and reports who owns
// them afterwards.
//
// It is insert-only with a lease: the first process to claim wins, and a
// process that already owns the player refreshes instead. That is what makes
// "nobody owns this player yet" safe — two processes racing to pick up the
// same piece of work resolve to one owner rather than both proceeding.
func (s *Store) Claim(ctx context.Context, playerID int64) (Route, error) {
	if playerID <= 0 {
		return Route{}, fmt.Errorf("playerroute: player id must be positive")
	}
	key := s.key(playerID)
	ours := s.self()
	taken, err := s.redis.SetNX(ctx, key, ours.encode(), Lease)
	if err != nil {
		return Route{}, fmt.Errorf("playerroute: claim %d: %w", playerID, err)
	}
	if taken {
		return ours, nil
	}
	current, err := s.Owner(ctx, playerID)
	if err != nil {
		return Route{}, err
	}
	if current == ours {
		// Ours already: extend the lease rather than leaving it to expire
		// under a player who is very much still here. Compared on the whole
		// route, token included — a previous incarnation of this same sid is
		// somebody else, and extending its lease would be the same mistake
		// as extending a stranger's.
		if _, err := s.redis.CompareAndExpire(ctx, key, ours.encode(), Lease); err != nil {
			return Route{}, fmt.Errorf("playerroute: refresh %d: %w", playerID, err)
		}
	}
	return current, nil
}

// Owner reports who owns a player, if anyone.
func (s *Store) Owner(ctx context.Context, playerID int64) (Route, error) {
	raw, err := s.redis.Get(ctx, s.key(playerID))
	if err != nil {
		if errors.Is(err, fredis.ErrNil) {
			// Nobody owns them. Not an error: it is the normal state of every
			// player who is not logged in.
			return Route{}, nil
		}
		return Route{}, fmt.Errorf("playerroute: owner of %d: %w", playerID, err)
	}
	route, convErr := decodeRoute(string(raw))
	if convErr != nil {
		return Route{}, fmt.Errorf("playerroute: owner of %d: %w", playerID, convErr)
	}
	return route, nil
}

// GetRoute implements core's ownerroute.Resolver.
//
// A player nobody owns is reported as NOT FOUND rather than as "owned by me".
// The difference matters: the router refuses, and the caller decides whether
// to claim the player first or to leave the work for whoever does own them.
// Answering "me" here would make every process the owner of every idle
// player, which is the corruption this package exists to prevent.
func (s *Store) GetRoute(ctx context.Context, playerID int64) (Route, bool, error) {
	route, err := s.Owner(ctx, playerID)
	if err != nil {
		return Route{}, false, err
	}
	return route, route.held(), nil
}

// Owns reports whether this process owns the player. It is the question the
// background consumers ask before touching one.
func (s *Store) Owns(ctx context.Context, playerID int64) (bool, error) {
	route, err := s.Owner(ctx, playerID)
	if err != nil {
		return false, err
	}
	return route == s.self(), nil
}

// Refresh extends this process's claims. Only claims that are still OURS are
// extended — Expire on a key another process now owns would hand them a lease
// they never asked for.
func (s *Store) Refresh(ctx context.Context, playerIDs []int64) []RefreshResult {
	ours := s.self().encode()
	out := make([]RefreshResult, 0, len(playerIDs))
	for _, playerID := range playerIDs {
		held, err := s.redis.CompareAndExpire(ctx, s.key(playerID), ours, Lease)
		out = append(out, RefreshResult{PlayerID: playerID, Held: held && err == nil, Err: err})
	}
	return out
}

// RefreshResult is what one lease's renewal did. Held is true only when Redis
// confirmed the lease is still ours; an error means UNKNOWN, not held —
// a caller that treats a failed renewal as success is exactly the double
// writer this table exists to prevent (RR-20260920-04).
type RefreshResult struct {
	PlayerID int64
	Held     bool
	Err      error
}

// Release gives up ownership, and only if it is ours: a stale release from a
// process the player has already left must not free the new owner's claim.
func (s *Store) Release(ctx context.Context, playerID int64) error {
	if _, err := s.redis.CompareAndDelete(ctx, s.key(playerID), s.self().encode()); err != nil {
		return fmt.Errorf("playerroute: release %d: %w", playerID, err)
	}
	return nil
}

// SID is this process's id.
func (s *Store) SID() int32 { return s.sid }
