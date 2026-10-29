package saga

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
)

// 装配的生命周期：Assemble 缺依赖拒绝、登记定义；未启动报告未运行、中继已关闭；Stop 先 Drain 启动中继、在 ctx 内等它真正关闭
// （中继仍在投递时不能停协调器），超时强停并返回 ctx 错误，用新 ctx 再调用能收尾。协调器的 Start / 三步停机走真实 JetStream
// （jetstream_coordinator_real_integration_test.go 的 TestRealAssemblyStartsTheRelayAndStopsInOrder）。
// 历史：Redis 装配的三个消费者（普通结果、启动意图、原生 Nest 完成）各有同一套用例；第 4 批只剩启动中继。

type assemblyJetStream struct{}

func (assemblyJetStream) EnsureStream(context.Context, fnats.JetStreamConfig) error { return nil }
func (assemblyJetStream) Publish(context.Context, string, []byte, fnats.JetStreamPublishOptions) (fnats.JetStreamPublishAck, error) {
	return fnats.JetStreamPublishAck{}, nil
}
func (assemblyJetStream) Subscribe(context.Context, fnats.JetStreamConsumerConfig, fnats.JetStreamHandler) (fnats.IJetStreamSubscription, error) {
	return nil, context.Canceled
}

func TestAssembleBuildsEngineAndRegistersDefinitions(t *testing.T) {
	options := Options{Layout: Layout{Prefix: "unit.saga", Stream: "UNIT_SAGA", Partitions: 1, Groups: 1}}
	for name, assemble := range map[string]func() (*Assembly, error){
		"no log": func() (*Assembly, error) {
			return Assemble(nil, &stubRaw{}, assemblyJetStream{}, AssemblyConfig{Engine: options})
		},
		"no raw": func() (*Assembly, error) {
			return Assemble(stubLog{}, nil, assemblyJetStream{}, AssemblyConfig{Engine: options})
		},
		"no jetstream": func() (*Assembly, error) {
			return Assemble(stubLog{}, &stubRaw{}, nil, AssemblyConfig{Engine: options})
		},
		"bad layout": func() (*Assembly, error) {
			return Assemble(stubLog{}, &stubRaw{}, assemblyJetStream{}, AssemblyConfig{Engine: Options{Layout: Layout{Prefix: "p", Stream: "S", Partitions: 100, Groups: 1}}})
		},
		"bad definition": func() (*Assembly, error) {
			return Assemble(stubLog{}, &stubRaw{}, assemblyJetStream{}, AssemblyConfig{Engine: options}, Definition{Type: "x"})
		},
	} {
		if _, err := assemble(); err == nil {
			t.Fatalf("%s: Assemble accepted", name)
		}
	}
	a, err := Assemble(stubLog{}, &stubRaw{}, assemblyJetStream{}, AssemblyConfig{Engine: options}, testDefinition())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Engine.definition("rally", 1); !ok {
		t.Fatal("definition was not registered")
	}
	if a.Running() || !a.ConsumersClosed() || a.RunError() != nil {
		t.Fatalf("an unstarted assembly: running=%v consumersClosed=%v runError=%v", a.Running(), a.ConsumersClosed(), a.RunError())
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop before Start = %v", err)
	}
}

type relaySubscription struct {
	once      sync.Once
	drainOnce sync.Once
	closed    chan struct{}
	drained   chan struct{}
	hold      bool
	stops     atomic.Int32
}

func (s *relaySubscription) Stop() { s.stops.Add(1); s.once.Do(func() { close(s.closed) }) }
func (s *relaySubscription) Drain() {
	s.drainOnce.Do(func() { close(s.drained) })
	if !s.hold {
		s.once.Do(func() { close(s.closed) })
	}
}
func (s *relaySubscription) Closed() <-chan struct{} { return s.closed }

