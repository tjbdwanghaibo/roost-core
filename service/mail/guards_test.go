// 入参与构造守卫：存储前提、标识符、信封字段校验、领取租约与投递前置条件。

package mail

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

// Redis 存储的三条构造前提（客户端、键前缀、发送账本 TTL）；Send 的过期秒数为正；Deliver / List / MarkRead / Delete 的玩家 id 为正、
// 邮件 id 不能为空——都在触达任何存储之前以对应哨兵拒绝。

// idleRedis is an IRedis that must never be reached: the constructor's own
// checks come before any call.
type idleRedis struct{ fredis.IRedis }

func TestNewRedisStoresRefusesEachMissingPrerequisite(t *testing.T) {
	if _, err := NewRedisStores(nil, RedisConfig{Prefix: "mail", SendTTL: time.Minute}); err == nil || !strings.Contains(err.Error(), "redis client is nil") {
		t.Fatalf("NewRedisStores(nil client) = %v", err)
	}
	if _, err := NewRedisStores(idleRedis{}, RedisConfig{Prefix: "  ", SendTTL: time.Minute}); err == nil || !strings.Contains(err.Error(), "key prefix is required") {
		t.Fatalf("NewRedisStores with a blank prefix = %v", err)
	}
	for _, ttl := range []time.Duration{0, -time.Second} {
		if _, err := NewRedisStores(idleRedis{}, RedisConfig{Prefix: "mail", SendTTL: ttl}); err == nil || !strings.Contains(err.Error(), "send ttl must be positive") {
			t.Fatalf("NewRedisStores with send ttl %v = %v", ttl, err)
		}
	}
}

func TestServiceEntryPointsRefuseInvalidIdentifiers(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	for _, seconds := range []int64{0, -1} {
		req := directTo(7)
		req.ExpiresInSeconds = seconds
		if _, err := h.service.Send(ctx, req); !errors.Is(err, ErrMailInvalid) || !strings.Contains(err.Error(), "expires_in_seconds must be positive") {
			t.Fatalf("Send with expires_in_seconds=%d = %v", seconds, err)
		}
	}
	if err := h.service.Deliver(ctx, 0, "m-1", 0); !errors.Is(err, ErrRequestInvalid) || !strings.Contains(err.Error(), "player id must be positive") {
		t.Fatalf("Deliver to player 0 = %v", err)
	}
	if err := h.service.Deliver(ctx, 7, "  ", 0); !errors.Is(err, ErrMailInvalid) || !strings.Contains(err.Error(), "mail id is empty") {
		t.Fatalf("Deliver with a blank mail id = %v", err)
	}
	if _, err := h.service.List(ctx, 0, "", 10); !errors.Is(err, ErrRequestInvalid) || !strings.Contains(err.Error(), "player id must be positive") {
		t.Fatalf("List for player 0 = %v", err)
	}
	if _, err := h.service.MarkRead(ctx, -1, "m-1"); !errors.Is(err, ErrRequestInvalid) || !strings.Contains(err.Error(), "player id must be positive") {
		t.Fatalf("MarkRead for player -1 = %v", err)
	}
	if _, err := h.service.MarkRead(ctx, 7, " "); !errors.Is(err, ErrMailInvalid) || !strings.Contains(err.Error(), "mail id is empty") {
		t.Fatalf("MarkRead with a blank mail id = %v", err)
	}
	if _, err := h.service.Delete(ctx, 0, "m-1"); !errors.Is(err, ErrRequestInvalid) {
		t.Fatalf("Delete for player 0 = %v", err)
	}
	if _, err := h.service.Delete(ctx, 7, ""); !errors.Is(err, ErrMailInvalid) {
		t.Fatalf("Delete with an empty mail id = %v", err)
	}
	if _, found, err := h.service.Mailbox(ctx, 7); err != nil || found {
		t.Fatalf("refused operations created a mailbox: found=%v err=%v", found, err)
	}
}

// The Redis constructors refuse a nil client and a blank prefix, and the envelope store refuses empty ids on both read paths
// and a stored envelope that carries no id.
func TestRedisStoresRefuseInvalidConfigAndEmptyIDs(t *testing.T) {
	ctx := context.Background()
	if _, err := NewRedisStores(nil, RedisConfig{Prefix: "mail", SendTTL: time.Hour}); err == nil || !strings.Contains(err.Error(), "redis client is nil") {
		t.Fatalf("NewRedisStores(nil) = %v", err)
	}
	if _, err := NewRedisEnvelopes(nil, "mail", nil); err == nil || !strings.Contains(err.Error(), "redis client is nil") {
		t.Fatalf("NewRedisEnvelopes(nil) = %v", err)
	}
	if _, err := NewRedisEnvelopes(newFakeRedisEnvelopes(), "  ", nil); err == nil || !strings.Contains(err.Error(), "key prefix is required") {
		t.Fatalf("NewRedisEnvelopes with a blank prefix = %v", err)
	}

	store, fake := newRedisEnvelopeStore(t)
	if _, _, err := store.Get(ctx, " "); !errors.Is(err, ErrMailInvalid) || !strings.Contains(err.Error(), "id is empty") {
		t.Fatalf("Get with an empty id = %v", err)
	}
	if _, err := store.GetMany(ctx, []string{"m-1", ""}); !errors.Is(err, ErrMailInvalid) || !strings.Contains(err.Error(), "batch contains an empty id") {
		t.Fatalf("GetMany with an empty id = %v", err)
	}
	fake.mu.Lock()
	fake.values["mailtest:env:blank"] = []byte(`{"subject":"orphan"}`)
	fake.mu.Unlock()
	if _, _, err := store.Get(ctx, "blank"); !errors.Is(err, ErrMailInvalid) || !strings.Contains(err.Error(), "stored envelope has no id") {
		t.Fatalf("Get of a stored envelope without an id = %v", err)
	}
}

