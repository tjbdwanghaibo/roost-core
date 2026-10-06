package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// RedisStores are the three stores this package needs, over Redis.
//
// Two of them are pure reuse: mailboxes and the send ledger are versioned
// state, and kit's versionstore.RedisStore already implements that contract.
// Only the envelope store needed writing, because an envelope is not
// versioned state — it is written once and never rewritten, and its read path
// is a BATCH, which is the whole reason EnvelopeStore has a GetMany.
type RedisStores struct {
	Envelopes EnvelopeStore
	Mailboxes MailboxStore
	Sends     SendLedger
}

// RedisConfig configures the three stores.
type RedisConfig struct {
	// Prefix namespaces every key. Required and non-empty.
	Prefix string
	// SendTTL expires send-ledger entries. Required and positive, and it must
	// exceed the longest client retry horizon: past it a retried send is
	// indistinguishable from a new one, and the recipient gets the mail twice.
	SendTTL time.Duration
	// MaxAttempts and RetryBackoff are passed through; zero selects defaults.
	MaxAttempts  int
	RetryBackoff time.Duration
	// Now is the clock the envelope store derives key ttls from. It must be
	// the SAME clock the Service uses: the ttl is computed from the expiry the
	// Service compares against, and two components disagreeing about the
	// current time is a mail that reads as live and has already been evicted.
	// In a deployment it is the business clock (D-L3; the kit Mod injects
	// app.BusinessClock). nil means time.Now.
	Now func() time.Time
	// StorageGrace is how much longer than the mail's business expiry its
	// envelope key lives. D-L3: storage ttl only reclaims space and must
	// outlive the business expiry, which the Service decides with
	// Envelope.Expired; the grace also covers time.logic_offset being moved
	// back between two runs of a test environment. Zero selects
	// DefaultEnvelopeStorageGrace; negative is refused.
	StorageGrace time.Duration
}

// DefaultEnvelopeStorageGrace is how long an envelope key outlives the mail's
// business expiry when RedisConfig.StorageGrace is zero.
const DefaultEnvelopeStorageGrace = 24 * time.Hour

// NewRedisStores builds them.
//
// Mailboxes carry no key ttl. A mailbox is a player's durable state, and a ttl
// on versioned state takes the version with the value: a mailbox that expired
// and was written again would restart at version 1, so a concurrent status
// change would compare against a version that had just reset. Its size is
// bounded by MaxMailboxEntries instead, which is enforced and counted.
func NewRedisStores(client fredis.IRedis, cfg RedisConfig) (RedisStores, error) {
	if client == nil {
		return RedisStores{}, fmt.Errorf("mail: redis client is nil")
	}
	if strings.TrimSpace(cfg.Prefix) == "" {
		return RedisStores{}, fmt.Errorf("mail: redis key prefix is required")
	}
	if cfg.SendTTL <= 0 {
		return RedisStores{}, fmt.Errorf("mail: send ttl must be positive; it must also exceed the " +
			"longest client retry horizon, or a retried send becomes a second mail")
	}

	if cfg.StorageGrace < 0 {
		return RedisStores{}, fmt.Errorf("mail: envelope storage grace must not be negative")
	}
	envelopes, err := NewRedisEnvelopes(client, cfg.Prefix, cfg.Now)
	if err != nil {
		return RedisStores{}, err
	}
	if cfg.StorageGrace > 0 {
		envelopes.(*redisEnvelopes).grace = cfg.StorageGrace
	}
	mailboxes, err := versionstore.NewRedisStore(client, versionstore.RedisConfig[int64, Mailbox]{
		Prefix:       cfg.Prefix + ":box:",
		KeyOf:        func(playerID int64) string { return strconv.FormatInt(playerID, 10) },
		Codec:        versionstore.JSONCodec[Mailbox]{},
		MaxAttempts:  cfg.MaxAttempts,
		RetryBackoff: cfg.RetryBackoff,
	})
	if err != nil {
		return RedisStores{}, fmt.Errorf("mail: mailbox store: %w", err)
	}
	sends, err := versionstore.NewRedisStore(client, versionstore.RedisConfig[string, SentRecord]{
		Prefix:       cfg.Prefix + ":send:",
		KeyOf:        func(requestID string) string { return requestID },
		Codec:        versionstore.JSONCodec[SentRecord]{},
		TTL:          cfg.SendTTL,
		MaxAttempts:  cfg.MaxAttempts,
		RetryBackoff: cfg.RetryBackoff,
	})
	if err != nil {
		return RedisStores{}, fmt.Errorf("mail: send ledger: %w", err)
	}
	return RedisStores{Envelopes: envelopes, Mailboxes: mailboxes, Sends: sends}, nil
}

