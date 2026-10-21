package mail

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	businessclock "github.com/tjbdwanghaibo/roost-core/infra/base/clock"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

// 全服邮件“存一份 + 拉取合并”（DESIGN-TODO D17，维护者 2026-10-20 决定 A）。
//
// 一封广播（AudienceBroadcast）发出时只存一份信封，并在广播账本里登记一条 GlobalMail，拿到一个
// 单调递增的全局邮件 ID。每个玩家的邮箱记一个水位（BroadcastWatermark：已合并到的最大全局邮件
// ID）和最近一次登录时给出的画像（BroadcastProfile：所在范围、注册时间、业务属性）。合并就是在
// 邮箱的同一次 compare-and-set 里，把账本中 ID 大于水位、范围匹配、未过期、未撤回、满足条件的
// 全局邮件写成邮箱条目（条目键是广播信封 id，Entry.GlobalID 记全局邮件 ID），并把水位推进到账本头。
//
// 由此得到的保证：
//   - 去重按全局邮件：条目键是信封 id，水位与条目在同一次 CAS 里移动，所以并发的登录、List、
//     Summary、重复拉取只合并一次；已读 / 已领 / 删除记在玩家自己那条上，附件领取沿用原有的
//     恒定 token，只领一次。
//   - 删除后不重现：水位已越过它，条目被保留策略清掉之后也不会再合并。
//   - 邮箱满（全是未读）时合并停在放不进去的那封之前，水位不越过它，腾出位置后补上，不丢。
//
// 不保证的：画像是玩家最近一次 PullBroadcasts 给的。范围（例如公会）变化之后要重新拉取，否则
// List 时仍按旧画像合并、水位越过新范围里已发出的邮件。条件在合并时只判断一次，判为不满足的
// 不会因为玩家之后满足了（例如升级）而重新判断。

// Limits on what one profile and one ledger may hold.
const (
	// DefaultMaxActiveBroadcasts bounds the ledger: how many unexpired global
	// mails may exist at once. Publishing over it is refused (ErrBroadcastFull)
	// rather than growing the one document every merge reads.
	DefaultMaxActiveBroadcasts = 1000
	// MaxProfileScopes bounds BroadcastProfile.Scopes.
	MaxProfileScopes = 8
	// MaxProfileAttributes bounds BroadcastProfile.Attributes.
	MaxProfileAttributes = 16
	// MaxScopeBytes bounds one scope string and one attribute name.
	MaxScopeBytes = 128
)

// ServerScope is the canonical scope string for one game server: a broadcast
// sent with Scope ServerScope(sid) reaches the players whose profile lists it.
// Scope is opaque to this package; these helpers only keep sender and login
// endpoint spelling it the same way. The empty scope is "every player".
func ServerScope(serverID int32) string {
	return "server:" + strconv.FormatInt(int64(serverID), 10)
}

// GuildScope is the canonical scope string for one guild.
func GuildScope(guildID int64) string {
	return "guild:" + strconv.FormatInt(guildID, 10)
}

// BroadcastProfile is what a lazy broadcast needs to know about one player to
// decide which global mails are theirs. The login endpoint passes it to
// PullBroadcasts; the mailbox keeps the latest one so List and Summary can
// top up without asking again.
type BroadcastProfile struct {
	// Scopes the player belongs to besides "every player" — typically
	// ServerScope(role's server) and GuildScope(guild) when in one.
	Scopes []string `json:"scopes,omitempty"`
	// RegisteredAtUnix is when the player registered (for a per-server role,
	// account's Role.CreatedAtUnix). Required: the built-in condition is
	// "registered before the mail was sent", so a newcomer does not receive
	// old compensation, and an unknown registration time cannot answer it.
	RegisteredAtUnix int64 `json:"registered_at_unix"`
	// Attributes are inputs for the business predicate
	// (LazyBroadcastConfig.Eligible): level, VIP tier, channel id, ...
	Attributes map[string]int64 `json:"attributes,omitempty"`
}

