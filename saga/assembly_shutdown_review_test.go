package saga

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
)

// N06：第三消费者与前两个具有相同的启动清理、drain、强停和重试责任。
type reviewSagaSubscription struct {
	once      sync.Once
	drainOnce sync.Once
	closed    chan struct{}
	drained   chan struct{}
	hold      bool
	stops     atomic.Int32
}

func (s *reviewSagaSubscription) Stop() { s.stops.Add(1); s.once.Do(func() { close(s.closed) }) }
func (s *reviewSagaSubscription) Drain() {
	s.drainOnce.Do(func() { close(s.drained) })
	if !s.hold {
		s.once.Do(func() { close(s.closed) })
	}
}
func (s *reviewSagaSubscription) Closed() <-chan struct{} { return s.closed }

type reviewSagaJetStream struct {
	assemblyJetStream
	failAt    int
	thirdHeld bool
	calls     int
	subs      []*reviewSagaSubscription
}

func (j *reviewSagaJetStream) Subscribe(_ context.Context, _ fnats.JetStreamConsumerConfig, _ fnats.JetStreamHandler) (fnats.IJetStreamSubscription, error) {
	j.calls++
	if j.calls == j.failAt {
		return nil, errors.New("subscribe refused")
	}
	s := &reviewSagaSubscription{closed: make(chan struct{}), drained: make(chan struct{}), hold: j.thirdHeld && len(j.subs) == 2}
	j.subs = append(j.subs, s)
	return s, nil
}
func newReviewSagaAssembly(t *testing.T, j fnats.IJetStream) *Assembly {
	t.Helper()
	cfg := AssemblyConfig{Store: MongoStoreOptions{Database: "shutdown"}, Engine: DefaultOptions(), Prefix: "roost.saga", Stream: fnats.JetStreamConfig{Name: "ROOST_SAGA"}, Completions: CompletionConsumerConfig{Stream: "ROOST_SAGA", Durable: "results", SubjectPrefix: "roost.saga"}, Starts: NestStartConsumerConfig{Stream: "ROOST_EFFECTS", Durable: "starts", EffectPrefix: "roost.effect"}}
	a, err := Assemble(mongotest.NewClient(), j, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := a.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	return a
}
func TestSagaPartialSubscriptionsCleanUpAndRetry(t *testing.T) {
	for _, failAt := range []int{2, 3} {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			j := &reviewSagaJetStream{failAt: failAt}
			a := newReviewSagaAssembly(t, j)
			if err := a.Start(context.Background()); err == nil {
				t.Fatal("subscription failure swallowed")
			}
			for _, s := range j.subs {
				select {
				case <-s.closed:
				default:
					t.Fatal("partial Start leaked an earlier subscription")
				}
			}
			if a.Running() {
				t.Fatal("failed Start ran engine")
			}
			if err := a.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			if a.ConsumersClosed() || !a.Running() {
				t.Fatal("retry did not establish healthy consumers")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := a.Stop(ctx); err != nil {
				t.Fatal(err)
			}
			for _, s := range j.subs {
				select {
				case <-s.closed:
				default:
					t.Fatal("Stop leaked retried subscription")
				}
			}
		})
	}
}
func TestSagaThirdConsumerDrainTimeoutCanFinishOnRetry(t *testing.T) {
	j := &reviewSagaJetStream{thirdHeld: true}
	a := newReviewSagaAssembly(t, j)
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Stop(ctx) }()
	select {
	case <-j.subs[2].drained:
	case <-time.After(time.Second):
		t.Fatal("third consumer was not drained")
	}
	select {
	case err := <-done:
		t.Fatalf("Stop returned before third closure: %v", err)
	default:
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Stop cancellation=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop ignored cancellation")
	}
	for _, s := range j.subs {
		if s.stops.Load() != 1 {
			t.Fatal("hard stop did not cover every consumer")
		}
	}
	finish, cancelFinish := context.WithTimeout(context.Background(), time.Second)
	defer cancelFinish()
	if err := a.Stop(finish); err != nil {
		t.Fatal(err)
	}
	if a.Running() || !a.ConsumersClosed() {
		t.Fatal("retry did not finish lifecycle")
	}
	if err := a.Stop(finish); err != nil {
		t.Fatal(err)
	}
}
