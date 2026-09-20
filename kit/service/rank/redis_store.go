package rank

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"

	"github.com/tjbdwanghaibo/roost-core/versionstore"
	"github.com/tjbdwanghaibo/roost-kit/service/servicemetrics"
)

// RedisClient is the slice of redis.IRedis a RedisStore uses.
type RedisClient interface {
	fredis.ScriptRunner
	Del(ctx context.Context, keys ...string) (int64, error)
	ZCard(ctx context.Context, key string) (int64, error)
	// ZRevRangeWithScores is the descending range. Scores are all zero here —
	// ordering lives entirely in the member — so only Member is read.
	ZRevRangeWithScores(ctx context.Context, key string, start, stop int64) ([]fredis.Z, error)
	ZRevRank(ctx context.Context, key string, member string) (int64, error)
}

// RedisConfig configures a RedisStore.
type RedisConfig struct {
	// Prefix namespaces every key. Required to be non-empty so two services
	// cannot share a keyspace by accident.
	Prefix string
	// Now supplies the default tiebreak for a submit that leaves Tie zero.
	// nil means time.Now.
	Now func() time.Time
	// RetryBackoff is the base delay between lost compare-and-swaps; zero
	// selects versionstore.DefaultRetryBackoff, negative disables sleeping.
	RetryBackoff time.Duration
	// Sleep is the delay function; nil means time.Sleep. Test seam.
	Sleep func(time.Duration)
	// Metrics receives reports. A nil reporter means no reporting and never
	// fails an operation. Without it the paths below are invisible, which is
	// how the implementation this replaces hid a submit that answered
	// failure after a successful write.
	Metrics servicemetrics.Reporter
}

// RedisStore keeps one sorted set per board, whose members carry everything a
// ranked page needs. There is no second structure to fall out of sync with —
// the defect that produced ordering entries with no payload, which then made
// pages short and silently truncated archives.
type RedisStore struct {
	client RedisClient
	cfg    RedisConfig
	report servicemetrics.Sink
}

