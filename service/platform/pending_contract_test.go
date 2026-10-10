package platform

import (
	"context"
	"errors"
	goredis "github.com/redis/go-redis/v9"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func redisOrdersForTest(t *testing.T) (*RedisOrders, *goredis.Client, string) {
	t.Helper()
	addr := os.Getenv("ROOST_REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("ROOST_REDIS_TEST_ADDR is not set")
	}
	client := goredis.NewClient(&goredis.Options{Addr: addr})
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis at %s is not reachable: %v", addr, err)
	}
	prefix := "roost:test:platform:" + strconv.FormatInt(time.Now().UnixNano(), 36)
	orders, err := NewRedisOrders(redisClientAdapter{client: client}, prefix)
	if err != nil {
		t.Fatalf("new orders: %v", err)
	}
	t.Cleanup(func() {
		keys, _ := client.Keys(context.Background(), prefix+"*").Result()
		if len(keys) > 0 {
			client.Del(context.Background(), keys...)
		}
		_ = client.Close()
	})
	return orders, client, prefix
}

// redisClientAdapter is the narrow slice versionstore needs.
type redisClientAdapter struct{ client *goredis.Client }

func (a redisClientAdapter) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return a.client.Eval(ctx, script, keys, args...).Result()
}

func (a redisClientAdapter) Get(ctx context.Context, key string) ([]byte, error) {
	value, err := a.client.Get(ctx, key).Bytes()
	if errors.Is(err, goredis.Nil) {
		return nil, fredis.ErrNil
	}
	return value, err
}

func (a redisClientAdapter) Del(ctx context.Context, keys ...string) (int64, error) {
	return a.client.Del(ctx, keys...).Result()
}

func storedOrder(orderID string, now int64) Order {
	return Order{
		OrderID: orderID, PlayerID: 1001, Channel: "test", ProductID: "gems",
		AmountMinor: 499, State: DeliveryReserved, MaxAttempts: 5,
		PaidAtUnix: now, CreatedAtUnix: now, UpdatedAtUnix: now,
	}
}

// Recording the order IS entering the index. Nothing else has to happen, so
// there is no window in which the process can die and lose the order.
func TestAStoredOrderIsImmediatelyEnumerableIntegration(t *testing.T) {
	orders, _, _ := redisOrdersForTest(t)
	ctx := context.Background()
	now := time.Now().Unix()

	if _, created, err := orders.Create(ctx, "order-1", storedOrder("order-1", now)); err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	pending, err := orders.PendingOrders(ctx, 128)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 1 || pending[0] != "order-1" {
		t.Fatalf("pending = %v, want the order that was just recorded", pending)
	}
}

// The write that finishes an order takes it out of the index, so a delivered
// order is not read again every tick.
func TestATerminalOrderLeavesTheIndexIntegration(t *testing.T) {
	orders, _, _ := redisOrdersForTest(t)
	ctx := context.Background()
	now := time.Now().Unix()
	if _, _, err := orders.Create(ctx, "order-2", storedOrder("order-2", now)); err != nil {
		t.Fatal(err)
	}
	for name, state := range map[string]DeliveryState{
		"delivered": DeliveryDelivered,
		"exhausted": DeliveryExhausted,
		"settled":   DeliverySettled,
	} {
		if _, _, err := orders.Update(ctx, "order-2", func(current Order, _ bool) (Order, bool, error) {
			current.State = state
			return current, true, nil
		}); err != nil {
			t.Fatalf("%s: update: %v", name, err)
		}
		pending, err := orders.PendingOrders(ctx, 128)
		if err != nil {
			t.Fatalf("%s: pending: %v", name, err)
		}
		if len(pending) != 0 {
			t.Fatalf("%s: pending = %v, want the finished order retired", name, pending)
		}
		// Put it back for the next state.
		if _, _, err := orders.Update(ctx, "order-2", func(current Order, _ bool) (Order, bool, error) {
			current.State = DeliveryReserved
			return current, true, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// Backoff is the score: an order whose next attempt is in the future is in the
// index and is not offered, and one that keeps failing moves itself back
// rather than sitting at the head of every page.
func TestBackoffMovesAnOrderInTheIndexIntegration(t *testing.T) {
	orders, _, _ := redisOrdersForTest(t)
	ctx := context.Background()
	now := time.Now().Unix()
	if _, _, err := orders.Create(ctx, "slow", storedOrder("slow", now-10)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := orders.Create(ctx, "new", storedOrder("new", now-5)); err != nil {
		t.Fatal(err)
	}
	if pending, _ := orders.PendingOrders(ctx, 1); len(pending) != 1 || pending[0] != "slow" {
		t.Fatalf("pending = %v, want the older order first", pending)
	}
	// The attempt failed and the service pushed the next one out.
	if _, _, err := orders.Update(ctx, "slow", func(current Order, _ bool) (Order, bool, error) {
		current.Attempts++
		current.NextAttemptAtUnix = now + 3600
		return current, true, nil
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := orders.PendingOrders(ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0] != "new" {
		t.Fatalf("pending = %v, want only the order that is actually due", pending)
	}
}

// RR-20260919-07：一条读不出来 / 已经不存在的成员不能永久占住页里的一个槽。
//
// 索引进了存储之后，读的时候不再逐条查订单，所以"一条坏记录让整页失败"没有了。
// 剩下的那一半是：一个**没有订单记录**的成员（删除的两步之间崩过一次）会被每一
// 页读到、每一次都得到 ErrOrderInvalid，然后继续留在集合里——limit 有多小，
// 它就占掉多大比例的预算。所以循环遇到"这个 id 根本没有订单"时要把它退休掉。
func TestAnIndexEntryWithNoOrderIsRetiredIntegration(t *testing.T) {
	orders, client, prefix := redisOrdersForTest(t)
	ctx := context.Background()
	now := time.Now().Unix()

	// A ghost: in the index, no record behind it.
	if err := client.ZAdd(ctx, PendingIndexKey(prefix), goredis.Z{Score: float64(now - 60), Member: "ghost"}).Err(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := orders.Create(ctx, "real", storedOrder("real", now)); err != nil {
		t.Fatal(err)
	}
	// The ghost is older, so it comes first — and with a small batch it would
	// be the whole batch, every tick, forever.
	if pending, _ := orders.PendingOrders(ctx, 1); len(pending) != 1 || pending[0] != "ghost" {
		t.Fatalf("pending = %v, want the ghost first (that is the shape of the problem)", pending)
	}

	if err := orders.RetirePending(ctx, "ghost"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	pending, err := orders.PendingOrders(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0] != "real" {
		t.Fatalf("pending = %v, want the real order once the ghost is retired", pending)
	}
}

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
