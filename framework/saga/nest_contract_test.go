package saga

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"sync"
	"testing"
	"time"
)

func TestNestCompletionRejectsForeignSagaRouteBeforeMutation(t *testing.T) {
	for _, topic := range []string{CompletionEffectTopicPrefix + "other", CompletionEffectTopicPrefix, CompletionEffectTopicPrefix + "target.extra"} {
		t.Run(topic, func(t *testing.T) {
			ctx := context.Background()
			store, err := NewMongoStore(mongotest.NewClient(), MongoStoreOptions{Database: "completion_identity"})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.EnsureInfrastructure(ctx); err != nil {
				t.Fatal(err)
			}
			e, err := NewEngine(store, PublishFunc(func(context.Context, Command) error { return nil }), DefaultOptions())
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Register(testDefinition()); err != nil {
				t.Fatal(err)
			}
			client := &startJetStream{}
			if _, err := SubscribeNestCompletions(ctx, client, NestCompletionConsumerConfig{Stream: "EFFECTS", Durable: "completion-identity", EffectPrefix: "roost.effect"}, e); err != nil {
				t.Fatal(err)
			}
			r := waitingSaga(t, store, "target")
			completion := Completion{SagaID: r.ID, CommandID: r.CommandID, IdempotencyKey: r.OperationKey, Success: true, Data: []byte("committed")}
			effect, err := NewCompletionEffect(completion)
			if err != nil {
				t.Fatal(err)
			}
			makeMessage := func(route string) *fnats.JetStreamMsg {
				raw, err := json.Marshal(nestwal.EffectEnvelope{EffectID: effect.ID, Topic: route, Key: effect.Key, Payload: effect.Payload})
				if err != nil {
					t.Fatal(err)
				}
				return &fnats.JetStreamMsg{Data: raw}
			}
			err = client.handler(ctx, makeMessage(topic))
			if !errors.Is(err, ErrInvalidRecord) || !fnats.IsPermanent(err) {
				t.Errorf("foreign route=%q was not refused permanently: %v", topic, err)
			}
			after, err := store.Get(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Version != r.Version || after.Status != StatusWaiting {
				t.Errorf("foreign route mutated saga: version=%d status=%s", after.Version, after.Status)
			}
			recorded, err := store.CompletionRecorded(ctx, completion)
			if err != nil || recorded {
				t.Errorf("foreign route recorded completion: recorded=%t err=%v", recorded, err)
			}
			for i := 0; i < 2; i++ {
				if err := client.handler(ctx, makeMessage(effect.Topic)); err != nil {
					t.Fatal(err)
				}
			}
			after, err = store.Get(ctx, r.ID)
			if err != nil || after.Version != r.Version+1 || after.Status != StatusPending || string(after.Data) != "committed" {
				t.Fatalf("valid recovery/duplicate=%+v err=%v", after, err)
			}
		})
	}
}

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

	// 断言只看“第 0 步的结果被收下”，不看此刻的 Status（2026-10-06 发版前验证）：Complete 收下结果后
	// 立即 kick 协调器，协调器会把第 1 步派发出去、记录重新回到 waiting（等第 1 步）。之前这里断言
	// Status != waiting，压力下（-cpu 1 时协调器 goroutine 常先于测试的 Get 运行）读到
	// {Status:waiting Step:1 CompletedSteps:1} 而偶发失败——产品行为正确，是用例的时序假设。
	stored, err := assembly.Store.Get(context.Background(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status == StatusWaiting && stored.OperationKey == record.OperationKey {
		t.Fatalf("the saga is still waiting on step 0 after its native step reported success: %+v", stored)
	}
	if stored.CompletedSteps != 1 || stored.Step != 1 {
		t.Fatalf("completed steps = %d at step %d, want 1 at step 1: %+v", stored.CompletedSteps, stored.Step, stored)
	}
	recorded, err := assembly.Store.CompletionRecorded(context.Background(), completion)
	if err != nil {
		t.Fatal(err)
	}
	if !recorded {
		t.Fatalf("the native completion left no receipt: %+v", stored)
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
