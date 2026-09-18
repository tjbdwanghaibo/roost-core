package saga

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
)

// U-0231 · C4 · RR-20260917-07（Wanted-04 转入）：原生 Nest 步骤用
// saga.EmitCompletion 把完成结果放进同一条 Nest 事务，它经 Data Engine 的 outbox
// 变成 `<effect_prefix>.saga.result.<sagaID>`。Assembly.Start 只订了
// `<saga_prefix>.result.>`（普通完成）与 `<effect_prefix>.saga.start`（启动），
// 原生完成不匹配任何默认消费者——saga 一直 waiting，步骤按超时重发，
// inbox 判重放回执不重跑业务，协调器还是收不到。承诺：默认装配就有人消费它。

// recordingJetStream remembers every subscription's filter subject and lets a
// test deliver a message to the handler that claimed it.
type recordingJetStream struct {
	mu       sync.Mutex
	handlers map[string]fnats.JetStreamHandler
	configs  []fnats.JetStreamConsumerConfig
}

func newRecordingJetStream() *recordingJetStream {
	return &recordingJetStream{handlers: map[string]fnats.JetStreamHandler{}}
}

func (j *recordingJetStream) EnsureStream(context.Context, fnats.JetStreamConfig) error { return nil }
func (j *recordingJetStream) Publish(context.Context, string, []byte, fnats.JetStreamPublishOptions) (fnats.JetStreamPublishAck, error) {
	return fnats.JetStreamPublishAck{}, nil
}
func (j *recordingJetStream) Subscribe(_ context.Context, config fnats.JetStreamConsumerConfig, handler fnats.JetStreamHandler) (fnats.IJetStreamSubscription, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.handlers[config.FilterSubject] = handler
	j.configs = append(j.configs, config)
	return &recordedSubscription{closed: make(chan struct{})}, nil
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

func (j *recordingJetStream) subjects() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]string, 0, len(j.handlers))
	for subject := range j.handlers {
		out = append(out, subject)
	}
	return out
}

func (j *recordingJetStream) deliver(t *testing.T, subject string, data []byte) error {
	t.Helper()
	j.mu.Lock()
	handler, ok := j.handlers[subject]
	j.mu.Unlock()
	if !ok {
		t.Fatalf("no consumer claimed %q; subscribed subjects: %v", subject, j.subjects())
	}
	return handler(context.Background(), &fnats.JetStreamMsg{Subject: subject, Data: data})
}

// waitingSaga puts a rally saga into the state a native step answers: the
// first step's command is out and the coordinator is waiting for its result.
func waitingSaga(t *testing.T, store *MongoStore, id string) Record {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	record := Record{
		ID: id, Type: "rally", DefinitionVersion: 1, BusinessKey: id,
		Status: StatusWaiting, Phase: PhaseForward, Step: 0, CompletedSteps: 0, Attempt: 1, Version: 1,
		OperationKey: operationKey(id, PhaseForward, 0), CommandID: commandID(operationKey(id, PhaseForward, 0), 0, 1),
		NextRunAt: now.Add(time.Minute), CreatedAt: now, UpdatedAt: now,
	}
	if err := record.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestAssemblyConsumesNativeNestCompletionEffects(t *testing.T) {
	jetStream := newRecordingJetStream()
	cfg := AssemblyConfig{
		Store:       MongoStoreOptions{Database: "saga"},
		Engine:      DefaultOptions(),
		Prefix:      "roost.saga",
		Stream:      fnats.JetStreamConfig{Name: "ROOST_SAGA"},
		Completions: CompletionConsumerConfig{Stream: "ROOST_SAGA", Durable: "roost-saga-coordinator", SubjectPrefix: "roost.saga"},
		Starts:      NestStartConsumerConfig{Stream: "ROOST_EFFECTS", Durable: "roost-saga-start", EffectPrefix: "roost.effect"},
	}
	assembly, err := Assemble(mongotest.NewClient(), jetStream, cfg, testDefinition())
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if err := assembly.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = assembly.Stop(stopCtx)
	})

	// The effect a native step commits inside its Nest transaction.
	record := waitingSaga(t, assembly.Store, "native-1")
	completion := Completion{CommandID: record.CommandID, IdempotencyKey: record.OperationKey, SagaID: record.ID, Success: true}
	effect, err := NewCompletionEffect(completion)
	if err != nil {
		t.Fatalf("completion effect: %v", err)
	}
	envelope, err := json.Marshal(nestwal.EffectEnvelope{TransactionID: "tx-1", EffectID: effect.ID, Topic: effect.Topic, Key: effect.Key, Payload: effect.Payload})
	if err != nil {
		t.Fatal(err)
	}
	// The outbox publishes it under the effect prefix, exactly as it does
	// for a start intent.
	subject := "roost.effect." + effect.Topic
	if err := jetStream.deliver(t, "roost.effect.saga.result.>", envelope); err != nil {
		t.Fatalf("delivering %s to the native completion consumer: %v", subject, err)
	}

	stored, err := assembly.Store.Get(context.Background(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status == StatusWaiting {
		t.Fatalf("the saga is still waiting after its native step reported success: %+v", stored)
	}
	if stored.CompletedSteps != 1 {
		t.Fatalf("completed steps = %d, want 1", stored.CompletedSteps)
	}
}

// The two existing consumers must keep their subjects: this adds a third, it
// does not move the others.
func TestAssemblyKeepsItsExistingConsumerSubjects(t *testing.T) {
	jetStream := newRecordingJetStream()
	cfg := AssemblyConfig{
		Store:       MongoStoreOptions{Database: "saga"},
		Engine:      DefaultOptions(),
		Prefix:      "roost.saga",
		Stream:      fnats.JetStreamConfig{Name: "ROOST_SAGA"},
		Completions: CompletionConsumerConfig{Stream: "ROOST_SAGA", Durable: "roost-saga-coordinator", SubjectPrefix: "roost.saga"},
		Starts:      NestStartConsumerConfig{Stream: "ROOST_EFFECTS", Durable: "roost-saga-start", EffectPrefix: "roost.effect"},
	}
	assembly, err := Assemble(mongotest.NewClient(), jetStream, cfg, testDefinition())
	if err != nil {
		t.Fatal(err)
	}
	if err := assembly.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = assembly.Stop(context.Background()) })

	want := map[string]bool{
		"roost.saga.result.>":        false,
		"roost.effect.saga.start":    false,
		"roost.effect.saga.result.>": false,
	}
	for _, subject := range jetStream.subjects() {
		if _, ok := want[subject]; ok {
			want[subject] = true
		}
	}
	for subject, found := range want {
		if !found {
			t.Errorf("no consumer for %q; subscribed: %v", subject, jetStream.subjects())
		}
	}
	// Every consumer needs its own durable, or two of them fight over one
	// cursor.
	durables := map[string]string{}
	for _, config := range jetStream.configs {
		if previous, clash := durables[config.Durable]; clash {
			t.Errorf("durable %q is shared by %q and %q", config.Durable, previous, config.FilterSubject)
		}
		durables[config.Durable] = config.FilterSubject
	}
}
