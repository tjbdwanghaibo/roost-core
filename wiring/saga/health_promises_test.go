package saga

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/health"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

// RR-20261005-NC-37：正式 Mod 装配与 health 检查不能遗漏原生完成消费者。
type healthSubscription struct {
	once   sync.Once
	closed chan struct{}
}

func (s *healthSubscription) Stop()                   { s.once.Do(func() { close(s.closed) }) }
func (s *healthSubscription) Drain()                  { s.Stop() }
func (s *healthSubscription) Closed() <-chan struct{} { return s.closed }

type healthJetStream struct {
	subs map[string]*healthSubscription
}

func (*healthJetStream) EnsureStream(context.Context, fnats.JetStreamConfig) error { return nil }
func (*healthJetStream) Publish(context.Context, string, []byte, fnats.JetStreamPublishOptions) (fnats.JetStreamPublishAck, error) {
	return fnats.JetStreamPublishAck{}, nil
}
func (j *healthJetStream) Subscribe(_ context.Context, cfg fnats.JetStreamConsumerConfig, _ fnats.JetStreamHandler) (fnats.IJetStreamSubscription, error) {
	s := &healthSubscription{closed: make(chan struct{})}
	j.subs[cfg.FilterSubject] = s
	return s, nil
}

func TestSagaModHealthDetectsEveryConsumerAndRecovers(t *testing.T) {
	for _, subject := range []string{"roost.saga.result.>", "roost.effect.saga.start", "roost.effect.saga.result.>"} {
		t.Run(subject, func(t *testing.T) {
			cfg := viper.New()
			registry := app.NewRegistry(cfg)
			js := &healthJetStream{subs: make(map[string]*healthSubscription)}
			if err := registry.Register(mods.ModMongo, mongotest.NewClient()); err != nil {
				t.Fatal(err)
			}
			if err := registry.Register(mods.ModNatsJetStream, js); err != nil {
				t.Fatal(err)
			}
			m := NewMod()
			if err := m.Init(cfg); err != nil {
				t.Fatal(err)
			}
			if err := m.Provide(registry); err != nil {
				t.Fatal(err)
			}
			if err := m.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := m.StopWithContext(ctx); err != nil {
					t.Error(err)
				}
			})
			if got := m.checkHealth(context.Background()); got.Status != health.StatusOK {
				t.Fatalf("healthy start: %+v", got)
			}
			if len(js.subs) != 3 || js.subs[subject] == nil {
				t.Fatalf("unexpected subscriptions: %v", js.subs)
			}
			js.subs[subject].Stop()
			if got := m.checkHealth(context.Background()); got.Status != health.StatusFail || got.Message != "durable consumer stopped" {
				t.Errorf("%s closed but health=%+v", subject, got)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			err := m.StopWithContext(ctx)
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			if got := m.checkHealth(context.Background()); got.Status != health.StatusFail {
				t.Fatalf("stopped health: %+v", got)
			}
			if err := m.Start(); err != nil {
				t.Fatal(err)
			}
			if got := m.checkHealth(context.Background()); got.Status != health.StatusOK {
				t.Fatalf("fresh subscriptions did not recover health: %+v", got)
			}
		})
	}
}
