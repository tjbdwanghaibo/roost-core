package entitysync

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// U-0206 · C8 · RR-20260915-03:配置了持久化水位门槛后,**所有**暴露实体内容的出口都要过同一道门。
// 旧实现只有 FlushSubject 查 `LastCommitLSN > durableWatermark` 就推迟;Subscribe(含 profile 切换)
// 捕获快照直接准入,Prepare → Distribute 也直接准入——最新流水提交尚未落盘的状态经这三个入口送出,
// 之后进程故障,接收端见过的状态无法从持久日志恢复。承诺:门槛跟着**被捕获的内容**走——快照与
// 预备批次在实体锁内一并记下捕获时刻的 CommitLSN(与 nest 在锁内盖 LSN 同一把锁,没有检查-捕获窗口),
// 准入时任一条高于水位即整体推迟:Subscribe 保留原有效订阅并返回可重试错误,Distribute 整批 abort、
// 状态保持 dirty,水位追上后重试成功。

func gateSetup(t *testing.T) (*SubscriptionCoordinator, *recordingEnvelopeSink, *entity.SubjectSyncState, SubscriberRef, entity.SyncProfile) {
	t.Helper()
	sink := &recordingEnvelopeSink{}
	c := NewSubscriptionCoordinator(sink)
	t.Cleanup(c.Close)
	packs := 0
	s := newSubscriptionTestState(t, 201, &packs)
	t.Cleanup(func() { s.Close() })
	return c, sink, s, SubscriberRef{Kind: SubscriberKindPlayer, ID: 1}, entity.SyncProfile{Key: "a"}
}

func TestDurabilityGatePromiseCoversEveryDeliveryEntryPoint(t *testing.T) {
	for _, mode := range []string{"subscribe", "profile_change", "direct_distribute", "flush_blocked", "durable_subscribe"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			c, sink, s, sub, profile := gateSetup(t)
			if mode != "subscribe" && mode != "durable_subscribe" {
				if _, e := c.Subscribe(ctx, sub, s, profile); e != nil {
					t.Fatal(e)
				}
			}
			before := len(sink.snapshot())
			watermark := uint64(0)
			if mode == "durable_subscribe" {
				watermark = 7
			}
			c.SetDurableWatermark(func() uint64 { return watermark })
			s.MarkDirty(1)
			s.SetLastCommitLSN(7)
			switch mode {
			case "subscribe", "durable_subscribe":
				_, _ = c.Subscribe(ctx, sub, s, profile)
			case "profile_change":
				_, _ = c.Subscribe(ctx, sub, s, entity.SyncProfile{Key: "b"})
			case "direct_distribute":
				p, e := s.Prepare([]entity.SyncProfile{profile})
				if e != nil {
					t.Fatal(e)
				}
				_ = c.Distribute(ctx, p)
			case "flush_blocked":
				if e := c.FlushSubject(ctx, s); e != nil {
					t.Fatal(e)
				}
			}
			emitted := len(sink.snapshot()) - before
			if mode == "durable_subscribe" {
				if emitted != 1 {
					t.Fatalf("durable snapshot not delivered: %d", emitted)
				}
			} else if emitted != 0 {
				t.Fatalf("non-durable LSN=7 watermark=0 escaped via %s: batches=%d", mode, emitted)
			}
		})
	}
}

// 推迟不是丢弃:错误可辨认、可重试;profile 切换失败时原订阅保留;批量分发 abort 后状态仍 dirty;
// 水位追上之后同样的调用成功。
func TestDurabilityGatePromiseDeferralIsRetryable(t *testing.T) {
	ctx := context.Background()
	c, sink, s, sub, profile := gateSetup(t)
	if _, err := c.Subscribe(ctx, sub, s, profile); err != nil {
		t.Fatal(err)
	}
	watermark := uint64(0)
	c.SetDurableWatermark(func() uint64 { return watermark })
	s.MarkDirty(1)
	s.SetLastCommitLSN(7)

	// profile 切换被推迟:返回可重试错误,原订阅原样保留。
	if _, err := c.Subscribe(ctx, sub, s, entity.SyncProfile{Key: "b"}); !errors.Is(err, ErrDurabilityDeferred) {
		t.Fatalf("deferred profile change: err=%v want ErrDurabilityDeferred", err)
	}
	if got, ok := c.Get(sub, s.SubjectID()); !ok || got.State != SubscriptionActive || got.Profile.Key != "a" {
		t.Fatalf("deferred profile change disturbed the existing subscription: %+v ok=%v", got, ok)
	}
	// 新订阅者被推迟:不留下 pending 订阅。
	other := SubscriberRef{Kind: SubscriberKindPlayer, ID: 2}
	if _, err := c.Subscribe(ctx, other, s, profile); !errors.Is(err, ErrDurabilityDeferred) {
		t.Fatalf("deferred subscribe: err=%v", err)
	}
	if _, ok := c.Get(other, s.SubjectID()); ok {
		t.Fatal("deferred subscribe left a subscription behind")
	}
	// 批量分发被推迟:整批 abort,状态保持 dirty,可以再 Prepare。
	p, err := s.Prepare([]entity.SyncProfile{profile})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Distribute(ctx, p); !errors.Is(err, ErrDurabilityDeferred) {
		t.Fatalf("deferred distribute: err=%v", err)
	}
	if !s.PendingDirty() || s.Version() != 0 {
		t.Fatalf("deferred distribute consumed the dirty state: dirty=%v version=%d", s.PendingDirty(), s.Version())
	}
	if len(sink.snapshot()) != 1 {
		t.Fatalf("something escaped while deferred: batches=%d", len(sink.snapshot()))
	}

	// 水位追上:三件事都成功。
	watermark = 7
	if _, err := c.Subscribe(ctx, other, s, profile); err != nil {
		t.Fatalf("subscribe after the watermark caught up: %v", err)
	}
	if _, err := c.Subscribe(ctx, sub, s, entity.SyncProfile{Key: "b"}); err != nil {
		t.Fatalf("profile change after the watermark caught up: %v", err)
	}
	if err := c.FlushSubject(ctx, s); err != nil {
		t.Fatal(err)
	}
	if s.PendingDirty() || s.Version() != 1 {
		t.Fatalf("flush after the watermark caught up: dirty=%v version=%d", s.PendingDirty(), s.Version())
	}
}