// Validate checks the bounds and the required registration time.
func (p BroadcastProfile) Validate() error {
	if p.RegisteredAtUnix <= 0 {
		return fmt.Errorf("%w: a broadcast profile needs the player's registration time", ErrRequestInvalid)
	}
	if len(p.Scopes) > MaxProfileScopes {
		return fmt.Errorf("%w: %d scopes, limit %d", ErrRequestInvalid, len(p.Scopes), MaxProfileScopes)
	}
	for _, scope := range p.Scopes {
		if strings.TrimSpace(scope) == "" || len(scope) > MaxScopeBytes {
			return fmt.Errorf("%w: scope %q must be non-empty and at most %d bytes", ErrRequestInvalid, scope, MaxScopeBytes)
		}
	}
	if len(p.Attributes) > MaxProfileAttributes {
		return fmt.Errorf("%w: %d attributes, limit %d", ErrRequestInvalid, len(p.Attributes), MaxProfileAttributes)
	}
	for name := range p.Attributes {
		if strings.TrimSpace(name) == "" || len(name) > MaxScopeBytes {
			return fmt.Errorf("%w: attribute name %q must be non-empty and at most %d bytes", ErrRequestInvalid, name, MaxScopeBytes)
		}
	}
	return nil
}

// covers reports whether a broadcast with this scope addresses the player.
func (p BroadcastProfile) covers(scope string) bool {
	return scope == "" || slices.Contains(p.Scopes, scope)
}

func (p BroadcastProfile) clone() BroadcastProfile {
	out := p
	out.Scopes = slices.Clone(p.Scopes)
	if p.Attributes != nil {
		out.Attributes = make(map[string]int64, len(p.Attributes))
		for name, value := range p.Attributes {
			out.Attributes[name] = value
		}
	}
	return out
}

func (p BroadcastProfile) equal(other BroadcastProfile) bool {
	if p.RegisteredAtUnix != other.RegisteredAtUnix || !slices.Equal(p.Scopes, other.Scopes) ||
		len(p.Attributes) != len(other.Attributes) {
		return false
	}
	for name, value := range p.Attributes {
		if got, ok := other.Attributes[name]; !ok || got != value {
			return false
		}
	}
	return true
}

// GlobalMail is one broadcast's entry in the ledger. The content stays in the
// envelope (stored once); this is only what a merge decides on.
type GlobalMail struct {
	// ID is the global mail id: assigned at publish, strictly increasing, the
	// unit a player's watermark counts in.
	ID uint64 `json:"id"`
	// MailID is the broadcast envelope's id — also the key of the merged
	// mailbox entry, which is what makes a merge idempotent per global mail.
	MailID        string `json:"mail_id"`
	Scope         string `json:"scope,omitempty"`
	SentAtUnix    int64  `json:"sent_at_unix"`
	ExpiresAtUnix int64  `json:"expires_at_unix"`
	// RevokedAtUnix, when set, stops further merges. Copies already merged
	// stay. The record is kept until it expires so a replayed send cannot
	// publish it again.
	RevokedAtUnix int64 `json:"revoked_at_unix,omitempty"`
}

// BroadcastLedger is every live global mail, in id order, and the last id
// handed out. Head never decreases, even when expired records are reclaimed.
type BroadcastLedger struct {
	Head  uint64       `json:"head"`
	Mails []GlobalMail `json:"mails,omitempty"`
}

func (l BroadcastLedger) find(mailID string) (int, bool) {
	for index, mail := range l.Mails {
		if mail.MailID == mailID {
			return index, true
		}
	}
	return -1, false
}

// reclaimed returns a copy without the records that expired by nowUnix.
func (l BroadcastLedger) reclaimed(nowUnix int64) BroadcastLedger {
	out := BroadcastLedger{Head: l.Head, Mails: make([]GlobalMail, 0, len(l.Mails)+1)}
	for _, mail := range l.Mails {
		if mail.ExpiresAtUnix > nowUnix {
			out.Mails = append(out.Mails, mail)
		}
	}
	return out
}

