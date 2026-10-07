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

	"github.com/tjbdwanghaibo/roost-core/kit/service/servicemetrics"

	"github.com/tjbdwanghaibo/roost-core/kit/service/directory"
)

// Config wires a Service. Every field without a safe default is required, and
// the constructor refuses an incomplete one — the alternative is a service
// that starts and then behaves as if a missing piece were a policy choice.
type Config struct {
	// Accounts, Roles and Servers hold the durable state.
	Accounts versionstore.Store[string, Account]
	Roles    versionstore.Store[int64, Role]
	Servers  versionstore.Store[int32, GameServer]

	// Names reserves display names. Slot intents retain the reservation and
	// role identity for recovery after uncertain writes.
	Names directory.Directory
	// Slots stores one durable role plan per account/server. Insert-only
	// creation excludes different plans; same-name retries resume the same ID.
	// Stores used here must preserve ConditionalDeleter for safe pre-role cleanup.
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
	// ClaimTTL is how long an uncommitted name claim is held while a role is being
	// created; zero selects DefaultClaimTTL.
	ClaimTTL time.Duration
	// Now is the business clock (D-L3 round 8; the account Mod injects
	// app.BusinessClock, real time + time.logic_offset): the times a game
	// shows or builds rules on — account creation, role creation, last login
	// and logout. nil means time.Now.
	Now func() time.Time
	// SystemNow is the system clock: session token issue and expiry, and the
	// operator-facing times (UpsertServer, ResolvePendingCreation's audit
	// stamp). A token's life is a security bound in real time and an audit
	// says when an operator acted, so neither moves with the business offset.
	// nil means Now, so a test that drives both with one clock keeps doing so;
	// the account Mod injects time.Now. Name claims during role creation run
	// on the name directory's own clock, which the Mod leaves on time.Now.
	SystemNow func() time.Time
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
	} else if _, ok := cfg.Slots.(versionstore.ConditionalDeleter[string, Slot]); !ok {
		missing = append(missing, "Slots (atomic identity-checked DeleteIf is required)")
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
	if cfg.SystemNow == nil {
		cfg.SystemNow = cfg.Now
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
	legacyID := strings.ToLower(strings.TrimSpace(string(verified.Channel))) + ":" + strings.TrimSpace(verified.OpenID)
	if legacyID != accountID {
		legacy, found, err := s.cfg.Accounts.Get(ctx, legacyID)
		if err != nil {
			return Account{}, err
		}
		if found && sameVerifiedIdentity(legacy.Value, verified) {
			accountID = legacyID
		}
	}
	now := s.cfg.Now()

	var result Account
	_, _, err = s.cfg.Accounts.Update(ctx, accountID, func(current Account, found bool) (Account, bool, error) {
		if found && !sameVerifiedIdentity(current, verified) {
			return current, false, fmt.Errorf("%w: stored account identity does not match verified identity", ErrIdentityInvalid)
		}
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

// CreateRole creates or resumes one pending role with the same name.
// The slot persists its plan before other writes. Unknown results retain the
// plan for retry; a completed slot still refuses a second role.
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

	return s.createRole(ctx, accountID, serverID, strings.TrimSpace(name))
}

func sameVerifiedIdentity(account Account, identity Verified) bool {
	return strings.EqualFold(strings.TrimSpace(string(account.Channel)), strings.TrimSpace(string(identity.Channel))) && strings.TrimSpace(account.OpenID) == strings.TrimSpace(identity.OpenID)
}

// slotKeyFor renders the exclusive-membership key for one account's role
// allowance on one server.
//
// The insert-only slot enforces one role plan per key, exactly "one role per
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
	if err := s.roleReady(ctx, role.Value); err != nil {
		return Session{}, err
	}
	// The login stamp is business time; the token is system time (D-L3).
	now, issued := s.cfg.Now(), s.cfg.SystemNow()
	token, err := security.SignSessionToken(playerID, s.cfg.SessionSecret, s.cfg.SessionTTL, issued)
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
		Token: token, ExpiresAtUnix: issued.Add(s.cfg.SessionTTL).Unix(),
	}, nil
}

// ValidateSession verifies a token and returns the role it names.
func (s *Service) ValidateSession(ctx context.Context, playerID int64, token string) (Role, error) {
	if _, err := security.VerifySessionToken(token, s.cfg.SessionSecret, playerID, s.cfg.SystemNow()); err != nil {
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
	if err := s.roleReady(ctx, role.Value); err != nil {
		return Role{}, err
	}
	return role.Value.clone(), nil
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
	stored, found, err := s.cfg.Roles.Get(ctx, playerID)
	if err != nil {
		return Role{}, err
	}
	if found {
		if stored.Value.AccountID != accountID {
			s.report.Refused("update_profile", "not_owner")
			return Role{}, fmt.Errorf("%w: player %d", ErrNotPermitted, playerID)
		}
		if err := s.roleReady(ctx, stored.Value); err != nil {
			return Role{}, err
		}
	}
	var result Role
	_, _, err = s.cfg.Roles.Update(ctx, playerID, func(current Role, found bool) (Role, bool, error) {
		if !found {
			return current, false, fmt.Errorf("%w: player %d", ErrRoleMissing, playerID)
		}
		if current.AccountID != accountID {
			s.report.Refused("update_profile", "not_owner")
			return current, false, fmt.Errorf("%w: player %d", ErrNotPermitted, playerID)
		}
		current.Profile = append([]byte(nil), profile...)
		result = current.clone()
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
	if server.ID <= 0 {
		return GameServer{}, fmt.Errorf("%w: id is zero or negative", ErrServerInvalid)
	}
	if server.Status == "" {
		server.Status = ServerOpen
	}
	switch server.Status {
	case ServerOpen, ServerFull, ServerClosed, ServerMaintenance:
	default:
		return GameServer{}, fmt.Errorf("%w: unsupported status %q", ErrServerInvalid, server.Status)
	}
	now := s.cfg.SystemNow() // an operator record, not game time
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
