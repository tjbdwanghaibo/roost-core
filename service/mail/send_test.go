// 发送：按 RequestID 幂等与恢复、账本竞争报冲突、瞬时失败可重试、过期邮箱腾出空间、被拒投递不留痕迹。

package mail

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

// vanishingSends models the ledger race the guard exists for: our claim loses
// to a racer, and by the time we read the racer's row back it is gone.
type vanishingSends struct {
	versionstore.Store[string, SentRecord]
}

func (vanishingSends) Create(context.Context, string, SentRecord) (versionstore.Versioned[SentRecord], bool, error) {
	return versionstore.Versioned[SentRecord]{}, false, nil
}
func (vanishingSends) Get(context.Context, string) (versionstore.Versioned[SentRecord], bool, error) {
	return versionstore.Versioned[SentRecord]{}, false, nil
}

// danglingSends loses the claim to a racer whose ledger row names an envelope
// that does not exist.
type danglingSends struct {
	versionstore.Store[string, SentRecord]
}

func (danglingSends) Create(context.Context, string, SentRecord) (versionstore.Versioned[SentRecord], bool, error) {
	return versionstore.Versioned[SentRecord]{}, false, nil
}
func (danglingSends) Get(_ context.Context, requestID string) (versionstore.Versioned[SentRecord], bool, error) {
	return versionstore.Versioned[SentRecord]{Value: SentRecord{RequestID: requestID, MailID: "mail-missing"}, Version: 1}, true, nil
}

// collidingEnvelopes reports every mail id as already taken.
type collidingEnvelopes struct{ *fakeEnvelopes }

func (c collidingEnvelopes) Create(context.Context, Envelope) (bool, error) { return false, nil }

// The three "the world moved under us" branches of Send are each a conflict the caller must see, never a silent success —
// and none of them may leave an envelope behind that the ledger does not name.
func TestSendReportsEachLedgerRaceAsConflict(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"claim lost and racer row vanished", func(cfg *Config) { cfg.Sends = vanishingSends{Store: cfg.Sends} }, `send "send-1" vanished during create`},
		{"claim lost and racer names a missing mail", func(cfg *Config) { cfg.Sends = danglingSends{Store: cfg.Sends} }, `send "send-1" names missing mail mail-missing`},
		{"mail id already in use", func(cfg *Config) { cfg.Envelopes = collidingEnvelopes{fakeEnvelopes: cfg.Envelopes.(*fakeEnvelopes)} }, "mail id mail-1 is already in use"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.mutate)
			_, err := h.service.Send(ctx, directTo(7))
			if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Send = %v; want ErrConflict containing %q", err, tc.want)
			}
			h.envelopes.mu.Lock()
			stored := len(h.envelopes.items)
			h.envelopes.mu.Unlock()
			if stored != 0 {
				t.Fatalf("a refused send left %d envelope(s) behind", stored)
			}
			if got := h.metrics.Count("accepted:send"); got != 0 {
				t.Fatalf("a refused send was counted as accepted (%d)", got)
			}
		})
	}
}

// 同 RequestID 的恢复回读信封后确认“内容相同”：nil 切片与空切片一视同仁（逐字段比较，切片用 slices.Equal / bytes.Equal），
// ID、期限、正文、受众等真的不同时仍然拒绝。
// 历史：RR-20261001-02。

// nilToEmptyEnvelopes 模拟把缺失的切片字段解码成空切片的存储。
type nilToEmptyEnvelopes struct{ EnvelopeStore }

func (s nilToEmptyEnvelopes) Get(ctx context.Context, id string) (Envelope, bool, error) {
	e, ok, err := s.EnvelopeStore.Get(ctx, id)
	if e.Attachment == nil {
		e.Attachment = []byte{}
	}
	if e.Recipients == nil {
		e.Recipients = []int64{}
	}
	return e, ok, err
}

func TestSameRequestRecoveryToleratesEmptyVsNilSlices(t *testing.T) {
	for name, req := range map[string]SendRequest{
		"direct_no_attachment": directTo(1),
		"broadcast_no_recipients": {
			Audience: AudienceBroadcast, Subject: "notice", Body: "maintenance",
			ExpiresInSeconds: 604800, RequestID: "send-broadcast",
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, func(c *Config) {
				c.Envelopes = &reviewEnvelopeOnceFailure{EnvelopeStore: nilToEmptyEnvelopes{c.Envelopes}}
				c.Broadcast = DelivererFunc(func(context.Context, Envelope) error { return nil })
			})
			ctx := context.Background()
			if _, err := h.service.Send(ctx, req); err == nil {
				t.Fatal("fixture must fail the first envelope write")
			}
			recovered, err := h.service.Send(ctx, req)
			if err != nil {
				t.Fatalf("semantically identical envelope refused on retry: %v", err)
			}
			again, err := h.service.Send(ctx, req)
			if err != nil || again.ID != recovered.ID {
				t.Fatalf("third attempt changed outcome: id %s -> %s err=%v", recovered.ID, again.ID, err)
			}
		})
	}
}

