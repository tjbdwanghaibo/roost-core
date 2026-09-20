package account

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/security"
	"github.com/tjbdwanghaibo/roost-core/versionstore"

	"github.com/tjbdwanghaibo/roost-kit/service/servicemetrics"

	"github.com/tjbdwanghaibo/roost-kit/service/directory"
)

// Config wires a Service. Every field without a safe default is required, and
// the constructor refuses an incomplete one — the alternative is a service
// that starts and then behaves as if a missing piece were a policy choice.
type Config struct {
	// Accounts, Roles and Servers hold the durable state.
	Accounts versionstore.Store[string, Account]
	Roles    versionstore.Store[int64, Role]
	Servers  versionstore.Store[int32, GameServer]

	// Names reserves display names. Uniqueness is a two-phase claim so a
	// crash between reserving the name and writing the role releases the name
	// instead of burning it.
	Names directory.Directory
	// Slots records the one-role-per-account-per-server occupancy.
	//
	// Deliberately NOT the directory primitive. A directory reservation is
	// idempotent for the same owner — a retried Reserve returns the claim it
	// already holds, which is right for a name a caller may re-request — and
	// that is exactly wrong here: an account racing itself would get one
	// shared claim and every racer would proceed. The slot is a mutual
	// exclusion for the duration of one create, so it uses insert-only
	// versioned state, where a second creator loses regardless of who it is.
	//
	// The limit it replaces was a read-count-write over a deliberately
	// non-unique index, so two concurrent creates both saw zero and both
	// inserted.
	Slots versionstore.Store[string, Slot]

	// Verifier, Allocator and NameRules are required. See their interfaces
	// for why none of them has a default.
	Verifier  IdentityVerifier
	Allocator PlayerIDAllocator
	NameRules NameValidator

	// SessionSecret signs role session tokens. Required and non-empty.
	SessionSecret string
	// SessionTTL bounds a token's life; zero selects DefaultSessionTTL.
	SessionTTL time.Duration
	// RolesPerServer is how many roles one account may hold on one server;
	// zero selects one. It is configuration, not a compiled-in constant.
	RolesPerServer int
	// ClaimTTL is how long a name or slot claim is held while a role is being
	// created; zero selects DefaultClaimTTL.
	ClaimTTL time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Metrics receives reports. A nil reporter means no reporting and never
	// fails an operation.
	//
	// Two signals here are not conveniences. A refused authority check is how
	// an operator learns a client is asking for roles it does not own — the
	// implementation this replaces took the account id from the request, so
	// there was nothing to refuse and nothing to count. And a failed rollback
	// leaves an unreleased slot that blocks one account on one server until
	// someone intervenes; that error is returned, but a returned error nobody
	// aggregates is how it stayed invisible for a release.
	Metrics servicemetrics.Reporter
}

const (
	// DefaultSessionTTL is how long a role session token lasts.
	DefaultSessionTTL = 30 * time.Minute
	// DefaultClaimTTL is how long a name or slot reservation is held during
	// role creation. Short, because it only has to cover one create.
	DefaultClaimTTL = 30 * time.Second
)

// Service is the account directory.
type Service struct {
	cfg    Config
	report servicemetrics.Sink
}

