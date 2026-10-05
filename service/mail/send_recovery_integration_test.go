//go:build integration

package mail

// N06 S6 复核（2026-10-05）：RR-20260929-16 / RR-20261001-02 的同 RequestID 恢复在
// 真实 Redis 信封存储上的组合——Create 未落地 / 已落地但回复丢失两种未知结果，
// 定向与广播两种受众（广播无收件人、无附件，正是 nil 与空切片的形状）。单测里的
// nilToEmptyEnvelopes 只模拟了一种解码；这里用真实 JSON 往返与 SETNX 回读。
//
//	REDIS_ADDR=<隔离 Redis> go test -tags integration ./service/mail/ -run IntegrationSendRecovery

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// landedButLostEnvelopes writes the first envelope and then loses the reply.
type landedButLostEnvelopes struct {
	EnvelopeStore
	lost atomic.Bool
}

func (s *landedButLostEnvelopes) Create(ctx context.Context, e Envelope) (bool, error) {
	created, err := s.EnvelopeStore.Create(ctx, e)
	if err == nil && created && s.lost.CompareAndSwap(false, true) {
		return false, fmt.Errorf("injected: envelope stored, reply lost")
	}
	return created, err
}

func TestIntegrationSendRecoveryOnRealEnvelopes(t *testing.T) {
	requests := map[string]SendRequest{
		"direct": {
			Audience: AudienceDirect, Recipients: []int64{7},
			Subject: "reward", Body: "well played", ExpiresInSeconds: 3600, RequestID: "revn06-direct",
		},
		"broadcast": {
			Audience: AudienceBroadcast, Subject: "notice", Body: "maintenance",
			ExpiresInSeconds: 3600, RequestID: "revn06-broadcast",
		},
	}
	failures := map[string]func(EnvelopeStore) EnvelopeStore{
		"write_never_landed": func(s EnvelopeStore) EnvelopeStore { return &reviewEnvelopeOnceFailure{EnvelopeStore: s} },
		"landed_reply_lost":  func(s EnvelopeStore) EnvelopeStore { return &landedButLostEnvelopes{EnvelopeStore: s} },
	}
	for reqName, req := range requests {
		for failName, wrap := range failures {
			t.Run(reqName+"/"+failName, func(t *testing.T) {
				client := integrationClient(t)
				prefix := fmt.Sprintf("revn06:mail:%d", time.Now().UnixNano())
				t.Cleanup(func() { deleteKeysUnder(t, prefix) })
				stores, err := NewRedisStores(client, RedisConfig{Prefix: prefix, SendTTL: time.Hour})
				if err != nil {
					t.Fatal(err)
				}
				var broadcasts atomic.Int32
				service, err := New(Config{
					Envelopes: wrap(stores.Envelopes), Mailboxes: stores.Mailboxes, Sends: stores.Sends,
					Broadcast: DelivererFunc(func(context.Context, Envelope) error { broadcasts.Add(1); return nil }),
				})
				if err != nil {
					t.Fatal(err)
				}
				ctx := context.Background()
				if _, err := service.Send(ctx, req); err == nil {
					t.Fatal("fixture must fail the first envelope write")
				}
				recovered, err := service.Send(ctx, req)
				if err != nil {
					t.Fatalf("same-request recovery on real Redis envelopes failed: %v", err)
				}
				again, err := service.Send(ctx, req)
				if err != nil || again.ID != recovered.ID {
					t.Fatalf("third attempt changed the outcome: %s -> %s err=%v", recovered.ID, again.ID, err)
				}
				stored, found, err := stores.Envelopes.Get(ctx, recovered.ID)
				if err != nil || !found || !sameSendIntent(stored, recovered) {
					t.Fatalf("stored envelope %+v found=%v err=%v, recovered %+v", stored, found, err, recovered)
				}
				switch req.Audience {
				case AudienceDirect:
					page, err := service.List(ctx, 7, "", 10)
					if err != nil || len(page.Items) != 1 || page.Items[0].Envelope.ID != recovered.ID {
						t.Fatalf("mailbox after recovery: %+v err=%v", page, err)
					}
				case AudienceBroadcast:
					if got := broadcasts.Load(); got != 1 {
						t.Fatalf("broadcast delivered %d times, want exactly once", got)
					}
				}
			})
		}
	}
}
