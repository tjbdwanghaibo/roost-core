//go:build integration

// A3 ②（docs/feature/A3-2-SYNCBUS-DRAINING-UNSUBSCRIBE-2026-10-07.md）：真实 nats-server（私有进程）上，
// 经正式 kit NatsMod + SyncBusMod 装配的两种传输（JetStream / 普通 NATS），退订本身排空：
//   - handler 卡住时 Unsubscribe(短预算) 返回 DeadlineExceeded，handler 放行后重试返回 nil，之后发布的消息不再调 handler；
//   - handler 里用投递 ctx 退订自己不死锁（JetStream 上它是最后一个本地订阅，退订会在 consume 回调里停消费者）。
//
// 修前（旧接口退订函数）在同一环境上退订立即返回、回调仍在跑，见方案 §9。
package syncbus

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	kitnats "github.com/tjbdwanghaibo/roost-core/wiring/nats"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus"
)

const drainTopic = "unsubscribe_drain"

// startDrainNode 与 startMigrationNode 相同的正式装配，传输可选（jetstream / nats）。
func startDrainNode(t *testing.T, url string, sid int32, transport string) fsyncbus.ISyncBus {
	t.Helper()
	cfg := viper.New()
	cfg.Set("sid", sid)
	cfg.Set("server_type", "drain")
	cfg.Set("nats.url", url)
	cfg.Set("syncbus.transport", transport)
	cfg.Set("syncbus.prefix", "zzdrain.sync")
	cfg.Set("syncbus.storage", "memory")
	natsMod, syncMod := kitnats.NewNatsMod(nil), NewSyncBusMod(sid)
	registry := app.NewRegistry(cfg)
	for _, step := range []func() error{
		func() error { return natsMod.Init(cfg) },
		func() error { return natsMod.Provide(registry) },
		natsMod.Start,
		func() error { return syncMod.Init(cfg) },
		func() error { return syncMod.Provide(registry) },
		syncMod.Start,
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = syncMod.StopWithContext(context.Background())
		_ = natsMod.StopWithContext(context.Background())
	})
	bus, ok := app.Lookup[fsyncbus.ISyncBus](registry, mods.ModSyncBus)
	if !ok {
		t.Fatal("sync bus capability not published")
	}
	return bus
}

func publishDrain(t *testing.T, bus fsyncbus.ISyncBus, key int64) {
	t.Helper()
	if err := bus.Publish(&fsyncbus.SyncMsg{Topic: drainTopic, Key: key, Version: key, Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
}

func TestRealSyncBusUnsubscribeDrainsTheSubscription(t *testing.T) {
	url := startPrivateJetStream(t)
	for _, transport := range []string{"jetstream", "nats"} {
		t.Run(transport, func(t *testing.T) {
			subscriber := startDrainNode(t, url, 11, transport)
			publisher := startDrainNode(t, url, 12, transport)
			entered, release, returned := make(chan struct{}, 1), make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			sub, err := subscriber.Subscribe(drainTopic, func(_ context.Context, m *fsyncbus.SyncMsg) error {
				calls.Add(1)
				if m.Key == 1 {
					entered <- struct{}{}
					<-release
					close(returned)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			releaseOnce := false
			t.Cleanup(func() {
				if !releaseOnce {
					close(release)
				}
			})
			time.Sleep(200 * time.Millisecond) // 普通 NATS 的订阅要先到达服务端（至多一次，之前发布的会丢）
			publishDrain(t, publisher, 1)
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("handler did not receive the message")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			firstErr := sub.Unsubscribe(ctx)
			select {
			case <-returned:
				t.Fatal("handler returned before release")
			default:
			}
			close(release)
			releaseOnce = true
			retryErr := sub.Unsubscribe(context.Background())
			handlerReturned := false
			select {
			case <-returned:
				handlerReturned = true
			default:
			}
			publishDrain(t, publisher, 2)
			time.Sleep(500 * time.Millisecond)
			t.Logf("first Unsubscribe=%v retry=%v handler_returned_at_nil=%v calls_after_publish=%d", firstErr, retryErr, handlerReturned, calls.Load())
			if !errors.Is(firstErr, context.DeadlineExceeded) {
				t.Fatalf("Unsubscribe with the handler in flight = %v, want DeadlineExceeded", firstErr)
			}
			if retryErr != nil || !handlerReturned {
				t.Fatal("retry Unsubscribe did not wait for the in-flight handler")
			}
			if calls.Load() != 1 {
				t.Fatalf("handler calls = %d, want 1 (nothing after Unsubscribe returned nil)", calls.Load())
			}
		})
	}
}

func TestRealSyncBusUnsubscribeFromOwnHandler(t *testing.T) {
	url := startPrivateJetStream(t)
	for _, transport := range []string{"jetstream", "nats"} {
		t.Run(transport, func(t *testing.T) {
			subscriber := startDrainNode(t, url, 21, transport)
			publisher := startDrainNode(t, url, 22, transport)
			var sub atomic.Pointer[fsyncbus.Subscription]
			result := make(chan error, 1)
			var calls atomic.Int32
			s, err := subscriber.Subscribe(drainTopic, func(ctx context.Context, _ *fsyncbus.SyncMsg) error {
				calls.Add(1)
				result <- sub.Load().Unsubscribe(ctx)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			sub.Store(s)
			time.Sleep(200 * time.Millisecond)
			publishDrain(t, publisher, 1)
			select {
			case err := <-result:
				if err != nil {
					t.Fatalf("self Unsubscribe = %v, want nil", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Unsubscribe inside the subscription's own handler deadlocked (or the message never arrived)")
			}
			publishDrain(t, publisher, 2)
			time.Sleep(500 * time.Millisecond)
			if calls.Load() != 1 {
				t.Fatalf("handler calls = %d, want 1", calls.Load())
			}
			if err := s.Unsubscribe(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
