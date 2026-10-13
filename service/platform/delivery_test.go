// 交付与外部结果：未知发放阻止结算且不重复发放、回执须拥有待定证明、退休不藏起迟到的已付订单、对账、退避与预算竞争、坏记录推后。

package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

func bugfix4Platform(t *testing.T, attempts int, deliver Deliverer) *harness {
	t.Helper()
	h := newHarness(t, func(c *Config) {
		c.DeliveryAttempts = attempts
		c.Deliver = deliver
		if os.Getenv("ROOST_REVIEW4_BACKEND") == "redis" {
			addr := os.Getenv("ROOST_REVIEW_REDIS")
			if addr == "" {
				t.Fatal("isolated Redis address is required")
			}
			client, err := driver.NewClient(fredis.DefaultConfig(addr))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			store, err := NewRedisOrders(client, fmt.Sprintf("review4:platform:%d", time.Now().UnixNano()))
			if err != nil {
				t.Fatal(err)
			}
			c.Orders = store
		}
	})
	h.orders = h.service.cfg.Orders
	order := Order{OrderID: "paid", PlayerID: 1, Channel: "store", ProductID: "gems-100", AmountMinor: 100, Currency: "USD", State: DeliveryReserved, MaxAttempts: int32(attempts), PaidAtUnix: h.clock.Now().Unix(), CreatedAtUnix: h.clock.Now().Unix(), UpdatedAtUnix: h.clock.Now().Unix()}
	if err := order.Validate(); err != nil {
		t.Fatal(err)
	}
	order.PayloadDigest = contentDigest(order)
	_, created, err := h.orders.Create(context.Background(), "paid", order)
	if err != nil || !created {
		t.Fatal(created, err)
	}
	return h
}

// The goods commit at the external boundary; only the response is lost.
func TestUnknownGrantMustBlockSettlement(t *testing.T) {
	grants := 0
	h := bugfix4Platform(t, 1, DelivererFunc(func(context.Context, Order) error { grants++; return context.DeadlineExceeded }))
	ctx := context.Background()
	_, firstErr := h.service.AttemptDelivery(ctx, "paid")
	if !errors.Is(firstErr, context.DeadlineExceeded) || grants != 1 {
		t.Fatal(firstErr, grants)
	}
	before, found, err := h.service.Order(ctx, "paid")
	if err != nil || !found {
		t.Fatal(err, found)
	}
	settled, settleErr := h.service.SettleOutOfBand(ctx, "paid", "refund after exhausted delivery")
	if len(before.PendingAttempts) == 0 || !errors.Is(settleErr, ErrNotResolvable) {
		t.Fatalf("external grant=%d timeout=%v pending=%v state=%s; refund admitted: state=%s err=%v", grants, firstErr, before.PendingAttempts, before.State, settled.State, settleErr)
	}
}