// envelopeClient is the slice of redis.IRedis the envelope store uses.
//
// Taking the narrow interface documents exactly which backend capability this
// depends on, and lets a test supply an evaluating double without stubbing a
// whole client — the same reason kit's versionstore does it.
type envelopeClient interface {
	SetNX(ctx context.Context, key string, value any, expiration time.Duration) (bool, error)
	Get(ctx context.Context, key string) ([]byte, error)
	Pipeline() fredis.IPipeline
}

// redisEnvelopes stores envelopes as one JSON value per key, with the key's
// TTL set from the envelope's own expiry.
//
// Redis rather than a document store, deliberately: every other package in
// this repository already requires Redis and none requires Mongo, so this
// adds no infrastructure. And an envelope's shape fits exactly — written once
// (SETNX), read in bounded pipelines, expiring on its own deadline (the key
// TTL). The implementation this replaces stored mail in Mongo with timestamps
// as int64 milliseconds rather than dates, which meant it could not have a
// TTL index and the collection grew without bound.
//
// A caller that needs envelopes to outlive their expiry — as a durable record
// of what was sent — should implement EnvelopeStore over a document store
// instead. It is three methods, and the contract is what matters, not this
// implementation.
type redisEnvelopes struct {
	client envelopeClient
	prefix string
	now    func() time.Time
	// grace is added to the business remainder to get the key ttl (D-L3).
	grace time.Duration
}

// NewRedisEnvelopes builds an EnvelopeStore over Redis.
//
// now is the clock, and it is a parameter rather than a call to time.Now
// inside Create for two reasons. It has to agree with the Service's clock —
// the key ttl is derived from the same expiry the Service compares against,
// and two components disagreeing about the current time is a mail that reads
// as live and has already been evicted. And a store whose expiry cannot be
// moved by a test has no test for expiry. nil means time.Now.
func NewRedisEnvelopes(client envelopeClient, prefix string, now func() time.Time) (EnvelopeStore, error) {
	if client == nil {
		return nil, fmt.Errorf("mail: redis client is nil")
	}
	if strings.TrimSpace(prefix) == "" {
		return nil, fmt.Errorf("mail: redis key prefix is required")
	}
	if now == nil {
		now = time.Now
	}
	return &redisEnvelopes{client: client, prefix: prefix, now: now, grace: DefaultEnvelopeStorageGrace}, nil
}

func (s *redisEnvelopes) key(id string) string { return s.prefix + ":env:" + id }

// Create is SETNX, so it cannot overwrite. That is the property the contract
// asks for and the reason a retried send is safe without a transaction.
//
// The key's TTL comes from the envelope's own expiry plus the storage grace,
// so an expired mail stops occupying storage without a sweep. The ttl is only
// that reclamation (D-L3): whether a mail is expired is the Service's
// Envelope.Expired against the business clock, and the key outlives that by
// the grace. An envelope whose expiry has already passed is refused rather
// than stored — it would be a mail nobody could ever read.
func (s *redisEnvelopes) Create(ctx context.Context, envelope Envelope) (bool, error) {
	if err := envelope.Validate(); err != nil {
		return false, err
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return false, fmt.Errorf("mail: encode envelope %s: %w", envelope.ID, err)
	}
	remaining := time.Unix(envelope.ExpiresAtUnix, 0).Sub(s.now())
	if remaining <= 0 {
		return false, fmt.Errorf("%w: mail %s expired at %d, before it was stored",
			ErrExpired, envelope.ID, envelope.ExpiresAtUnix)
	}
	created, err := s.client.SetNX(ctx, s.key(envelope.ID), payload, remaining+s.grace)
	if err != nil {
		return false, fmt.Errorf("mail: store envelope %s: %w", envelope.ID, err)
	}
	return created, nil
}

