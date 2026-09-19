package platform

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/spf13/viper"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

// RR-20260919-04：一笔已付款的订单必须能被后台循环找到，而"找到"不能依赖
// 第二次写成功。
//
// 旧形状：服务写订单，部署侧在 deliverer 里补写索引。两次写之间进程退出、
// 或者索引那次写失败，就留下一笔后台永远枚举不到的已付款订单——而服务契约
// 允许调用方在订单已记录时就向渠道确认收到，所以"渠道会重投"兜不住。
//
// 现在索引是订单存储自己的一部分，和值同一个脚本。这里对着**真 Redis** 跑，
// 因为要证明的正是脚本的原子性：Go 替身里的"同一次写"是假的。

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

// A deployment can still bring its own, and saying nothing no longer means
// "no recovery".
func TestTheStoreIsTheDefaultPendingSource(t *testing.T) {
	mod := NewMod(
		VerifierFunc(func(context.Context, Credential) (Verified, error) { return Verified{}, nil }),
		PlayerResolverFunc(func(context.Context, Verified) (int64, error) { return 1, nil }),
		DelivererFunc(func(context.Context, Order) error { return nil }),
		nil,
	)
	if mod.pending != nil {
		t.Fatal("a fresh Mod already has a pending source")
	}
	// Provide is what installs the default; it needs Redis, so the assertion
	// here is on the wiring decision rather than on a built service.
	explicit := PendingOrdersFunc(func(context.Context, int) ([]string, error) { return nil, nil })
	if mod.WithPendingOrders(explicit).pending == nil {
		t.Fatal("an explicit index was not kept")
	}
}

// Two keys in one script are only atomic inside one hash slot. A cluster
// deployment whose prefix has no hash tag would get CROSSSLOT errors at best
// and a quietly non-atomic index at worst, so it is refused at Init.
func TestAClusterWithoutAHashTagIsRefused(t *testing.T) {
	cfg := viper.New()
	cfg.Set("platform.key_prefix", "roost:platform")
	cfg.Set("platform.session_secret", "s")
	cfg.Set("platform.payment_secret", "p")
	cfg.Set("redis.cluster_addrs", "10.0.0.1:6379,10.0.0.2:6379")
	mod := NewMod(
		VerifierFunc(func(context.Context, Credential) (Verified, error) { return Verified{}, nil }),
		PlayerResolverFunc(func(context.Context, Verified) (int64, error) { return 1, nil }),
		DelivererFunc(func(context.Context, Order) error { return nil }),
		nil,
	)
	err := mod.Init(cfg)
	if err == nil {
		t.Fatal("a cluster deployment with no hash tag was accepted; its index would not be atomic")
	}
	if !strings.Contains(err.Error(), "hash tag") {
		t.Errorf("the refusal does not say what to change: %v", err)
	}

	// With a tag it starts, and a single-node deployment is unaffected.
	cfg.Set("platform.key_prefix", "{roost:platform}")
	if err := mod.Init(cfg); err != nil {
		t.Errorf("a tagged prefix was refused: %v", err)
	}
	cfg.Set("platform.key_prefix", "roost:platform")
	cfg.Set("redis.cluster_addrs", "")
	if err := mod.Init(cfg); err != nil {
		t.Errorf("a single-node deployment was refused: %v", err)
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