func TestUnknownGrantMustNotGrantAgain(t *testing.T) {
	grants := 0
	h := bugfix4Platform(t, 2, DelivererFunc(func(context.Context, Order) error {
		grants++
		if grants == 1 {
			return context.DeadlineExceeded
		}
		return nil
	}))
	ctx := context.Background()
	if _, err := h.service.AttemptDelivery(ctx, "paid"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	h.clock.advance(6 * time.Second)
	receipt, err := h.service.AttemptDelivery(ctx, "paid")
	if grants != 1 {
		t.Fatalf("lost external grant reply caused %d grants; receipt=%+v err=%v", grants, receipt, err)
	}
}

func TestUnknownGrantProofSurvivesServiceReconstruction(t *testing.T) {
	grants := 0
	h := bugfix4Platform(t, 3, DelivererFunc(func(context.Context, Order) error {
		grants++
		return context.DeadlineExceeded
	}))
	ctx := context.Background()
	if _, err := h.service.AttemptDelivery(ctx, "paid"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	rebuilt, err := New(h.service.cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.clock.advance(time.Hour)
	if _, err := rebuilt.AttemptDelivery(ctx, "paid"); !errors.Is(err, ErrDeliveryExpired) || grants != 1 {
		t.Fatal("reconstruction retried an unknown grant", grants, err)
	}
	if _, err := rebuilt.ReopenDelivery(ctx, "paid", "retry before reconciliation"); !errors.Is(err, ErrNotResolvable) {
		t.Fatal(err)
	}
	if _, err := rebuilt.ResolvePendingAttempts(ctx, "paid", "external record inspected; refund chosen"); err != nil {
		t.Fatal(err)
	}
	if got, err := rebuilt.SettleOutOfBand(ctx, "paid", "refunded after reconciliation"); err != nil || got.State != DeliverySettled {
		t.Fatal(got, err)
	}
}

// The collaborator explicitly proves that the refusal performed no grant.
func TestKnownNoGrantControl(t *testing.T) {
	h := bugfix4Platform(t, 1, DelivererFunc(func(context.Context, Order) error {
		return fmt.Errorf("%w: rejected before grant", ErrDeliveryNotApplied)
	}))
	ctx := context.Background()
	if _, err := h.service.AttemptDelivery(ctx, "paid"); !errors.Is(err, ErrDeliveryExpired) {
		t.Fatal(err)
	}
	got, err := h.service.SettleOutOfBand(ctx, "paid", "verified no grant; refunded")
	if err != nil || got.State != DeliverySettled {
		t.Fatal(got, err)
	}
}

func TestCallbackReceiptMustOwnPendingProof(t *testing.T) {
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	h := bugfix4Platform(t, 2, DelivererFunc(func(context.Context, Order) error { close(started); <-release; return nil }))
	raw, sig := callback(t, "alias-order", 1, 100)
	ctx := context.Background()
	go func() { _, err := h.service.HandleCallback(ctx, raw, sig); done <- err }()
	<-started
	defer func() {
		close(release)
		if err := <-done; err != nil {
			t.Errorf("original callback completion: %v", err)
		}
	}()
	receipt, err := h.service.HandleCallback(ctx, raw, sig)
	if !errors.Is(err, ErrDeliveryHeld) || len(receipt.Order.PendingAttempts) != 1 {
		t.Fatal(receipt, err)
	}
	before, _, err := h.orders.Get(ctx, "alias-order")
	if err != nil {
		t.Fatal(err)
	}
	expected := before.Value.PendingAttempts[0]
	receipt.Order.PendingAttempts[0] = expected + 100
	after, _, err := h.orders.Get(ctx, "alias-order")
	if err != nil {
		t.Fatal(err)
	}
	if after.Value.PendingAttempts[0] != expected || after.Version != before.Version {
		t.Fatalf("receipt changed authoritative pending proof without CAS: before=%d after=%v version=%d/%d", expected, after.Value.PendingAttempts, before.Version, after.Version)
	}
}

type bugfix6Retiring struct {
	*RedisOrders
	entered, release chan struct{}
	once             sync.Once
}

func (p *bugfix6Retiring) RetirePending(ctx context.Context, id string) error {
	close(p.entered)
	select {
	case <-p.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return p.RedisOrders.RetirePending(ctx, id)
}

func bugfix6Wait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("barrier not reached")
	}
}

// The barrier delays the real cleanup after the real retry loop observed
// absence. No order-store result or Lua behavior is mocked.
func TestRetirementCannotHideLatePaidOrder(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("reconstruct_%v", restart), func(t *testing.T) {
			orders, rawClient, prefix := redisOrdersForTest(t)
			gate := &bugfix6Retiring{RedisOrders: orders, entered: make(chan struct{}), release: make(chan struct{})}
			unblock := func() { gate.once.Do(func() { close(gate.release) }) }
			t.Cleanup(unblock)
			h := newHarness(t, func(cfg *Config) { cfg.Orders = orders; cfg.Pending = gate })
			orders.now = h.clock.Now
			h.deliverer.failFor["late-paid"] = 1
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := rawClient.ZAdd(ctx, PendingIndexKey(prefix), goredis.Z{Member: "late-paid", Score: float64(h.clock.Now().Unix() - 60)}).Err(); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { defer close(done); h.service.retryPendingOnce(ctx) }()
			bugfix6Wait(t, gate.entered)
			payload, signature := callback(t, "late-paid", 1001, 499)
			receipt, err := h.service.HandleCallback(ctx, payload, signature)
			if !errors.Is(err, ErrDeliveryFailed) || receipt.Order.OrderID != "late-paid" {
				t.Fatalf("callback: receipt=%+v err=%v", receipt, err)
			}
			if _, err := rawClient.ZScore(ctx, PendingIndexKey(prefix), "late-paid").Result(); err != nil {
				t.Fatalf("new paid order was not initially indexed: %v", err)
			}
			unblock()
			bugfix6Wait(t, done)
			h.clock.advance(10 * time.Second)
			service := h.service
			if restart {
				fresh, err := NewRedisOrders(redisClientAdapter{client: rawClient}, prefix)
				if err != nil {
					t.Fatal(err)
				}
				fresh.now = h.clock.Now
				cfg := h.service.cfg
				cfg.Orders = fresh
				cfg.Pending = fresh
				service, err = New(cfg)
				if err != nil {
					t.Fatal(err)
				}
				orders = fresh
			}
			pending, err := orders.PendingOrders(ctx, 128)
			if err != nil {
				t.Fatal(err)
			}
			service.retryPendingOnce(ctx)
			value, found, err := orders.Get(ctx, "late-paid")
			if err != nil || !found {
				t.Fatalf("stored order: found=%v err=%v", found, err)
			}
			t.Logf("pending=%v stored_state=%s due=%v attempts=%d pending_attempts=%v grants=%d", pending, value.Value.State, value.Value.Due(h.clock.Now().Unix()), value.Value.Attempts, value.Value.PendingAttempts, h.deliverer.grantsFor("late-paid"))
			if value.Value.State != DeliveryDelivered || h.deliverer.grantsFor("late-paid") != 1 {
				t.Fatal("late paid order lost its retry index and never received its goods")
			}
		})
	}
}

