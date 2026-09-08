package saga

import (
	"context"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
)

type drainTestSubscription struct {
	drained chan struct{}
	closed  chan struct{}
}

func (s *drainTestSubscription) Stop()                   {}
func (s *drainTestSubscription) Drain()                  { close(s.drained) }
func (s *drainTestSubscription) Closed() <-chan struct{} { return s.closed }

// Moved from the kit Mod with the lifecycle (P3b): Stop must not tear the
// engine down while a consumer is still delivering.
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

// P3b: Assemble is what the Mod publishes as the saga capability. It must
// refuse missing clients, build store / transport / engine, register the
// definitions it is given, and report an unstarted assembly as not running.
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