// BroadcastIndex stores the ledger. Two implementations ship:
// NewRedisBroadcastIndex (what the wiring Mod uses) and NewMemoryBroadcastIndex
// (tests and single-process development; nothing in it survives a restart).
type BroadcastIndex interface {
	// Publish records a broadcast envelope and returns its ledger entry. It is
	// idempotent per envelope id — a retried delivery returns the entry it
	// already has — and it reclaims expired records before checking the bound.
	Publish(ctx context.Context, envelope Envelope, nowUnix int64, maxActive int) (GlobalMail, error)
	// Revoke stops further merges of one broadcast. It reports false when the
	// broadcast was already revoked, and found=false when the ledger does not
	// hold it.
	Revoke(ctx context.Context, mailID string, nowUnix int64) (mail GlobalMail, changed bool, err error)
	// Snapshot returns the ledger. It must be cheap when nothing changed: it is
	// on the List / Summary path of every player with a profile.
	Snapshot(ctx context.Context) (BroadcastLedger, error)
}

// LazyBroadcastConfig configures NewLazyBroadcast. The zero value is usable
// inside the wiring Mod, which supplies the Redis index.
type LazyBroadcastConfig struct {
	// Index stores the ledger. nil lets the wiring Mod supply the Redis index
	// under mail.key_prefix; a Service built directly must set it.
	Index BroadcastIndex
	// Eligible is the business predicate, applied after scope, expiry,
	// revocation and the built-in "registered before sent". nil admits
	// everything that passes those. It runs inside the mailbox compare-and-set
	// and may run more than once per merge, so it must be pure and fast — no
	// I/O; everything it needs belongs in BroadcastProfile.Attributes.
	Eligible func(profile BroadcastProfile, mail GlobalMail) bool
	// MaxActive bounds the ledger; zero selects DefaultMaxActiveBroadcasts.
	MaxActive int
}

// LazyBroadcast is a Deliverer that stores a broadcast once and lets each
// mailbox pull it: at login (Service.PullBroadcasts) and before List /
// Summary. It is the built-in strategy for AudienceBroadcast; nil Broadcast
// (refuse broadcasts) stays the default.
//
//	func Broadcast() mail.Deliverer { return mail.NewLazyBroadcast(mail.LazyBroadcastConfig{}) }
//
// The Service recognises it by type: wrapping it in another Deliverer hides it
// and nothing is ever merged.
type LazyBroadcast struct {
	cfg   LazyBroadcastConfig
	index BroadcastIndex
	now   func() time.Time
	// bound is set by the one Service that owns this strategy; a second
	// Service would merge with a different clock and metrics.
	bound atomic.Bool
}

// NewLazyBroadcast returns the strategy. See LazyBroadcastConfig.
func NewLazyBroadcast(cfg LazyBroadcastConfig) *LazyBroadcast {
	if cfg.MaxActive <= 0 {
		cfg.MaxActive = DefaultMaxActiveBroadcasts
	}
	return &LazyBroadcast{cfg: cfg, index: cfg.Index, now: businessclock.Now}
}

// HasIndex reports whether a ledger store is set.
func (b *LazyBroadcast) HasIndex() bool { return b.index != nil }

// UseIndex sets the ledger store when none was configured. The wiring Mod
// calls it in Provide, before the Service is built.
func (b *LazyBroadcast) UseIndex(index BroadcastIndex) error {
	if index == nil {
		return fmt.Errorf("mail: lazy broadcast index is nil")
	}
	if b.index != nil {
		return fmt.Errorf("mail: lazy broadcast already has an index")
	}
	b.index = index
	return nil
}

// bind attaches the strategy to its Service.
func (b *LazyBroadcast) bind(now func() time.Time) error {
	if b.index == nil {
		return fmt.Errorf("mail: lazy broadcast has no index; set LazyBroadcastConfig.Index " +
			"(the wiring Mod supplies the Redis one)")
	}
	if !b.bound.CompareAndSwap(false, true) {
		return fmt.Errorf("mail: a lazy broadcast belongs to one Service")
	}
	b.now = now
	return nil
}