func TestPendingMaintenanceControls(t *testing.T) {
	t.Run("ghost_only", func(t *testing.T) {
		orders, client, prefix := redisOrdersForTest(t)
		h := newHarness(t, func(cfg *Config) { cfg.Orders = orders; cfg.Pending = orders })
		orders.now = h.clock.Now
		if err := client.ZAdd(context.Background(), PendingIndexKey(prefix), goredis.Z{Member: "ghost", Score: 1}).Err(); err != nil {
			t.Fatal(err)
		}
		h.service.retryPendingOnce(context.Background())
		ids, err := orders.PendingOrders(context.Background(), 128)
		if err != nil || len(ids) != 0 {
			t.Fatalf("ghost not retired: %v %v", ids, err)
		}
	})
	t.Run("existing_paid_order", func(t *testing.T) {
		orders, _, _ := redisOrdersForTest(t)
		h := newHarness(t, func(cfg *Config) { cfg.Orders = orders; cfg.Pending = orders })
		orders.now = h.clock.Now
		_, created, err := orders.Create(context.Background(), "known", storedOrder("known", h.clock.Now().Unix()))
		if err != nil || !created {
			t.Fatal(err)
		}
		h.service.retryPendingOnce(context.Background())
		value, found, err := orders.Get(context.Background(), "known")
		if err != nil || !found || value.Value.State != DeliveryDelivered || h.deliverer.grantsFor("known") != 1 {
			t.Fatalf("known order: %+v %v %v", value, found, err)
		}
	})
	t.Run("defer_does_not_resurrect_terminal", func(t *testing.T) {
		orders, client, prefix := redisOrdersForTest(t)
		ctx := context.Background()
		terminal := storedOrder("done", time.Now().Unix())
		terminal.State = DeliveryDelivered
		if _, _, err := orders.Create(ctx, "done", terminal); err != nil {
			t.Fatal(err)
		}
		if err := orders.DeferPending(ctx, "done", time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := client.ZScore(ctx, PendingIndexKey(prefix), "done").Result(); !errors.Is(err, goredis.Nil) {
			t.Fatalf("terminal index recreated: %v", err)
		}
	})
}

func TestReconciledSettlementRejectsStaleDeliveryCompletion(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	h := newHarness(t, func(c *Config) {
		c.Deliver = DelivererFunc(func(context.Context, Order) error { close(started); <-release; return nil })
	})
	ctx := context.Background()
	if _, _, err := h.orders.Create(ctx, "reconciled", Order{OrderID: "reconciled", PlayerID: 1, State: DeliveryReserved, MaxAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	go func() { _, err := h.service.AttemptDelivery(ctx, "reconciled"); done <- err }()
	<-started
	h.clock.advance(6 * time.Second)
	if _, err := h.service.AttemptDelivery(ctx, "reconciled"); !errors.Is(err, ErrDeliveryExpired) {
		close(release)
		<-done
		t.Fatal(err)
	}
	if _, err := h.service.ReopenDelivery(ctx, "reconciled", "retry"); !errors.Is(err, ErrNotResolvable) {
		close(release)
		<-done
		t.Fatal(err)
	}
	if _, err := h.service.ResolvePendingAttempts(ctx, "reconciled", "delivery stopped; external result reconciled"); err != nil {
		close(release)
		<-done
		t.Fatal(err)
	}
	if _, err := h.service.SettleOutOfBand(ctx, "reconciled", "refunded"); err != nil {
		close(release)
		<-done
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, ErrOrderSettled) {
		t.Fatal(err)
	}
	got, _, err := h.service.Order(ctx, "reconciled")
	if err != nil || got.State != DeliverySettled || got.AdminNote != "refunded" {
		t.Fatal(got, err)
	}
}

// blockingDeliverer parks its first call until released, so a second attempt
// can claim and deliver the same order while the first is still inside the
// deliverer — the one window the in-CAS claim cannot close, because the
// backoff that guards it can elapse while a slow deliverer is running.
type blockingDeliverer struct {
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	release chan struct{}
}

func (d *blockingDeliverer) Deliver(_ context.Context, _ Order) error {
	d.mu.Lock()
	d.calls++
	first := d.calls == 1
	d.mu.Unlock()
	if first {
		close(d.entered)
		<-d.release
	}
	return nil
}

// An outstanding external call remains proof even after its backoff expires.
func TestElapsedBackoffDoesNotStartAnotherExternalGrant(t *testing.T) {
	d := &blockingDeliverer{entered: make(chan struct{}), release: make(chan struct{})}
	h := newHarness(t, func(cfg *Config) { cfg.Deliver = d })
	ctx := context.Background()
	raw, signature := callback(t, "order-1", 1001, 499)
	done := make(chan error, 1)
	go func() { _, err := h.service.HandleCallback(ctx, raw, signature); done <- err }()
	<-d.entered
	h.clock.advance(time.Hour)
	second, err := h.service.AttemptDelivery(ctx, "order-1")
	close(d.release)
	firstErr := <-done
	if !errors.Is(err, ErrDeliveryExpired) || len(second.Order.PendingAttempts) != 1 {
		t.Fatalf("pending call was retried: %+v %v", second, err)
	}
	if firstErr != nil {
		t.Fatal(firstErr)
	}
	stored, found, err := h.service.Order(ctx, "order-1")
	if err != nil || !found || stored.State != DeliveryDelivered || len(stored.PendingAttempts) != 0 {
		t.Fatalf("late authoritative success: %+v %v", stored, err)
	}
	if d.calls != 1 {
		t.Fatalf("external calls=%d", d.calls)
	}
}

// An order whose budget is already spent when an attempt claims it — a
// stored record that says "reserved, 3 of 3 attempts" — is moved to
// exhausted and counted, not refused in place: a refusal leaves an order
// that looks retryable to every sweep forever.
func TestClaimingASpentBudgetRecordsExhaustionRatherThanRefusingInPlace(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	now := h.clock.Now().Unix()
	if _, _, err := h.orders.Create(ctx, "order-9", Order{
		OrderID: "order-9", PlayerID: 1001, Channel: "test", ProductID: "gems", AmountMinor: 499,
		State: DeliveryReserved, Attempts: 3, MaxAttempts: 3,
		PaidAtUnix: now, CreatedAtUnix: now, UpdatedAtUnix: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.AttemptDelivery(ctx, "order-9"); !errors.Is(err, ErrDeliveryExpired) {
		t.Fatalf("claiming a spent budget produced %v, want ErrDeliveryExpired", err)
	}
	stored, found, err := h.orders.Get(ctx, "order-9")
	if err != nil || !found {
		t.Fatalf("order vanished: found=%v err=%v", found, err)
	}
	if stored.Value.State != DeliveryExhausted {
		t.Fatalf("the stored order is %s after its budget was found spent, want exhausted", stored.Value.State)
	}
	if got := h.metrics.Count("dropped:order.exhausted"); got != 1 {
		t.Fatalf("exhaustion at claim reported %d drops; %s", got, h.metrics.Events())
	}
	if h.deliverer.grantsFor("order-9") != 0 {
		t.Fatal("the deliverer ran for an order with no budget")
	}
}

func TestSettledOrderSurvivesLateDelivery(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	h := newHarness(t, func(c *Config) {
		c.DeliveryAttempts = 1
		c.Deliver = DelivererFunc(func(context.Context, Order) error { close(started); <-release; return nil })
	})
	ctx := context.Background()
	_, created, err := h.orders.Create(ctx, "late", Order{OrderID: "late", PlayerID: 1, ProductID: "gem", AmountMinor: 1, State: DeliveryReserved, MaxAttempts: 1})
	if err != nil || !created {
		t.Fatal(err)
	}
	go func() { _, err := h.service.AttemptDelivery(ctx, "late"); done <- err }()
	<-started
	h.clock.advance(6 * time.Second)
	_, err = h.service.AttemptDelivery(ctx, "late")
	if !errors.Is(err, ErrDeliveryExpired) {
		close(release)
		<-done
		t.Fatal(err)
	}
	_, err = h.service.SettleOutOfBand(ctx, "late", "refunded")
	close(release)
	lateErr := <-done
	if !errors.Is(err, ErrNotResolvable) || lateErr != nil {
		t.Fatal(err, lateErr)
	}
	order, _, err := h.service.Order(ctx, "late")
	if err != nil {
		t.Fatal(err)
	}
	if order.State != DeliveryDelivered || order.AdminNote != "" {
		t.Fatalf("pending delivery was incorrectly settled: state=%s note=%s", order.State, order.AdminNote)
	}
}

// 一条读不回来的订单不能永久占住重试页：坏记录被推后（不是退休——它是一条需要人看的真记录），页面往前走，健康订单照常进入 AttemptDelivery。
// 历史：RR-20260920-05。

// scoredPending is an index with real scores, so "oldest first" and "deferred
// entries move" both mean something here.
type scoredPending struct {
	mu     sync.Mutex
	scores map[string]int64
	now    int64
}

func (p *scoredPending) PendingOrders(_ context.Context, limit int) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	type entry struct {
		id    string
		score int64
	}
	due := make([]entry, 0, len(p.scores))
	for id, score := range p.scores {
		if score <= p.now {
			due = append(due, entry{id: id, score: score})
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].score != due[j].score {
			return due[i].score < due[j].score
		}
		return due[i].id < due[j].id
	})
	out := make([]string, 0, limit)
	for _, item := range due {
		if len(out) == limit {
			break
		}
		out = append(out, item.id)
	}
	return out, nil
}

func (p *scoredPending) DeferPending(_ context.Context, orderID string, notBefore time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.scores[orderID]; !ok {
		return nil
	}
	p.scores[orderID] = notBefore.Unix()
	return nil
}

func TestAnUndecodableOrderStopsBlockingThePage(t *testing.T) {
	const poisoned = RetryBatch
	index := &scoredPending{scores: map[string]int64{}, now: 1700000000}
	orders := versionstore.NewMemoryStore[string, Order]()
	// The poisoned members are older than the healthy one, so they sort first.
	for i := 0; i < poisoned; i++ {
		index.scores[poisonID(i)] = 1600000000 + int64(i)
	}
	index.scores["healthy"] = 1650000000

	service, err := New(Config{
		Orders:        &undecodableOrders{store: orders},
		Deliver:       newDeliverer(),
		Verifier:      acceptingVerifier(),
		Players:       resolver(),
		SessionSecret: "session", PaymentSecret: "payment",
		Pending: index,
		Now:     func() time.Time { return time.Unix(index.now, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}

	// One pass: every id on the page is unreadable.
	service.retryPendingOnce(context.Background())

	page, err := index.PendingOrders(context.Background(), RetryBatch)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range page {
		if id == "healthy" {
			return // the healthy order is now reachable, which is the promise
		}
	}
	t.Fatalf("after a pass over %d unreadable orders the page is still %v; the healthy order is never read", poisoned, page[:min(3, len(page))])
}

func poisonID(i int) string { return "poison-" + time.Unix(int64(i), 0).UTC().Format("150405.000") }

// undecodableOrders answers every Get with a malformed-record error, the way a
// store does when the bytes behind a key no longer decode.
type undecodableOrders struct {
	store versionstore.Store[string, Order]
}

func (o *undecodableOrders) Get(ctx context.Context, key string) (versionstore.Versioned[Order], bool, error) {
	if key == "healthy" {
		return o.store.Get(ctx, key)
	}
	return versionstore.Versioned[Order]{}, false, versionstore.ErrMalformedRecord
}

func (o *undecodableOrders) Update(ctx context.Context, key string, mutate versionstore.Mutate[Order]) (versionstore.Versioned[Order], bool, error) {
	if key == "healthy" {
		return o.store.Update(ctx, key, mutate)
	}
	return versionstore.Versioned[Order]{}, false, versionstore.ErrMalformedRecord
}

func (o *undecodableOrders) Create(ctx context.Context, key string, value Order) (versionstore.Versioned[Order], bool, error) {
	return o.store.Create(ctx, key, value)
}

func (o *undecodableOrders) Delete(ctx context.Context, key string, expect versionstore.Versioned[Order]) error {
	return o.store.Delete(ctx, key, expect)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
