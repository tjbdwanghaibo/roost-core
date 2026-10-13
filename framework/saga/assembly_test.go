package saga

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
)

type drainTestSubscription struct {
	drained chan struct{}
	closed  chan struct{}
}

func (s *drainTestSubscription) Stop()                   {}
func (s *drainTestSubscription) Drain()                  { close(s.drained) }
func (s *drainTestSubscription) Closed() <-chan struct{} { return s.closed }

// 停机先 Drain 再等消费者真正关闭：消费者仍在投递时不能拆掉引擎。
func TestDrainSubscriptionsWaitsForConsumerClosure(t *testing.T) {
	sub := &drainTestSubscription{drained: make(chan struct{}), closed: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- drainSubscriptions(context.Background(), []fnats.IJetStreamSubscription{sub}) }()
	<-sub.drained
	select {
	case err := <-done:
		t.Fatalf("drain returned before closure: %v", err)
	default:
	}
	close(sub.closed)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("drain did not finish after closure")
	}
}

type assemblyJetStream struct{}

func (assemblyJetStream) EnsureStream(context.Context, fnats.JetStreamConfig) error { return nil }
func (assemblyJetStream) Publish(context.Context, string, []byte, fnats.JetStreamPublishOptions) (fnats.JetStreamPublishAck, error) {
	return fnats.JetStreamPublishAck{}, nil
}
func (assemblyJetStream) Subscribe(context.Context, fnats.JetStreamConsumerConfig, fnats.JetStreamHandler) (fnats.IJetStreamSubscription, error) {
	return nil, context.Canceled
}

// Assemble 是 Mod 发布的 saga 能力：缺 Mongo / JetStream 客户端拒绝；装配出 store、transport、engine 并注册给定定义；
// 未启动的装配报告未运行、消费者已关闭。
func TestAssembleBuildsEngineAndRegistersDefinitions(t *testing.T) {
	cfg := AssemblyConfig{
		Store:  MongoStoreOptions{Database: "saga"},
		Engine: DefaultOptions(),
		Prefix: "roost.saga",
		Stream: fnats.JetStreamConfig{Name: "ROOST_SAGA", Subjects: []string{"roost.saga.>"}},
	}
	cfg.Engine.Owner = "assembly-test"
	if _, err := Assemble(nil, assemblyJetStream{}, cfg); err == nil {
		t.Fatal("nil mongo client was assembled")
	}
	if _, err := Assemble(mongotest.NewClient(), nil, cfg); err == nil {
		t.Fatal("nil jetstream client was assembled")
	}
	definition := Definition{Type: "rally", Version: 1, Steps: []Step{{Name: "reserve", ForwardTopic: "reserve", Timeout: time.Second, MaxAttempts: 3, BackoffMin: time.Millisecond, BackoffMax: time.Second}}}
	asm, err := Assemble(mongotest.NewClient(), assemblyJetStream{}, cfg, definition)
	if err != nil {
		t.Fatal(err)
	}
	if asm.Store == nil || asm.Transport == nil || asm.Engine == nil {
		t.Fatalf("assembly incomplete: %+v", asm)
	}
	if err := asm.Engine.Register(definition); err == nil {
		t.Fatal("definition registered by Assemble could be registered again")
	}
	if asm.Running() || asm.RunError() != nil || !asm.ConsumersClosed() {
		t.Fatalf("unstarted assembly: running=%v err=%v consumersClosed=%v", asm.Running(), asm.RunError(), asm.ConsumersClosed())
	}
	var none *Assembly
	if err := none.Start(context.Background()); err == nil {
		t.Fatal("nil assembly Start returned no error")
	}
	if err := none.Stop(context.Background()); err != nil {
		t.Fatalf("nil assembly Stop = %v", err)
	}
}

// 任意一个必需消费者（普通结果、启动意图、原生 Nest 完成回执）缺失或退出，装配都报告不健康。
func TestAssemblyHealthIncludesEveryRequiredConsumer(t *testing.T) {
	for _, which := range []string{"ordinary_result", "start", "native_result"} {
		for _, missing := range []bool{false, true} {
			t.Run(which+map[bool]string{false: "/closed", true: "/missing"}[missing], func(t *testing.T) {
				subs := []*recordedSubscription{
					{closed: make(chan struct{})}, {closed: make(chan struct{})}, {closed: make(chan struct{})},
				}
				a := &Assembly{resultSub: subs[0], startSub: subs[1], nestResults: subs[2]}
				if a.ConsumersClosed() {
					t.Fatal("three open consumers reported closed")
				}
				index := map[string]int{"ordinary_result": 0, "start": 1, "native_result": 2}[which]
				if missing {
					switch index {
					case 0:
						a.resultSub = nil
					case 1:
						a.startSub = nil
					case 2:
						a.nestResults = nil
					}
				} else {
					subs[index].Stop()
				}
				if !a.ConsumersClosed() {
					t.Fatalf("%s unavailable but ConsumersClosed=false (missing=%v)", which, missing)
				}
				// 独立的新生命周期恢复三个消费者；不伪造旧订阅复活。
				recovered := &Assembly{resultSub: &recordedSubscription{closed: make(chan struct{})}, startSub: &recordedSubscription{closed: make(chan struct{})}, nestResults: &recordedSubscription{closed: make(chan struct{})}}
				if recovered.ConsumersClosed() {
					t.Fatal("replacement consumers did not restore health")
				}
			})
		}
	}
}

func TestAssemblyNativeConsumerClosureIsVisibleAfterFormalStart(t *testing.T) {
	cfg := AssemblyConfig{
		Store: MongoStoreOptions{Database: "health"}, Engine: DefaultOptions(), Prefix: "roost.saga",
		Stream:      fnats.JetStreamConfig{Name: "ROOST_SAGA"},
		Completions: CompletionConsumerConfig{Stream: "ROOST_SAGA", Durable: "results", SubjectPrefix: "roost.saga"},
		Starts:      NestStartConsumerConfig{Stream: "ROOST_EFFECTS", Durable: "starts", EffectPrefix: "roost.effect"},
	}
	a, err := Assemble(mongotest.NewClient(), newRecordingJetStream(), cfg, testDefinition())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := a.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	if !a.Running() || a.ConsumersClosed() {
		t.Fatal("formal Start did not establish healthy runtime")
	}
	a.nestResults.Stop()
	if !a.Running() || !a.ConsumersClosed() {
		t.Fatalf("native consumer failure masked by running loop: running=%v closed=%v", a.Running(), a.ConsumersClosed())
	}
}

// 三个消费者承担同样的启动失败清理、drain、超时强停和重试收尾责任。
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