func (s *redisEnvelopes) Get(ctx context.Context, id string) (Envelope, bool, error) {
	if strings.TrimSpace(id) == "" {
		return Envelope{}, false, fmt.Errorf("%w: id is empty", ErrMailInvalid)
	}
	raw, err := s.client.Get(ctx, s.key(id))
	if err != nil {
		if errors.Is(err, fredis.ErrNil) {
			return Envelope{}, false, nil
		}
		return Envelope{}, false, fmt.Errorf("mail: read envelope %s: %w", id, err)
	}
	// Absence is ErrNil above. A key that exists with an empty value is a
	// malformed stored envelope, and it is reported the way GetMany reports
	// it — not folded into "not found", which would make a single read say
	// the mail is gone while the page read fails on the same bytes.
	envelope, err := decodeEnvelope(raw)
	if err != nil {
		return Envelope{}, false, fmt.Errorf("mail: decode envelope %s: %w", id, err)
	}
	return envelope, true, nil
}

// GetMany batches at most MaxPageSize single-key reads in one pipeline Exec.
// Cluster routes these commands per node; it is neither one global network
// round trip nor an atomic snapshot. Unlike multi-key MGET, it supports
// envelopes in different slots without changing their existing keys.
func (s *redisEnvelopes) GetMany(ctx context.Context, ids []string) (map[string]Envelope, error) {
	if len(ids) == 0 {
		return map[string]Envelope{}, nil
	}
	if len(ids) > MaxPageSize {
		// Bounded, and the bound is checked here as well as at the call site.
		// A batch read whose size is whatever the caller asked for is the
		// unbounded read this contract was introduced to prevent.
		return nil, fmt.Errorf("%w: %d ids requested, limit %d", ErrRangeInvalid, len(ids), MaxPageSize)
	}
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("%w: batch contains an empty id", ErrMailInvalid)
		}
	}
	pipe := s.client.Pipeline()
	defer pipe.Discard()
	futures := make([]*fredis.FutureBytes, len(ids))
	for index, id := range ids {
		futures[index] = pipe.Get(ctx, s.key(id))
	}
	if err := pipe.Exec(ctx); err != nil {
		return nil, fmt.Errorf("mail: batch read %d envelopes: %w", len(ids), err)
	}
	out := make(map[string]Envelope, len(ids))
	for index, future := range futures {
		payload, err := future.Result()
		if errors.Is(err, fredis.ErrNil) {
			// Absent: the envelope expired. The caller counts these; see
			// Service.List.
			continue
		}
		// Exec's aggregate result cannot replace checking each command: an
		// earlier missing key may mask a later command error in that result.
		if err != nil {
			return nil, fmt.Errorf("mail: read envelope %s: %w", ids[index], err)
		}
		envelope, err := decodeEnvelope(payload)
		if err != nil {
			// Reported rather than skipped. A malformed stored value that
			// reads as "absent" is how a page comes back short while a count
			// says otherwise.
			return nil, fmt.Errorf("mail: decode envelope %s: %w", ids[index], err)
		}
		out[ids[index]] = envelope
	}
	return out, nil
}

func decodeEnvelope(raw []byte) (Envelope, error) {
	var envelope Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return Envelope{}, err
	}
	if strings.TrimSpace(envelope.ID) == "" {
		return Envelope{}, fmt.Errorf("%w: stored envelope has no id", ErrMailInvalid)
	}
	return envelope, nil
}

var _ EnvelopeStore = (*redisEnvelopes)(nil)