// New validates the configuration and returns a Service.
func New(cfg Config) (*Service, error) {
	missing := []string{}
	if cfg.Accounts == nil {
		missing = append(missing, "Accounts")
	}
	if cfg.Roles == nil {
		missing = append(missing, "Roles")
	}
	if cfg.Servers == nil {
		missing = append(missing, "Servers")
	}
	if cfg.Names == nil {
		missing = append(missing, "Names")
	}
	if cfg.Slots == nil {
		missing = append(missing, "Slots")
	}
	if cfg.Verifier == nil {
		// Stated at length because this is the one whose absence was a
		// vulnerability rather than an inconvenience.
		missing = append(missing, "Verifier (identity verification has no default: without it login authenticates nobody)")
	}
	if cfg.Allocator == nil {
		missing = append(missing, "Allocator (player ids have no default: a process-local counter collides across replicas and restarts)")
	}
	if cfg.NameRules == nil {
		missing = append(missing, "NameRules")
	}
	if strings.TrimSpace(cfg.SessionSecret) == "" {
		missing = append(missing, "SessionSecret")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("account: incomplete configuration: %s", strings.Join(missing, ", "))
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = DefaultSessionTTL
	}
	if cfg.ClaimTTL <= 0 {
		cfg.ClaimTTL = DefaultClaimTTL
	}
	if cfg.RolesPerServer <= 0 {
		cfg.RolesPerServer = 1
	}
	if cfg.RolesPerServer != 1 {
		// Honest refusal beats silent downgrade. The exclusive-claim design
		// enforces exactly one owner per key; supporting N would need N keys
		// and a free-slot search, which is a different design. Accepting the
		// value and enforcing one is how a configuration option becomes a
		// lie.
		return nil, fmt.Errorf("account: RolesPerServer above one is not implemented; got %d", cfg.RolesPerServer)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{cfg: cfg, report: servicemetrics.Wrap(cfg.Metrics)}, nil
}

// Login verifies an identity with its channel and returns the account,
// creating it on first sight.
//
// Verification happens before anything else and its result — not the submitted
// identity — decides the account. A caller cannot present its own credential
// alongside someone else's open id.
func (s *Service) Login(ctx context.Context, identity Identity) (Account, error) {
	if err := identity.Validate(); err != nil {
		return Account{}, err
	}
	verified, err := s.cfg.Verifier.Verify(ctx, identity)
	if err != nil {
		if errors.Is(err, ErrVerifierUnavailable) {
			s.report.Refused("login", "verifier_unavailable")
			// Propagated as-is: a channel outage is not a client error, and
			// answering "denied" would tell the player their credential is
			// bad when it is not.
			return Account{}, err
		}
		s.report.Refused("login", "denied")
		return Account{}, fmt.Errorf("%w: %s", ErrIdentityDenied, err)
	}
	if err := verified.Validate(); err != nil {
		return Account{}, err
	}
	accountID := Identity{Channel: verified.Channel, OpenID: verified.OpenID}.AccountID()
	now := s.cfg.Now()

	var result Account
	_, _, err = s.cfg.Accounts.Update(ctx, accountID, func(current Account, found bool) (Account, bool, error) {
		if found && current.Banned {
			// A banned account still resolves, so an operator can see it, but
			// login does not succeed.
			result = current
			s.report.Refused("login", "banned")
			return current, false, fmt.Errorf("%w: account is banned", ErrIdentityDenied)
		}
		next := current
		if !found {
			next = Account{ID: accountID, Channel: verified.Channel, OpenID: verified.OpenID, CreatedAtUnix: now.Unix()}
		}
		next.LastLoginAtUnix = now.Unix()
		result = next
		return next, true, nil
	})
	if err != nil {
		return Account{}, err
	}
	s.report.Accepted("login")
	return result, nil
}

// CreateRole allocates a role under an account on a server.
//
// The order is deliberate and is the whole point of the two directories:
//
//  1. Reserve the per-server slot. An account that already holds its allowance
//     on this server loses here, atomically, rather than by a count that two
//     callers can both read as zero.
//  2. Reserve the name. Losing here releases the slot.
//  3. Allocate the player id and insert the role. Insert-only: an id
//     collision fails instead of overwriting an existing player.
//  4. Commit both claims.
//
// A crash at any point leaves claims that expire, so nothing is burned. The
// implementation this replaces wrote the name and the role to two collections
// with no transaction and dropped the error from its compensating release, so
// a crash between them reserved a name to a role that did not exist, with no
// code path able to free it.
func (s *Service) CreateRole(ctx context.Context, accountID string, serverID int32, name string) (Role, error) {
	if strings.TrimSpace(accountID) == "" {
		return Role{}, fmt.Errorf("%w: account id is empty", ErrAccountMissing)
	}
	if err := s.cfg.NameRules.Validate(name); err != nil {
		return Role{}, fmt.Errorf("%w: %s", ErrNameInvalid, err)
	}
	account, found, err := s.cfg.Accounts.Get(ctx, accountID)
	if err != nil {
		return Role{}, err
	}
	if !found {
		return Role{}, fmt.Errorf("%w: %s", ErrAccountMissing, accountID)
	}
	if account.Value.Banned {
		return Role{}, fmt.Errorf("%w: account is banned", ErrIdentityDenied)
	}
	server, found, err := s.cfg.Servers.Get(ctx, serverID)
	if err != nil {
		return Role{}, err
	}
	if !found {
		return Role{}, fmt.Errorf("%w: server %d is unknown", ErrServerInvalid, serverID)
	}
	if !server.Value.Status.acceptsNewRoles() {
		s.report.Refused("create_role", "server_closed")
		return Role{}, fmt.Errorf("%w: server %d is %s", ErrServerClosed, serverID, server.Value.Status)
	}

	slotKey := slotKeyFor(accountID, serverID)
	slot, claimed, err := s.cfg.Slots.Create(ctx, slotKey, Slot{AccountID: accountID, ServerID: serverID})
	if err != nil {
		return Role{}, err
	}
	if !claimed {
		// Insert-only, so exactly one creator wins — including when the same
		// account races itself, which a same-owner-idempotent reservation
		// would have let through.
		s.report.Refused("create_role", "role_limit")
		return Role{}, fmt.Errorf("%w: %s already holds a role on server %d", ErrRoleLimit, accountID, serverID)
	}

	nameClaim, err := s.cfg.Names.Reserve(ctx, name, directory.Owner(accountID), s.cfg.ClaimTTL)
	if err != nil {
		if releaseErr := s.releaseSlot(ctx, slotKey, slot); releaseErr != nil {
			err = errors.Join(err, releaseErr)
		}
		if errors.Is(err, directory.ErrKeyTaken) {
			s.report.Refused("create_role", "name_taken")
			return Role{}, fmt.Errorf("%w: %q", ErrNameTaken, name)
		}
		return Role{}, err
	}

	playerID, err := s.cfg.Allocator.Allocate(ctx, serverID)
	if err != nil {
		return Role{}, errors.Join(err, s.rollback(ctx, nameClaim, slotKey, slot))
	}
	if playerID == 0 {
		return Role{}, errors.Join(
			fmt.Errorf("account: allocator returned a zero player id"),
			s.rollback(ctx, nameClaim, slotKey, slot))
	}

	now := s.cfg.Now()
	role := Role{
		PlayerID: playerID, AccountID: accountID, ServerID: serverID,
		Name: strings.TrimSpace(name), CreatedAtUnix: now.Unix(),
	}
	// Insert-only. An id that is already taken fails here instead of
	// overwriting the player who holds it.
	storedRole, created, err := s.cfg.Roles.Create(ctx, playerID, role)
	if err != nil {
		return Role{}, errors.Join(err, s.rollback(ctx, nameClaim, slotKey, slot))
	}
	if !created {
		s.report.Conflict("create_role")
		return Role{}, errors.Join(
			fmt.Errorf("%w: player id %d is already in use", ErrConflict, playerID),
			s.rollback(ctx, nameClaim, slotKey, slot))
	}

	// The commit point is the slot write at the very end. Everything before it
	// is reversible and IS reversed on failure — including the role record,
	// which nobody has been handed yet. Until this fix the two tail failures
	// returned the error and kept the role: a lost name commit left a role
	// whose name claim lapsed and could then be taken by another account, and
	// a lost slot write left a role the retrying client was refused for with
	// ErrRoleLimit. Success is not reported before the commit point, and
	// nothing that precedes it may survive a failure (U-0021).
	if _, err := s.cfg.Names.Commit(ctx, nameClaim); err != nil {
		return Role{}, errors.Join(err,
			s.rollbackRole(ctx, storedRole),
			s.rollback(ctx, nameClaim, slotKey, slot))
	}
	// Record which role occupies the slot, now that there is one.
	if _, _, err := s.cfg.Slots.Update(ctx, slotKey, func(current Slot, found bool) (Slot, bool, error) {
		if !found {
			return current, false, fmt.Errorf("%w: slot %s vanished during create", ErrConflict, slotKey)
		}
		current.PlayerID = playerID
		return current, true, nil
	}); err != nil {
		// The name is committed by now, so it is released as an owner would
		// release it, not cancelled as a claim.
		var joined error
		if releaseErr := s.cfg.Names.Release(ctx, role.Name, directory.Owner(accountID)); releaseErr != nil {
			joined = errors.Join(joined, fmt.Errorf("release committed name: %w", releaseErr))
		}
		joined = errors.Join(joined, s.rollbackRole(ctx, storedRole), s.releaseSlot(ctx, slotKey, slot))
		if joined != nil {
			s.report.Dropped("rollback.failed", 1)
		}
		return Role{}, errors.Join(err, joined)
	}
	s.report.Accepted("create_role")
	return role, nil
}

// rollbackRole removes a role record the caller was never handed, version
// checked so it cannot remove a role that has since been touched by anything
// else.
func (s *Service) rollbackRole(ctx context.Context, stored versionstore.Versioned[Role]) error {
	if err := s.cfg.Roles.Delete(ctx, stored.Value.PlayerID, stored); err != nil {
		if errors.Is(err, versionstore.ErrVersionMismatch) {
			return nil
		}
		s.report.Dropped("rollback.failed", 1)
		return fmt.Errorf("remove role %d: %w", stored.Value.PlayerID, err)
	}
	return nil
}

// rollback undoes what a failed create took.
//
// A failure here is reported rather than dropped — the implementation this
// replaces discarded the error from its compensating release, so a name could
// be reserved to a role that did not exist with no path to free it. The name
// claim additionally carries a TTL, so even a rollback that fails outright
// lapses instead of burning the name. The slot has no TTL, so its release is
// the one that must be reported: an unreleased slot blocks that account on
// that server until an operator intervenes.
func (s *Service) rollback(ctx context.Context, nameClaim directory.Claim, slotKey string, slot versionstore.Versioned[Slot]) error {
	var joined error
	if err := s.cfg.Names.Cancel(ctx, nameClaim); err != nil && !errors.Is(err, directory.ErrClaimStale) {
		joined = errors.Join(joined, fmt.Errorf("release name claim: %w", err))
	}
	if err := s.releaseSlot(ctx, slotKey, slot); err != nil {
		joined = errors.Join(joined, err)
	}
	if joined != nil {
		// The slot is still held by a create that failed. This is the one
		// leak in the package that does not expire on its own.
		s.report.Dropped("rollback.failed", 1)
	}
	return joined
}

// releaseSlot frees an occupancy record, version-checked so it cannot remove
// a slot another creator has since taken.
func (s *Service) releaseSlot(ctx context.Context, slotKey string, slot versionstore.Versioned[Slot]) error {
	if err := s.cfg.Slots.Delete(ctx, slotKey, slot); err != nil {
		if errors.Is(err, versionstore.ErrVersionMismatch) {
			// Someone else owns it now, which means ours was already gone.
			return nil
		}
		return fmt.Errorf("release slot: %w", err)
	}
	return nil
}

// slotKeyFor renders the exclusive-membership key for one account's role
// allowance on one server.
//
// The directory enforces one owner per key, which is exactly "one role per
// account per server" — the common case and the default. An allowance above
// one needs one key per slot, which is why RolesPerServer above one is
// rejected at construction rather than silently enforced as one.
func slotKeyFor(accountID string, serverID int32) string {
	return accountID + "@" + strconv.FormatInt(int64(serverID), 10)
}

// SelectRole issues a session token for a role the account owns.
//
// The token is signed **before** the login timestamp is persisted. The
// implementation this replaces persisted first and signed second, so a signing
// failure left the write durable and the caller retried, bumping the version
// again on every attempt.
func (s *Service) SelectRole(ctx context.Context, accountID string, playerID int64) (Session, error) {
	role, found, err := s.cfg.Roles.Get(ctx, playerID)
	if err != nil {
		return Session{}, err
	}
	if !found {
		return Session{}, fmt.Errorf("%w: player %d", ErrRoleMissing, playerID)
	}
	if role.Value.AccountID != accountID {
		s.report.Refused("select_role", "not_owner")
		return Session{}, fmt.Errorf("%w: player %d", ErrNotPermitted, playerID)
	}
	now := s.cfg.Now()
	token, err := security.SignSessionToken(playerID, s.cfg.SessionSecret, s.cfg.SessionTTL, now)
	if err != nil {
		return Session{}, err
	}
	if _, _, err := s.cfg.Roles.Update(ctx, playerID, func(current Role, found bool) (Role, bool, error) {
		if !found {
			return current, false, fmt.Errorf("%w: player %d", ErrRoleMissing, playerID)
		}
		if current.AccountID != accountID {
			return current, false, fmt.Errorf("%w: player %d", ErrNotPermitted, playerID)
		}
		current.LastLoginAtUnix = now.Unix()
		return current, true, nil
	}); err != nil {
		return Session{}, err
	}
	return Session{
		PlayerID: playerID, AccountID: accountID, ServerID: role.Value.ServerID,
		Token: token, ExpiresAtUnix: now.Add(s.cfg.SessionTTL).Unix(),
	}, nil
}

// ValidateSession verifies a token and returns the role it names.
func (s *Service) ValidateSession(ctx context.Context, playerID int64, token string) (Role, error) {
	if _, err := security.VerifySessionToken(token, s.cfg.SessionSecret, playerID, s.cfg.Now()); err != nil {
		s.report.Refused("validate_session", "bad_token")
		return Role{}, fmt.Errorf("%w: %s", ErrSessionInvalid, err)
	}
	role, found, err := s.cfg.Roles.Get(ctx, playerID)
	if err != nil {
		return Role{}, err
	}
	if !found {
		return Role{}, fmt.Errorf("%w: player %d", ErrRoleMissing, playerID)
	}
	return role.Value, nil
}

// UpdateProfile replaces a role's opaque game profile.
//
// This is the only way a role's mutable payload changes, and it can change
// nothing else: not the account it belongs to, not its name, not its server.
// The implementation this replaces had one merge-upsert that could reparent a
// role to another account and rename it while orphaning the old reservation.
func (s *Service) UpdateProfile(ctx context.Context, accountID string, playerID int64, profile []byte) (Role, error) {
	if len(profile) > MaxProfileBytes {
		// A size violation is a range error the caller can act on. It was
		// ErrConflict, which a client reads as "retry" — and it was the only
		// reason ErrRangeInvalid had no producer in this package.
		return Role{}, fmt.Errorf("%w: profile is %d bytes, limit %d", ErrRangeInvalid, len(profile), MaxProfileBytes)
	}
	var result Role
	_, _, err := s.cfg.Roles.Update(ctx, playerID, func(current Role, found bool) (Role, bool, error) {
		if !found {
			return current, false, fmt.Errorf("%w: player %d", ErrRoleMissing, playerID)
		}
		if current.AccountID != accountID {
			s.report.Refused("update_profile", "not_owner")
			return current, false, fmt.Errorf("%w: player %d", ErrNotPermitted, playerID)
		}
		current.Profile = append([]byte(nil), profile...)
		result = current
		return current, true, nil
	})
	if err != nil {
		return Role{}, err
	}
	return result, nil
}

// MarkLogout stamps the logout time.
func (s *Service) MarkLogout(ctx context.Context, accountID string, playerID int64) error {
	now := s.cfg.Now()
	_, _, err := s.cfg.Roles.Update(ctx, playerID, func(current Role, found bool) (Role, bool, error) {
		if !found {
			return current, false, fmt.Errorf("%w: player %d", ErrRoleMissing, playerID)
		}
		if current.AccountID != accountID {
			s.report.Refused("mark_logout", "not_owner")
			return current, false, fmt.Errorf("%w: player %d", ErrNotPermitted, playerID)
		}
		current.LastLogoutAtUnix = now.Unix()
		return current, true, nil
	})
	return err
}

// UpsertServer records a server. Operator-facing.
func (s *Service) UpsertServer(ctx context.Context, server GameServer) (GameServer, error) {
	if server.ID == 0 {
		return GameServer{}, fmt.Errorf("%w: id is zero", ErrServerInvalid)
	}
	if server.Status == "" {
		server.Status = ServerOpen
	}
	now := s.cfg.Now()
	var result GameServer
	_, _, err := s.cfg.Servers.Update(ctx, server.ID, func(current GameServer, _ bool) (GameServer, bool, error) {
		next := server
		next.UpdatedAtUnix = now.Unix()
		result = next
		return next, true, nil
	})
	if err != nil {
		return GameServer{}, err
	}
	return result, nil
}