// assemblyWithRelay 是一个“已启动”的装配：协调器没有拥有的分区（停机只停空的发起方一侧），启动中继是可控的替身。
func assemblyWithRelay(t *testing.T, relay *relaySubscription) *Assembly {
	t.Helper()
	a, err := Assemble(stubLog{}, &stubRaw{}, assemblyJetStream{}, AssemblyConfig{Engine: Options{Layout: Layout{Prefix: "unit.saga", Stream: "UNIT_SAGA", Partitions: 1, Groups: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	a.startSub, a.running = relay, true
	return a
}

// 停机先 Drain 启动中继再等它真正关闭：中继仍在投递时不能停协调器（它的 StartSaga 要等判定）。
func TestAssemblyStopWaitsForTheRelayToClose(t *testing.T) {
	relay := &relaySubscription{closed: make(chan struct{}), drained: make(chan struct{}), hold: true}
	a := assemblyWithRelay(t, relay)
	done := make(chan error, 1)
	go func() { done <- a.Stop(context.Background()) }()
	<-relay.drained
	select {
	case err := <-done:
		t.Fatalf("Stop returned before the relay closed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if !a.Running() {
		t.Fatal("the coordinator was stopped while the relay was still delivering")
	}
	relay.once.Do(func() { close(relay.closed) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop did not finish after the relay closed")
	}
	if a.Running() || !a.ConsumersClosed() {
		t.Fatal("Stop did not finish the lifecycle")
	}
}

// 中继排空超时：强停中继、返回 ctx 错误；用新 ctx 再调用收尾，重复调用返回 nil。
func TestAssemblyRelayDrainTimeoutCanFinishOnRetry(t *testing.T) {
	relay := &relaySubscription{closed: make(chan struct{}), drained: make(chan struct{}), hold: true}
	a := assemblyWithRelay(t, relay)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Stop(ctx) }()
	<-relay.drained
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Stop cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop ignored cancellation")
	}
	if relay.stops.Load() != 1 {
		t.Fatal("the relay was not hard-stopped after the drain timed out")
	}
	finish, cancelFinish := context.WithTimeout(context.Background(), time.Second)
	defer cancelFinish()
	if err := a.Stop(finish); err != nil {
		t.Fatal(err)
	}
	if a.Running() || !a.ConsumersClosed() {
		t.Fatal("retry did not finish the lifecycle")
	}
	if err := a.Stop(finish); err != nil {
		t.Fatal(err)
	}
}

// recordedSubscription closes on Drain, the way a real consumer does once it
// has finished delivering. A stub whose Closed() never fires makes
// Assembly.Stop wait forever.
type recordedSubscription struct {
	once   sync.Once
	closed chan struct{}
}

func (s *recordedSubscription) Stop()                   { s.once.Do(func() { close(s.closed) }) }
func (s *recordedSubscription) Drain()                  { s.once.Do(func() { close(s.closed) }) }
func (s *recordedSubscription) Closed() <-chan struct{} { return s.closed }

// 冷副本的装配：每个进程都有回落读（Options.ColdCopy），只有拥有分区的进程跑投影；参数越界、缺 Mongo 时 Assemble 拒绝。
func TestAssembleWiresTheColdCopyReaderEverywhereAndTheProjectorOnlyOnOwners(t *testing.T) {
	layout := Layout{Prefix: "unit.saga", Stream: "UNIT_SAGA", Partitions: 2, Groups: 1}
	client := mongotest.NewClient()
	settings := &ColdCopySettings{Client: client, Database: "saga", TerminalOnly: true}
	client0 := Options{Layout: layout}
	owner := Options{Layout: layout, Owned: []int{0, 1}}
	initiator, err := Assemble(stubLog{}, &stubRaw{}, assemblyJetStream{}, AssemblyConfig{Engine: client0, ColdCopy: settings})
	if err != nil {
		t.Fatal(err)
	}
	if initiator.Engine.opts.ColdCopy == nil || initiator.coldCopy != nil {
		t.Fatalf("initiator: reader %v projector %v, want a reader and no projector", initiator.Engine.opts.ColdCopy != nil, initiator.coldCopy != nil)
	}
	coordinator, err := Assemble(stubLog{}, &stubRaw{}, assemblyJetStream{}, AssemblyConfig{Engine: owner, ColdCopy: settings})
	if err != nil {
		t.Fatal(err)
	}
	if coordinator.Engine.opts.ColdCopy == nil || coordinator.coldCopy == nil {
		t.Fatal("coordinator: want both the reader and the projector")
	}
	if got := coordinator.coldCopy.settings; got.BatchSize != DefaultColdCopyBatchSize || got.MaxWait != DefaultColdCopyMaxWait || got.Collection != DefaultColdCopyCollection || !got.TerminalOnly {
		t.Fatalf("projector settings %+v, want the defaults", got)
	}
	if config := coordinator.coldCopy.consumerConfig("UNIT_SAGA_G0"); config.Durable != "UNIT_SAGA_G0-cold-copy" || config.Replicas != 1 || !config.MemoryStorage || config.FilterSubject != "unit.saga.*.ev.done" || config.MaxAckPending < DefaultColdCopyBatchSize {
		t.Fatalf("cold copy durable %+v", config)
	}
	off, err := Assemble(stubLog{}, &stubRaw{}, assemblyJetStream{}, AssemblyConfig{Engine: owner})
	if err != nil {
		t.Fatal(err)
	}
	if off.Engine.opts.ColdCopy != nil || off.coldCopy != nil {
		t.Fatal("cold copy wired while off")
	}
	for name, bad := range map[string]*ColdCopySettings{
		"no client":   {Database: "saga"},
		"no database": {Client: client},
		"batch 1001":  {Client: client, Database: "saga", BatchSize: 1001},
		"wait 11s":    {Client: client, Database: "saga", MaxWait: 11 * time.Second},
	} {
		if _, err := Assemble(stubLog{}, &stubRaw{}, assemblyJetStream{}, AssemblyConfig{Engine: owner, ColdCopy: bad}); err == nil {
			t.Errorf("%s: Assemble accepted", name)
		}
	}
}
