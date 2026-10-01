package mail

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// RR-20261001-02：同 RequestID 的恢复（RR-20260929-16）回读信封后要确认“内容相同”，
// 这个判断对 nil 切片与空切片必须一视同仁。旧实现 sameSendIntent 用
// reflect.DeepEqual，而 SentRecord.Intent 经 clone / JSON 往返后 Recipients、
// Attachment 为 nil；自定义 EnvelopeStore 的 Get 把缺失字段还原成 []byte{} /
// []int64{} 时，语义相同的信封被判为“不同”，同一条发送每次重试都停在
// ErrConflict: reserved envelope differs or is missing，永远完不成。
// 修法是逐字段比较，切片用 slices.Equal / bytes.Equal；ID、期限、正文、受众等
// 真的不同时仍然拒绝。

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

// substitutedEnvelopes 回读时换掉信封的某个字段：恢复路径必须仍然拒绝，这是
// RR-16 “不能在保留的 id 下投递被替换的信封”的承诺，不能被切片宽容放宽。
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