// Deliver implements Deliverer: it publishes the envelope to the ledger. It
// is idempotent per envelope id, so the Service's replay of an unstamped send
// is safe.
func (b *LazyBroadcast) Deliver(ctx context.Context, envelope Envelope) error {
	if b.index == nil {
		return fmt.Errorf("mail: lazy broadcast has no index")
	}
	_, err := b.index.Publish(ctx, envelope, b.now().Unix(), b.cfg.MaxActive)
	return err
}

// admits is the whole merge condition for one global mail and one player.
func (b *LazyBroadcast) admits(profile BroadcastProfile, mail GlobalMail, nowUnix int64) bool {
	switch {
	case mail.RevokedAtUnix != 0:
		return false
	case nowUnix >= mail.ExpiresAtUnix:
		return false
	case !profile.covers(mail.Scope):
		return false
	case profile.RegisteredAtUnix >= mail.SentAtUnix:
		// 内置条件：注册时间早于发送时间（严格早于；同一秒注册的不算老玩家）。
		return false
	case b.cfg.Eligible != nil && !b.cfg.Eligible(profile, mail):
		return false
	default:
		return true
	}
}

var _ Deliverer = (*LazyBroadcast)(nil)

// mergeBroadcasts merges the ledger's global mails above the watermark into
// this mailbox. It is a pure function of its arguments (it runs inside a
// compare-and-set callback). It returns how many entries it added and whether
// it stopped early because the mailbox could not take one more.
func (m *Mailbox) mergeBroadcasts(ledger BroadcastLedger, admit func(GlobalMail) bool, nowUnix int64) (merged int, stopped bool) {
	if ledger.Head <= m.BroadcastWatermark {
		return 0, false
	}
	watermark := ledger.Head
	for _, mail := range ledger.Mails {
		if mail.ID <= m.BroadcastWatermark || !admit(mail) {
			continue
		}
		if _, ok := m.Entries[mail.MailID]; ok {
			continue
		}
		if _, ok := m.SettledClaims[mail.MailID]; ok {
			continue
		}
		// 放不下就停在这封之前：水位不越过它，腾出位置后下一次合并补上。未读不淘汰，所以这里
		// 不能像信封投递那样报错，只能等。
		if m.full() {
			watermark, stopped = mail.ID-1, true
			break
		}
		var backup Mailbox
		nearBound := len(m.Entries) >= MaxMailboxEntries
		if nearBound {
			backup = m.clone()
		}
		entry, added := m.deliver(mail.MailID, nowUnix)
		entry.EnvelopeExpiresAtUnix = mail.ExpiresAtUnix
		entry.GlobalID = mail.ID
		m.Entries[mail.MailID] = entry
		if added {
			m.evict(nowUnix)
		}
		if nearBound && m.settledClaimsOverflow() {
			*m = backup
			watermark, stopped = mail.ID-1, true
			break
		}
		if added {
			merged++
		}
	}
	if watermark > m.BroadcastWatermark {
		m.BroadcastWatermark = watermark
		m.UpdatedAtUnix = nowUnix
	}
	return merged, stopped
}

// ledgerKey is the single key the ledger lives under.
const ledgerKey = "ledger"

// versionHint is a cheap, monotonic copy of the ledger's version kept beside
// it, so a reader can tell "unchanged since my cached copy" with one O(1)
// read instead of reading the whole ledger.
type versionHint interface {
	Load(ctx context.Context) (version uint64, found bool, err error)
	// Raise sets the hint to version if that is higher than what it holds.
	Raise(ctx context.Context, version uint64) error
}

// ledgerIndex is the BroadcastIndex over one versioned document.
//
// One document, deliberately: assigning the next id and recording the mail
// in the same compare-and-set is what makes "every id ≤ head is visible"
// true, and that is what lets a watermark be a single number. Separate keys
// for the counter and the records would let a reader see id 8 before id 7
// committed and move a watermark past a mail nobody would ever merge. The
// document's size is bounded by MaxActive.
type ledgerIndex struct {
	store versionstore.Store[string, BroadcastLedger]
	// hint is nil for the memory store, whose Get is already cheap.
	hint  versionHint
	cache atomic.Pointer[versionstore.Versioned[BroadcastLedger]]
}