func validDirect() Envelope {
	return Envelope{ID: "m-1", Audience: AudienceDirect, Recipients: []int64{7}, Subject: "hi", Body: "body", CreatedAtUnix: 100, ExpiresAtUnix: 200}
}

// Envelope.Validate is the shape gate for everything the mail service stores
// and fans out. Every rule is pinned by its sentinel and message; a dropped
// rule turns exactly one case red.
func TestEnvelopeValidateRefusesEachBrokenField(t *testing.T) {
	if err := validDirect().Validate(); err != nil {
		t.Fatalf("baseline rejected: %v", err)
	}
	broadcast := Envelope{ID: "m-2", Audience: AudienceBroadcast, Subject: "all", ExpiresAtUnix: 200}
	if err := broadcast.Validate(); err != nil {
		t.Fatalf("broadcast baseline rejected: %v", err)
	}
	cases := []struct {
		name     string
		mutate   func(*Envelope)
		sentinel error
		text     string
	}{
		{"blank id", func(e *Envelope) { e.ID = " " }, ErrMailInvalid, "id is empty"},
		{"direct without recipients", func(e *Envelope) { e.Recipients = nil }, ErrAudienceInvalid, "a direct mail has no recipients"},
		{"direct with too many recipients", func(e *Envelope) {
			e.Recipients = make([]int64, MaxRecipients+1)
			for i := range e.Recipients {
				e.Recipients[i] = int64(i + 1)
			}
		}, ErrAudienceInvalid, "recipients, limit"},
		{"recipient that is not a player", func(e *Envelope) { e.Recipients = []int64{7, 0} }, ErrAudienceInvalid, "recipient 0 is not a player"},
		{"broadcast carrying recipients", func(e *Envelope) { e.Audience = AudienceBroadcast }, ErrAudienceInvalid, "a broadcast mail must not carry a recipient list"},
		{"unknown audience", func(e *Envelope) { e.Audience = Audience("carrier-pigeon") }, ErrAudienceInvalid, `unknown audience "carrier-pigeon"`},
		{"blank subject", func(e *Envelope) { e.Subject = "  " }, ErrBodyInvalid, "subject is empty"},
		{"subject too long", func(e *Envelope) { e.Subject = strings.Repeat("s", MaxSubjectBytes+1) }, ErrBodyInvalid, "subject is"},
		{"body too long", func(e *Envelope) { e.Body = strings.Repeat("b", MaxBodyBytes+1) }, ErrBodyInvalid, "body is"},
		{"attachment too long", func(e *Envelope) { e.Attachment = make([]byte, MaxAttachmentBytes+1) }, ErrBodyInvalid, "attachment is"},
		{"never expires", func(e *Envelope) { e.ExpiresAtUnix = 0 }, ErrMailInvalid, "a mail must expire"},
		{"expires before creation", func(e *Envelope) { e.ExpiresAtUnix = e.CreatedAtUnix }, ErrMailInvalid, "is not after creation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := validDirect()
			tc.mutate(&e)
			err := e.Validate()
			if !errors.Is(err, tc.sentinel) || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("Validate = %v, want %v containing %q", err, tc.sentinel, tc.text)
			}
		})
	}
}

func TestClaimLeaseRejectsSubsecond(t *testing.T) {
	h := newHarness(t)
	cfg := h.service.cfg
	cfg.ClaimLease = time.Millisecond
	if _, err := New(cfg); err == nil {
		t.Fatal("subsecond claim lease accepted despite Unix-second deadline")
	}
}
func TestDeliverRefusesMissingEnvelope(t *testing.T) {
	h := newHarness(t)
	if err := h.service.Deliver(context.Background(), 1, "missing", h.clock.Now().Unix()); err == nil {
		t.Fatal("missing envelope created immortal unread entry")
	}
	if _, found, err := h.mailboxes.Get(context.Background(), 1); err != nil || found {
		t.Fatalf("missing envelope changed mailbox: found=%v err=%v", found, err)
	}
}
