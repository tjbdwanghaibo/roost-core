package platform

import (
	"context"
	"errors"
	"fmt"

	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

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
func TestBugfix6RetirementCannotHideLatePaidOrder(t *testing.T) {
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

func TestBugfix6PendingMaintenanceControls(t *testing.T) {
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