// NewMemoryBroadcastIndex is a BroadcastIndex in this process's memory: for
// tests and single-process development. Nothing in it survives a restart and
// another process cannot see it.
func NewMemoryBroadcastIndex() BroadcastIndex {
	return &ledgerIndex{store: versionstore.NewMemoryStore[string, BroadcastLedger]()}
}

// NewRedisBroadcastIndex is the BroadcastIndex over Redis: the ledger under
// "<prefix>:bcast:ledger" and its version hint under "<prefix>:bcast:version".
// Both are single keys, so it works unchanged on Redis Cluster; the whole
// ledger is one key on one node, which is the point (see ledgerIndex).
func NewRedisBroadcastIndex(client fredis.IRedis, prefix string, maxAttempts int, retryBackoff time.Duration) (BroadcastIndex, error) {
	if client == nil {
		return nil, fmt.Errorf("mail: redis client is nil")
	}
	if strings.TrimSpace(prefix) == "" {
		return nil, fmt.Errorf("mail: redis key prefix is required")
	}
	store, err := versionstore.NewRedisStore(client, versionstore.RedisConfig[string, BroadcastLedger]{
		Prefix:       prefix + ":bcast:",
		KeyOf:        func(key string) string { return key },
		Codec:        versionstore.JSONCodec[BroadcastLedger]{},
		MaxAttempts:  maxAttempts,
		RetryBackoff: retryBackoff,
	})
	if err != nil {
		return nil, fmt.Errorf("mail: broadcast ledger: %w", err)
	}
	return &ledgerIndex{store: store, hint: redisHint{client: client, key: prefix + ":bcast:version"}}, nil
}

func (x *ledgerIndex) Publish(ctx context.Context, envelope Envelope, nowUnix int64, maxActive int) (GlobalMail, error) {
	if envelope.Audience != AudienceBroadcast {
		return GlobalMail{}, fmt.Errorf("%w: only a broadcast is published to the ledger", ErrAudienceInvalid)
	}
	if strings.TrimSpace(envelope.ID) == "" {
		return GlobalMail{}, fmt.Errorf("%w: id is empty", ErrMailInvalid)
	}
	if envelope.Expired(nowUnix) {
		return GlobalMail{}, fmt.Errorf("%w: broadcast %s", ErrExpired, envelope.ID)
	}
	if maxActive <= 0 {
		maxActive = DefaultMaxActiveBroadcasts
	}
	sentAt := envelope.CreatedAtUnix
	if sentAt <= 0 {
		sentAt = nowUnix
	}
	var result GlobalMail
	stored, _, err := x.store.Update(ctx, ledgerKey, func(current BroadcastLedger, _ bool) (BroadcastLedger, bool, error) {
		if index, ok := current.find(envelope.ID); ok {
			result = current.Mails[index]
			return current, false, nil
		}
		next := current.reclaimed(nowUnix)
		if len(next.Mails) >= maxActive {
			return current, false, fmt.Errorf("%w: %d unexpired broadcasts, limit %d", ErrBroadcastFull, len(next.Mails), maxActive)
		}
		next.Head++
		result = GlobalMail{
			ID: next.Head, MailID: envelope.ID, Scope: envelope.Scope,
			SentAtUnix: sentAt, ExpiresAtUnix: envelope.ExpiresAtUnix,
		}
		next.Mails = append(next.Mails, result)
		return next, true, nil
	})
	if err != nil {
		return GlobalMail{}, err
	}
	// The hint is raised even when nothing was written: a delivery retried
	// after a lost hint write is what repairs it.
	if err := x.raise(ctx, stored.Version); err != nil {
		return GlobalMail{}, err
	}
	return result, nil
}

