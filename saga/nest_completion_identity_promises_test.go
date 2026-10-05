package saga

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
)

// RR-20261005-NC-40：完成信封的 Saga 路由必须与 payload 一致，拒绝后无状态/回执副作用。
// 合法重投仍须完成并幂等；底层是正式 MongoStore + mongotest，不冒认真实 broker。
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
