package platform

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// U-0234 · C4 · RR-20260917-04：Server 的后台重试循环没有候选入口。
// 旧行为：`retryOrders()` 固定返回 nil，方法未导出、外包覆盖不了，Server / Mod / Config
// 也没有任何注入点——注释写着"由部署提供订单列表"，但没有实现。于是一次可恢复的发货失败
// （渠道不再回调）会一直挂着：订单有剩余次数、退避已到期、发货服务已恢复，后台却什么都不做。
// 承诺：部署能提供一个有界、可报错的待发货来源，循环按它重试；没提供时要明说，而不是静默空转。

// stubPending is a deployment's pending-order index: bounded by the limit the
// loop asks for, and able to fail.
type stubPending struct {
	mu     sync.Mutex
	ids    []string
	err    error
	calls  atomic.Int64
	limits []int
}

func (p *stubPending) PendingOrders(_ context.Context, limit int) ([]string, error) {
	p.calls.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.limits = append(p.limits, limit)
	if p.err != nil {
		return nil, p.err
	}
	if limit > 0 && len(p.ids) > limit {
		return append([]string(nil), p.ids[:limit]...), nil
	}
	return append([]string(nil), p.ids...), nil
}

func TestPendingOrderSourceFeedsTheRetryLoop(t *testing.T) {
	pending := &stubPending{ids: []string{"order-1", "order-2"}}
	service := newPendingTestService(t, pending)

	ids, err := service.pendingOrderIDs(context.Background())
	if err != nil {
		t.Fatalf("pending orders: %v", err)
	}
	if len(ids) != 2 || ids[0] != "order-1" {
		t.Fatalf("the loop got %v from the configured source", ids)
	}
	if got := pending.limits[0]; got <= 0 {
		t.Errorf("the source was asked for an unbounded page (limit %d)", got)
	}
}

// No source configured is a deployment decision, and it has to be visible:
// the loop asks for nothing and the service says so once, rather than
// pretending to retry.
func TestWithoutAPendingSourceTheLoopIsExplicitlyOff(t *testing.T) {
	service := newPendingTestService(t, nil)
	ids, err := service.pendingOrderIDs(context.Background())
	if err != nil {
		t.Fatalf("no source should not be an error: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("a service with no source produced %v", ids)
	}
	if service.BackgroundRetryEnabled() {
		t.Error("a service with no source claims background retry is on")
	}
}

// A failing index is the deployment's problem to see, not a reason to stop
// retrying every other order forever.
func TestPendingSourceErrorIsReportedNotFatal(t *testing.T) {
	pending := &stubPending{err: errors.New("index unavailable")}
	service := newPendingTestService(t, pending)
	if _, err := service.pendingOrderIDs(context.Background()); err == nil {
		t.Fatal("a failing source was reported as success")
	}
	// The next tick asks again rather than giving up.
	pending.mu.Lock()
	pending.err = nil
	pending.ids = []string{"order-3"}
	pending.mu.Unlock()
	ids, err := service.pendingOrderIDs(context.Background())
	if err != nil || len(ids) != 1 {
		t.Fatalf("after the source recovered: ids=%v err=%v", ids, err)
	}
}

// The scenario the review measured: a paid order whose first delivery failed,
// no further channel callback, attempts left and the backoff elapsed. With a
// pending index the background path recovers it; with the old fixed-nil
// source it hung forever.
func TestBackgroundPathRecoversAPaidOrderWithoutAnotherCallback(t *testing.T) {
	pending := &stubPending{ids: []string{"order-1"}}
	h := newHarness(t, func(c *Config) { c.Pending = pending })
	ctx := context.Background()
	h.deliverer.failFor["order-1"] = 1
	raw, signature := callback(t, "order-1", 1001, 499)
	if _, err := h.service.HandleCallback(ctx, raw, signature); !errors.Is(err, ErrDeliveryFailed) {
		t.Fatalf("the first delivery should have failed: %v", err)
	}
	// The channel never calls back again; the backoff elapses.
	h.clock.advance(time.Minute)

	ids, err := h.service.pendingOrderIDs(ctx)
	if err != nil || len(ids) != 1 || ids[0] != "order-1" {
		t.Fatalf("the background loop had nothing to retry: ids=%v err=%v", ids, err)
	}
	receipt, err := h.service.AttemptDelivery(ctx, ids[0])
	if err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	if !receipt.Delivered {
		t.Fatal("the retry did not deliver the paid order")
	}
	if !h.service.BackgroundRetryEnabled() {
		t.Error("a service with a pending index reports background retry as off")
	}
}

func newPendingTestService(t *testing.T, pending PendingOrders) *Service {
	t.Helper()
	service, err := New(Config{
		Orders:        versionstore.NewMemoryStore[string, Order](),
		Deliver:       newDeliverer(),
		Verifier:      acceptingVerifier(),
		Players:       resolver(),
		SessionSecret: "session", PaymentSecret: "payment",
		Pending: pending,
		Now:     func() time.Time { return time.Unix(1700000000, 0) },
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return service
}