func NewRedisStore(client RedisClient, cfg RedisConfig) (*RedisStore, error) {
	if client == nil {
		return nil, fmt.Errorf("rank: redis client is nil")
	}
	if strings.TrimSpace(cfg.Prefix) == "" {
		return nil, fmt.Errorf("rank: redis prefix is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &RedisStore{client: client, cfg: cfg, report: servicemetrics.Wrap(cfg.Metrics)}, nil
}

func (s *RedisStore) boardKey(board Board) string {
	return s.cfg.Prefix + ":z:" + board.String()
}

// ownerKey maps owner -> the exact member currently stored for it, so a submit
// can find and remove the previous member without scanning. It is written in
// the same script as the sorted set, so the two cannot disagree.
func (s *RedisStore) ownerKey(board Board) string {
	return s.cfg.Prefix + ":o:" + board.String()
}

// swapScript exchanges an owner's stored member for a new one, atomically and
// only if the stored member is still what the caller read.
//
// The encoding lives in Go only: reproducing the fixed-width signed mapping in
// Lua would mean two implementations of the ordering, and a divergence between
// them would corrupt the board silently. So Go computes the next member and
// the script performs the compare-and-swap — one round trip, no orphan
// possible, and no second copy of the encoding.
//
// KEYS[1] board sorted set, KEYS[2] owner->member hash.
// ARGV: owner id, expected member (empty for create), next member,
// request id (may be empty), request ring.
const swapScript = `
local ownerID  = ARGV[1]
local expected = ARGV[2]
local nextMember = ARGV[3]
local reqID    = ARGV[4]
local ring     = ARGV[5]

local stored = redis.call("HGET", KEYS[2], ownerID)
if expected == "" then
  if stored then return {0, stored} end
else
  if not stored or stored ~= expected then return {0, stored or ""} end
  redis.call("ZREM", KEYS[1], stored)
end
redis.call("ZADD", KEYS[1], 0, nextMember)
redis.call("HSET", KEYS[2], ownerID, nextMember)
if reqID ~= "" then
  redis.call("HSET", KEYS[2], ownerID .. ":applied", ring)
end
return {1, nextMember}
`

const removeScript = `
local ownerID = ARGV[1]
local stored = redis.call("HGET", KEYS[2], ownerID)
if not stored then return 0 end
redis.call("ZREM", KEYS[1], stored)
redis.call("HDEL", KEYS[2], ownerID)
redis.call("HDEL", KEYS[2], ownerID .. ":applied")
return 1
`

const maxSubmitAttempts = 8

func (s *RedisStore) Submit(ctx context.Context, board Board, score Score, mode UpdateMode, requestID string) (Entry, error) {
	if err := validateSubmit(board, score, mode, requestID); err != nil {
		return Entry{}, err
	}
	if score.Tie == 0 {
		// An unset tiebreak becomes the submit time, so equal scores are
		// ordered by who reached them first rather than by owner id.
		score.Tie = s.cfg.Now().UnixMilli()
	}
	zkey, okey := s.boardKey(board), s.ownerKey(board)

	for attempt := 0; attempt < maxSubmitAttempts; attempt++ {
		stored, ring, err := s.readOwner(ctx, okey, score.OwnerID)
		if err != nil {
			return Entry{}, err
		}
		if requestID != "" && ringContains(ring, requestID) {
			// A replay is the signal that the transport is redelivering, and
			// it is the difference between a correct accumulating submit and
			// a board that drifts on every redelivery.
			s.report.Replayed("submit")
			// Already applied. Return what is stored rather than applying the
			// change again — the difference between a replay-safe
			// accumulating submit and a leaderboard that drifts on every
			// redelivery.
			return s.entryFor(ctx, zkey, stored)
		}
		next, changed, err := nextScore(stored, score, mode)
		if err != nil {
			return Entry{}, err
		}
		if !changed {
			// Nothing to write, but the request still has to be recorded or a
			// replay would be indistinguishable from a first attempt.
			if requestID != "" {
				if _, _, err := s.swap(ctx, zkey, okey, score.OwnerID, stored, stored, requestID, appendRing(ring, requestID)); err != nil {
					return Entry{}, err
				}
			}
			return s.entryFor(ctx, zkey, stored)
		}
		applied, current, err := s.swap(ctx, zkey, okey, score.OwnerID, stored, encodeEntry(next), requestID, appendRing(ring, requestID))
		if err != nil {
			return Entry{}, err
		}
		if applied {
			s.report.Accepted("submit")
			return s.entryFor(ctx, zkey, encodeEntry(next))
		}
		_ = current
		// Lost the swap. Back off with jitter before re-reading: retrying
		// immediately makes contention on one owner look like failure, because
		// every loser retries in lockstep and they exhaust the budget
		// together. The policy lives in versionstore so there is one
		// implementation of it, not one per compare-and-set loop.
		versionstore.RetryBackoff(attempt, s.cfg.RetryBackoff, s.cfg.Sleep)
	}
	// Contention on one owner is the expected failure mode here, so it is
	// reported rather than left as an opaque internal error the caller cannot
	// distinguish from a Redis outage.
	s.report.Conflict("submit")
	return Entry{}, fmt.Errorf("%w: submit lost %d compare-and-swaps for owner %d", ErrConflict, maxSubmitAttempts, score.OwnerID)
}

// ErrConflictSentinel is the previous name of ErrConflict, kept so existing
// errors.Is call sites keep matching. It used to be a plain errors.New, which
// is why a submit that lost every compare-and-swap reached clients as
// CodeInternal / "server error" — the one failure mode its own comment promised
// was distinguishable from a store outage.
//
// Deprecated: use ErrConflict.
var ErrConflictSentinel = ErrConflict

func (s *RedisStore) readOwner(ctx context.Context, okey string, ownerID int64) (member string, ring string, err error) {
	ret, err := s.client.Eval(ctx, `
local m = redis.call("HGET", KEYS[1], ARGV[1])
local a = redis.call("HGET", KEYS[1], ARGV[1] .. ":applied")
return {m or "", a or ""}
`, []string{okey}, strconv.FormatInt(ownerID, 10))
	if err != nil {
		return "", "", err
	}
	items, ok := ret.([]any)
	if !ok || len(items) != 2 {
		return "", "", fmt.Errorf("rank: unexpected owner read result %T", ret)
	}
	return luaString(items[0]), luaString(items[1]), nil
}

func (s *RedisStore) swap(ctx context.Context, zkey, okey string, ownerID int64, expected, next, requestID, ring string) (bool, string, error) {
	ret, err := s.client.Eval(ctx, swapScript, []string{zkey, okey},
		strconv.FormatInt(ownerID, 10), expected, next, requestID, ring)
	if err != nil {
		return false, "", err
	}
	items, ok := ret.([]any)
	if !ok || len(items) != 2 {
		return false, "", fmt.Errorf("rank: unexpected swap result %T", ret)
	}
	appliedFlag, err := luaInt(items[0])
	if err != nil {
		return false, "", err
	}
	return appliedFlag == 1, luaString(items[1]), nil
}

func (s *RedisStore) Remove(ctx context.Context, board Board, ownerID int64) error {
	if err := board.Validate(); err != nil {
		return err
	}
	if ownerID == 0 {
		return ErrOwnerInvalid
	}
	_, err := s.client.Eval(ctx, removeScript, []string{s.boardKey(board), s.ownerKey(board)},
		strconv.FormatInt(ownerID, 10))
	return err
}

func (s *RedisStore) Page(ctx context.Context, board Board, offset, limit int) (Page, error) {
	if err := board.Validate(); err != nil {
		return Page{}, err
	}
	if err := validateRange(offset, limit); err != nil {
		return Page{}, err
	}
	zkey := s.boardKey(board)
	ranged, err := s.client.ZRevRangeWithScores(ctx, zkey, int64(offset), int64(offset+limit-1))
	if err != nil {
		return Page{}, err
	}
	members := make([]string, 0, len(ranged))
	for _, item := range ranged {
		members = append(members, item.Member)
	}
	total, err := s.client.ZCard(ctx, zkey)
	if err != nil {
		return Page{}, err
	}
	s.report.Depth("board."+board.ID, total)
	entries := make([]Entry, 0, len(members))
	for index, member := range members {
		score, err := decodeEntry(member)
		if err != nil {
			// A member we cannot parse is reported, not skipped. Skipping it
			// while still consuming a rank slot is what made pages short and
			// truncated archives.
			return Page{}, fmt.Errorf("rank: board %s position %d: %w", board, offset+index, err)
		}
		entries = append(entries, Entry{Rank: int64(offset+index) + 1, Score: score})
	}
	return Page{Entries: entries, Total: total}, nil
}

func (s *RedisStore) Rank(ctx context.Context, board Board, ownerID int64) (Entry, bool, error) {
	if err := board.Validate(); err != nil {
		return Entry{}, false, err
	}
	if ownerID == 0 {
		return Entry{}, false, ErrOwnerInvalid
	}
	member, _, err := s.readOwner(ctx, s.ownerKey(board), ownerID)
	if err != nil {
		return Entry{}, false, err
	}
	if member == "" {
		return Entry{}, false, nil
	}
	entry, err := s.entryFor(ctx, s.boardKey(board), member)
	if err != nil {
		return Entry{}, false, err
	}
	return entry, true, nil
}

func (s *RedisStore) Around(ctx context.Context, board Board, ownerID int64, radius int) (Page, error) {
	if radius <= 0 || radius > MaxPageSize/2 {
		return Page{}, fmt.Errorf("%w: radius must be in [1,%d], got %d", ErrRangeInvalid, MaxPageSize/2, radius)
	}
	entry, found, err := s.Rank(ctx, board, ownerID)
	if err != nil {
		return Page{}, err
	}
	if !found {
		return Page{}, fmt.Errorf("%w: owner %d is not on board %s", ErrNotFound, ownerID, board)
	}
	offset := int(entry.Rank) - 1 - radius
	if offset < 0 {
		offset = 0
	}
	return s.Page(ctx, board, offset, radius*2+1)
}

func (s *RedisStore) Reset(ctx context.Context, board Board) error {
	if err := board.Validate(); err != nil {
		return err
	}
	_, err := s.client.Del(ctx, s.boardKey(board), s.ownerKey(board))
	return err
}

func (s *RedisStore) Size(ctx context.Context, board Board) (int64, error) {
	if err := board.Validate(); err != nil {
		return 0, err
	}
	return s.client.ZCard(ctx, s.boardKey(board))
}

// entryFor resolves a member's rank. The score comes from the member itself,
// so the only thing read here is the position — a page never mixes a score
// from one read with a rank from another.
func (s *RedisStore) entryFor(ctx context.Context, zkey, member string) (Entry, error) {
	if member == "" {
		return Entry{}, fmt.Errorf("%w: no stored member", ErrNotFound)
	}
	score, err := decodeEntry(member)
	if err != nil {
		return Entry{}, err
	}
	position, err := s.client.ZRevRank(ctx, zkey, member)
	if err != nil {
		if errors.Is(err, fredis.ErrNil) {
			// The member was removed between the two calls. Report the score
			// with an unknown rank rather than an error: the write did land,
			// and reporting a successful submit as a failure is what made a
			// caller retry and resurrect a deleted entry.
			return Entry{Rank: 0, Score: score}, nil
		}
		return Entry{}, err
	}
	return Entry{Rank: position + 1, Score: score}, nil
}

// nextScore computes what to store, and whether anything changed.
func nextScore(stored string, incoming Score, mode UpdateMode) (Score, bool, error) {
	if stored == "" {
		return incoming, true, nil
	}
	current, err := decodeEntry(stored)
	if err != nil {
		return Score{}, false, err
	}
	next := incoming
	switch mode {
	case UpdateSet:
	case UpdateMax:
		if current.Value > incoming.Value {
			return current, false, nil
		}
		if current.Value == incoming.Value {
			// Equal value: keep the earlier tiebreak so a resubmit does not
			// move an owner down the board.
			if current.Tie <= incoming.Tie {
				return current, false, nil
			}
		}
	case UpdateAdd:
		next.Value = current.Value + incoming.Value
	default:
		return Score{}, false, fmt.Errorf("%w: unknown mode %q", ErrScoreInvalid, mode)
	}
	if len(next.Brief) == 0 {
		next.Brief = current.Brief
	}
	return next, true, nil
}

func ringContains(ring, requestID string) bool {
	if ring == "" || requestID == "" {
		return false
	}
	for _, token := range strings.Split(ring, ",") {
		if token == requestID {
			return true
		}
	}
	return false
}

// appendRing adds requestID to a bounded ring of recent ids. Bounded inside
// the record rather than in a separate growing key, so idempotency cannot
// become an unbounded keyspace.
func appendRing(ring, requestID string) string {
	if requestID == "" {
		return ring
	}
	tokens := []string{}
	if ring != "" {
		tokens = strings.Split(ring, ",")
	}
	for _, token := range tokens {
		if token == requestID {
			return ring
		}
	}
	tokens = append(tokens, requestID)
	if len(tokens) > maxAppliedRequests {
		tokens = tokens[len(tokens)-maxAppliedRequests:]
	}
	return strings.Join(tokens, ",")
}

func luaString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []byte:
		return string(typed)
	case nil:
		return ""
	}
	return ""
}

func luaInt(value any) (int64, error) {
	switch typed := value.(type) {
	case int64:
		return typed, nil
	case int:
		return int64(typed), nil
	case string:
		return strconv.ParseInt(typed, 10, 64)
	}
	return 0, fmt.Errorf("rank: unexpected lua integer %T", value)
}

var _ Store = (*RedisStore)(nil)