// substitutedEnvelopes 回读时换掉信封的某个字段：恢复路径仍然拒绝——不能在保留的 id 下投递被替换的信封，不能被切片宽容放宽。
type substitutedEnvelopes struct {
	EnvelopeStore
	mutate func(*Envelope)
}

func (s substitutedEnvelopes) Get(ctx context.Context, id string) (Envelope, bool, error) {
	e, ok, err := s.EnvelopeStore.Get(ctx, id)
	if ok {
		s.mutate(&e)
	}
	return e, ok, err
}

func TestSameRequestRecoveryStillRejectsSubstitutedEnvelope(t *testing.T) {
	for name, mutate := range map[string]func(*Envelope){
		"body":       func(e *Envelope) { e.Body += " (edited)" },
		"expiry":     func(e *Envelope) { e.ExpiresAtUnix++ },
		"attachment": func(e *Envelope) { e.Attachment = []byte("forged") },
		"recipients": func(e *Envelope) { e.Recipients = append(e.Recipients, 99) },
		"scope":      func(e *Envelope) { e.Scope = "other-world" },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, func(c *Config) {
				c.Envelopes = &reviewEnvelopeOnceFailure{EnvelopeStore: substitutedEnvelopes{c.Envelopes, mutate}}
			})
			ctx := context.Background()
			req := directTo(1)
			if _, err := h.service.Send(ctx, req); err == nil {
				t.Fatal("fixture must fail the first envelope write")
			}
			_, err := h.service.Send(ctx, req)
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("substituted envelope accepted on recovery: err=%v", err)
			}
		})
	}
}

func TestSameSendIntentFieldwise(t *testing.T) {
	// 逐字段比较不会自动覆盖新加的字段：Envelope 加字段时这里会红，提醒同时
	// 扩 sameSendIntent 和下面的差异表。
	if n := reflect.TypeOf(Envelope{}).NumField(); n != 10 {
		t.Fatalf("Envelope has %d fields; sameSendIntent compares 10 — extend both", n)
	}
	base := Envelope{
		ID: "mail-1", Audience: AudienceDirect, Scope: "s", Recipients: []int64{1, 2},
		Subject: "a", Body: "b", Attachment: []byte("x"), SendRequestID: "r",
		CreatedAtUnix: 10, ExpiresAtUnix: 20,
	}
	if !sameSendIntent(base, base.clone()) {
		t.Fatal("clone judged different")
	}
	nilSlices := base
	nilSlices.Recipients, nilSlices.Attachment = nil, nil
	emptySlices := base
	emptySlices.Recipients, emptySlices.Attachment = []int64{}, []byte{}
	if !sameSendIntent(nilSlices, emptySlices) || !sameSendIntent(emptySlices, nilSlices) {
		t.Fatal("nil and empty slices judged different")
	}
	for name, edit := range map[string]func(*Envelope){
		"id":          func(e *Envelope) { e.ID = "mail-2" },
		"audience":    func(e *Envelope) { e.Audience = AudienceBroadcast },
		"scope":       func(e *Envelope) { e.Scope = "t" },
		"recipients":  func(e *Envelope) { e.Recipients = []int64{1, 3} },
		"subject":     func(e *Envelope) { e.Subject = "z" },
		"body":        func(e *Envelope) { e.Body = "z" },
		"attachment":  func(e *Envelope) { e.Attachment = []byte("y") },
		"request_id":  func(e *Envelope) { e.SendRequestID = "q" },
		"created_at":  func(e *Envelope) { e.CreatedAtUnix++ },
		"expires_at":  func(e *Envelope) { e.ExpiresAtUnix++ },
		"drop_recips": func(e *Envelope) { e.Recipients = nil },
		"drop_attach": func(e *Envelope) { e.Attachment = nil },
	} {
		other := base.clone()
		edit(&other)
		if sameSendIntent(base, other) {
			t.Fatalf("%s: differing envelope judged same", name)
		}
	}
}