func (x *ledgerIndex) Revoke(ctx context.Context, mailID string, nowUnix int64) (GlobalMail, bool, error) {
	if strings.TrimSpace(mailID) == "" {
		return GlobalMail{}, false, fmt.Errorf("%w: mail id is empty", ErrMailInvalid)
	}
	var (
		result  GlobalMail
		found   bool
		changed bool
	)
	stored, _, err := x.store.Update(ctx, ledgerKey, func(current BroadcastLedger, _ bool) (BroadcastLedger, bool, error) {
		result, found, changed = GlobalMail{}, false, false
		index, ok := current.find(mailID)
		if !ok || current.Mails[index].ExpiresAtUnix <= nowUnix {
			return current, false, nil
		}
		found = true
		if current.Mails[index].RevokedAtUnix != 0 {
			result = current.Mails[index]
			return current, false, nil
		}
		next := BroadcastLedger{Head: current.Head, Mails: slices.Clone(current.Mails)}
		next.Mails[index].RevokedAtUnix = nowUnix
		result, changed = next.Mails[index], true
		return next, true, nil
	})
	if err != nil {
		return GlobalMail{}, false, err
	}
	if !found {
		return GlobalMail{}, false, fmt.Errorf("%w: broadcast %s is not in the ledger", ErrMailMissing, mailID)
	}
	if err := x.raise(ctx, stored.Version); err != nil {
		return GlobalMail{}, false, err
	}
	return result, changed, nil
}

func (x *ledgerIndex) Snapshot(ctx context.Context) (BroadcastLedger, error) {
	var (
		hinted     uint64
		hintExists bool
	)
	if x.hint != nil {
		version, found, err := x.hint.Load(ctx)
		if err != nil {
			return BroadcastLedger{}, err
		}
		hinted, hintExists = version, found
		if cached := x.cache.Load(); found && cached != nil && cached.Version == version {
			return cached.Value, nil
		}
	}
	stored, found, err := x.store.Get(ctx, ledgerKey)
	if err != nil {
		return BroadcastLedger{}, err
	}
	if !found {
		return BroadcastLedger{}, nil
	}
	// A hint that is missing or behind (its write was lost) is repaired here,
	// so the next reader gets the cheap path again.
	if x.hint != nil && (!hintExists || hinted < stored.Version) {
		if err := x.hint.Raise(ctx, stored.Version); err != nil {
			return BroadcastLedger{}, err
		}
	}
	x.cache.Store(&stored)
	return stored.Value, nil
}

func (x *ledgerIndex) raise(ctx context.Context, version uint64) error {
	if x.hint == nil || version == 0 {
		return nil
	}
	return x.hint.Raise(ctx, version)
}

// redisHint keeps the version hint in one plain Redis key.
type redisHint struct {
	client fredis.IRedis
	key    string
}

// raiseHintScript sets the key only upward, so two publishers finishing out
// of order cannot move the hint back to an older version.
const raiseHintScript = `
local current = tonumber(redis.call('GET', KEYS[1]) or '0')
local next = tonumber(ARGV[1])
if next > current then
  redis.call('SET', KEYS[1], ARGV[1])
end
return 1`

func (h redisHint) Load(ctx context.Context) (uint64, bool, error) {
	raw, err := h.client.Get(ctx, h.key)
	if err != nil {
		if errors.Is(err, fredis.ErrNil) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("mail: read broadcast version: %w", err)
	}
	version, err := strconv.ParseUint(string(raw), 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("mail: decode broadcast version %q: %w", raw, err)
	}
	return version, true, nil
}

func (h redisHint) Raise(ctx context.Context, version uint64) error {
	if _, err := h.client.Eval(ctx, raiseHintScript, []string{h.key}, strconv.FormatUint(version, 10)); err != nil {
		return fmt.Errorf("mail: raise broadcast version: %w", err)
	}
	return nil
}

var (
	_ BroadcastIndex = (*ledgerIndex)(nil)
	_ versionHint    = redisHint{}
)
