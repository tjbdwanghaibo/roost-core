package directory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/versionstore"

	"github.com/tjbdwanghaibo/roost-kit/service/servicemetrics"
)

// Config configures a versioned directory.
type Config struct {
	// Normalize maps a raw key to the form uniqueness is decided on.
	// Required — see Normalizer.
	Normalize Normalizer
	// DefaultTTL is used when Reserve is called with a non-positive ttl.
	// Required to be positive: a reservation with no expiry is the burned-key
	// failure this primitive exists to prevent.
	DefaultTTL time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// NewToken mints reservation tokens; nil means a 128-bit random token.
	// Tokens must be unguessable: a caller that can guess another owner's
	// token can commit or cancel that owner's reservation.
	NewToken func() (string, error)
	// Metrics receives reports. A nil reporter means no reporting and never
	// fails an operation.
	//
	// Cancel and Release deliberately return nil for claims that are no longer
	// the caller's — deleting there is the race this primitive exists to
	// prevent. That makes them silent no-ops reported as success, which is
	// only acceptable because they are counted here.
	Metrics servicemetrics.Reporter
}

// store is a Directory over versioned state. Every mutation goes through
// versionstore.Update, so "read, decide, write" cannot be expressed here.
type store struct {
	state  versionstore.Store[string, Entry]
	cfg    Config
	report servicemetrics.Sink
}

// New returns a Directory backed by state.
func New(state versionstore.Store[string, Entry], cfg Config) (Directory, error) {
	if state == nil {
		return nil, fmt.Errorf("directory: state store is nil")
	}
	if cfg.Normalize == nil {
		return nil, fmt.Errorf("directory: normalizer is required")
	}
	if cfg.DefaultTTL <= 0 {
		return nil, fmt.Errorf("directory: default ttl must be positive; a reservation that never expires burns the key when the caller dies")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.NewToken == nil {
		cfg.NewToken = randomToken
	}
	return &store{state: state, cfg: cfg, report: servicemetrics.Wrap(cfg.Metrics)}, nil
}

// NormalizeLower is the common normalizer: trim surrounding space and fold
// case, so "Alice", " alice " and "ALICE" are one key.
func NormalizeLower(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", ErrKeyEmpty
	}
	return strings.ToLower(trimmed), nil
}

func randomToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("directory: mint token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func (s *store) Reserve(ctx context.Context, raw string, owner Owner, ttl time.Duration) (Claim, error) {
	key, err := s.cfg.Normalize(raw)
	if err != nil {
		return Claim{}, err
	}
	if owner == "" {
		return Claim{}, ErrOwnerEmpty
	}
	if ttl <= 0 {
		ttl = s.cfg.DefaultTTL
	}
	now := s.cfg.Now()
	expiresAt := now.Add(ttl)

	token, err := s.cfg.NewToken()
	if err != nil {
		return Claim{}, err
	}

	// The callback only decides; it reports nothing. versionstore may run it
	// more than once — every lost compare-and-set re-reads and re-applies it
	// — so a counter incremented inside it counts attempts, not outcomes.
	// The decision is carried out and reported exactly once, after Update.
	var claim Claim
	var replayed bool
	_, _, err = s.state.Update(ctx, key, func(current Entry, found bool) (Entry, bool, error) {
		replayed = false
		// A lapsed reservation is treated as absent. This is what frees a key
		// whose reserver died before committing.
		if found && !current.Expired(now) {
			if current.Owner != owner {
				return current, false, fmt.Errorf("%w: %q held by %q", ErrKeyTaken, key, current.Owner)
			}
			replayed = true
			if current.State == StateCommitted {
				// Already ours and permanent: hand back a claim describing
				// that, so a retried Reserve is a no-op rather than an error.
				claim = Claim{Key: key, Owner: owner, Token: current.Token}
				return current, false, nil
			}
			// Ours and still reserved: return the existing claim instead of
			// minting a second one, so a retried request does not consume two
			// reservations.
			claim = Claim{Key: key, Owner: owner, Token: current.Token,
				ExpiresAt: time.Unix(current.ExpiresAtUnix, 0)}
			return current, false, nil
		}
		next := Entry{
			Key: key, Raw: strings.TrimSpace(raw), Owner: owner, State: StateReserved,
			Token: token, ExpiresAtUnix: expiresAt.Unix(), ReservedAtUnix: now.Unix(),
		}
		claim = Claim{Key: key, Owner: owner, Token: token, ExpiresAt: expiresAt}
		return next, true, nil
	})
	switch {
	case errors.Is(err, ErrKeyTaken):
		s.report.Refused("reserve", "taken")
		return Claim{}, err
	case err != nil:
		return Claim{}, err
	case replayed:
		s.report.Replayed("reserve")
	default:
		s.report.Accepted("reserve")
	}
	return claim, nil
}