func TestExpiredMailboxMakesRoom(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for i := 0; i < MaxMailboxEntries; i++ {
		req := directTo(1)
		req.RequestID = fmt.Sprintf("expired-%d", i)
		req.ExpiresInSeconds = 1
		if _, err := h.service.Send(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	h.clock.advance(2 * time.Second)
	page, err := h.service.List(ctx, 1, "", MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("visible=%d unread=%d", len(page.Items), page.Unread)
	req := directTo(1)
	req.RequestID = "fresh"
	if _, err := h.service.Send(ctx, req); err != nil {
		t.Fatalf("expired invisible mails still block delivery: %v", err)
	}
}

type reviewEnvelopeOnceFailure struct {
	EnvelopeStore
	failed bool
}

func (s *reviewEnvelopeOnceFailure) Create(ctx context.Context, e Envelope) (bool, error) {
	if !s.failed {
		s.failed = true
		return false, fmt.Errorf("injected transient failure before envelope write")
	}
	return s.EnvelopeStore.Create(ctx, e)
}
func TestTransientEnvelopeFailureCanRetrySameRequest(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Envelopes = &reviewEnvelopeOnceFailure{EnvelopeStore: c.Envelopes} })
	ctx := context.Background()
	req := directTo(1)
	first, firstErr := h.service.Send(ctx, req)
	second, err := h.service.Send(ctx, req)
	if err != nil {
		t.Fatalf("store recovered but same send cannot recover: %v", err)
	}
	if firstErr == nil && first.ID != second.ID {
		t.Fatalf("successful send replay changed mail id: %s -> %s", first.ID, second.ID)
	}
}

// 被拒绝的投递不留下任何痕迹：Update 回调先 clone 再修改，容量拒绝时条目、墓碑、未读数与版本都保持原样。
// 历史：RR-20260911-05。
func TestRefusedDeliveryLeavesTheMailboxUntouched(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	mailID := settleOneClaim(t, h)

	// Drive the settled claims to the bound so the next settling delivery is
	// refused with ErrClaimHistoryFull.
	refused := false
	for i := 0; i < MaxSettledClaims+5 && !refused; i++ {
		if err := deleteOneUnread(h, mailID); err != nil {
			t.Fatalf("free a slot before churn %d: %v", i, err)
		}
		h.clock.advance(time.Second)
		req := withAttachment(directTo(1), "reward")
		req.RequestID = fmt.Sprintf("atomic-%d", i)
		other, err := h.service.Send(ctx, req)
		if errors.Is(err, ErrClaimHistoryFull) {
			refused = true
			break
		}
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		claim, err := h.service.ReserveClaim(ctx, 1, other.ID, "")
		if err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
		if _, err := h.service.CommitClaim(ctx, 1, other.ID, claim.Token); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
		if err := evictUntilSettled(h, other.ID); errors.Is(err, ErrClaimHistoryFull) {
			refused = true
		} else if err != nil {
			t.Fatalf("settle %d: %v", i, err)
		}
	}
	if !refused {
		t.Fatal("control: never reached the claim-history bound")
	}

	before, _, err := h.mailboxes.Get(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	// One more delivery that would have to evict a claimed entry and settle a
	// new claim: it must be refused, and refusing must change nothing.
	h.clock.advance(time.Second)
	err = deliverMailboxFixture(h, ctx, 1, "one-too-many", 0)
	if !errors.Is(err, ErrClaimHistoryFull) && !errors.Is(err, ErrMailboxFull) {
		t.Fatalf("control: the extra delivery was not refused: %v", err)
	}
	after, _, err := h.mailboxes.Get(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, inserted := after.Value.Entries["one-too-many"]; inserted {
		t.Error("the refused delivery's entry is in the stored mailbox")
	}
	if len(after.Value.Entries) != len(before.Value.Entries) {
		t.Errorf("entries %d -> %d across a refused delivery", len(before.Value.Entries), len(after.Value.Entries))
	}
	if len(after.Value.SettledClaims) != len(before.Value.SettledClaims) {
		t.Errorf("settled claims %d -> %d across a refused delivery", len(before.Value.SettledClaims), len(after.Value.SettledClaims))
	}
	if after.Version != before.Version {
		t.Errorf("version %d -> %d across a refused delivery", before.Version, after.Version)
	}
	unread := int32(0)
	for _, entry := range after.Value.Entries {
		if entry.Status == StatusUnread {
			unread++
		}
	}
	if unread != after.Value.Unread {
		t.Errorf("Unread field %d disagrees with %d actual unread entries", after.Value.Unread, unread)
	}
}