func (s *store) Commit(ctx context.Context, claim Claim) (Entry, error) {
	if claim.Key == "" {
		return Entry{}, ErrKeyEmpty
	}
	if claim.Token == "" {
		return Entry{}, ErrClaimNotFound
	}
	now := s.cfg.Now()
	// As in Reserve: the callback decides, the report happens once after it.
	var committed Entry
	var replayed bool
	var refusal string
	_, _, err := s.state.Update(ctx, claim.Key, func(current Entry, found bool) (Entry, bool, error) {
		replayed, refusal = false, ""
		if !found {
			return current, false, fmt.Errorf("%w: %q", ErrClaimNotFound, claim.Key)
		}
		if current.Token != claim.Token {
			// Someone else holds the key now. Reporting this rather than
			// overwriting is the whole point of carrying a token.
			refusal = "stale"
			return current, false, fmt.Errorf("%w: %q is held by %q", ErrClaimStale, claim.Key, current.Owner)
		}
		if current.State == StateCommitted {
			// Idempotent: a retried Commit on our own committed entry
			// succeeds, so a client retry after a lost response does not fail.
			committed = current
			replayed = true
			return current, false, nil
		}
		if current.Expired(now) {
			refusal = "lapsed"
			return current, false, fmt.Errorf("%w: %q reservation lapsed", ErrClaimNotFound, claim.Key)
		}
		next := current
		next.State = StateCommitted
		next.ExpiresAtUnix = 0
		next.CommittedAtUnix = now.Unix()
		committed = next
		return next, true, nil
	})
	switch {
	case err != nil:
		if refusal != "" {
			s.report.Refused("commit", refusal)
		}
		return Entry{}, err
	case replayed:
		s.report.Replayed("commit")
	default:
		s.report.Accepted("commit")
	}
	return committed, nil
}

func (s *store) Cancel(ctx context.Context, claim Claim) error {
	if claim.Key == "" {
		return ErrKeyEmpty
	}
	current, found, err := s.state.Get(ctx, claim.Key)
	if err != nil {
		return err
	}
	if !found {
		// Idempotent: a caller retrying its own rollback must not fail.
		s.report.Replayed("cancel")
		return nil
	}
	if current.Value.Token != claim.Token {
		// Not ours any more — the reservation lapsed and someone else took
		// the key. Deleting here is exactly the bug this replaces: a release
		// racing a re-reservation removing the new owner's claim.
		s.report.Dropped("cancel.not_ours", 1)
		return nil
	}
	if current.Value.State == StateCommitted {
		s.report.Refused("cancel", "committed")
		return fmt.Errorf("%w: %q is committed; use Release", ErrClaimStale, claim.Key)
	}
	// Version-checked delete: if the entry changed since the read, the delete
	// is refused rather than removing whatever is there now.
	if err := s.state.Delete(ctx, claim.Key, current); err != nil {
		if errors.Is(err, versionstore.ErrVersionMismatch) {
			s.report.Dropped("cancel.raced", 1)
			return nil
		}
		return err
	}
	s.report.Accepted("cancel")
	return nil
}

func (s *store) Lookup(ctx context.Context, raw string) (Entry, bool, error) {
	key, err := s.cfg.Normalize(raw)
	if err != nil {
		return Entry{}, false, err
	}
	current, found, err := s.state.Get(ctx, key)
	if err != nil || !found {
		return Entry{}, false, err
	}
	if current.Value.Expired(s.cfg.Now()) {
		return Entry{}, false, nil
	}
	return current.Value, true, nil
}

func (s *store) Release(ctx context.Context, raw string, owner Owner) error {
	key, err := s.cfg.Normalize(raw)
	if err != nil {
		return err
	}
	if owner == "" {
		return ErrOwnerEmpty
	}
	current, found, err := s.state.Get(ctx, key)
	if err != nil {
		return err
	}
	if !found {
		s.report.Replayed("release")
		return nil
	}
	if current.Value.Owner != owner {
		s.report.Refused("release", "not_owner")
		return fmt.Errorf("%w: %q belongs to %q", ErrOwnerMismatch, key, current.Value.Owner)
	}
	if err := s.state.Delete(ctx, key, current); err != nil {
		if errors.Is(err, versionstore.ErrVersionMismatch) {
			s.report.Conflict("release")
			return fmt.Errorf("%w: %q changed during release", ErrOwnerMismatch, key)
		}
		return err
	}
	s.report.Accepted("release")
	return nil
}

var _ Directory = (*store)(nil)
