package saga

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
)

// RR-20261005-NC-39：启动身份属于原始意图，不能随步骤 Data 或 Resume 截止时间变化。
// 同时走内存与正式 MongoStore；持久测试每次换 Engine/Store，避免只验证返回的内存副本。
func TestStartIdentitySurvivesProgressAndResume(t *testing.T) {
	for _, backend := range []string{"memory", "mongo"} {
		for _, stage := range []string{"progress", "completed", "resume_deadline", "resume_clear"} {
			t.Run(backend+"/"+stage, func(t *testing.T) {
				ctx := context.Background()
				var store Store
				client := mongotest.NewClient()
				if backend == "memory" {
					store = newMemoryStore()
				} else {
					mongoStore, err := NewMongoStore(client, MongoStoreOptions{Database: "start_identity"})
					if err != nil {
						t.Fatal(err)
					}
					if err := mongoStore.EnsureInfrastructure(ctx); err != nil {
						t.Fatal(err)
					}
					store = mongoStore
				}
				newEngine := func() *Engine {
					if backend == "mongo" {
						var err error
						store, err = NewMongoStore(client, MongoStoreOptions{Database: "start_identity"})
						if err != nil {
							t.Fatal(err)
						}
					}
					e, err := NewEngine(store, PublishFunc(func(context.Context, Command) error { return nil }), DefaultOptions())
					if err != nil {
						t.Fatal(err)
					}
					if err := e.Register(testDefinition()); err != nil {
						t.Fatal(err)
					}
					return e
				}
				e := newEngine()
				now := time.Now().UTC().Truncate(time.Millisecond)
				request := StartRequest{ID: "identity", Type: "rally", DefinitionVersion: 1, BusinessKey: "identity", Data: []byte("initial"), DeadlineAt: now.Add(time.Hour), Now: now}
				r, err := e.StartSaga(ctx, request)
				if err != nil {
					t.Fatal(err)
				}
				complete := func(success bool) Record {
					t.Helper()
					if err := e.processClaimed(ctx, r, now); err != nil {
						t.Fatal(err)
					}
					waiting, err := store.Get(ctx, r.ID)
					if err != nil {
						t.Fatal(err)
					}
					result := Completion{SagaID: r.ID, CommandID: waiting.CommandID, IdempotencyKey: waiting.OperationKey, Success: success, Data: []byte("current")}
					if !success {
						result.Error = "repair required"
					}
					after, err := e.Complete(ctx, result)
					if err != nil {
						t.Fatal(err)
					}
					return after
				}
				switch stage {
				case "progress", "completed":
					r = complete(true)
					if stage == "completed" {
						r = complete(true)
					}
				case "resume_deadline", "resume_clear":
					r = complete(false)
					resume := ResumeRequest{ID: r.ID, Now: now}
					if stage == "resume_clear" {
						resume.ClearDeadline = true
					} else {
						resume.DeadlineAt = now.Add(2 * time.Hour)
					}
					r, err = e.Resume(ctx, resume)
					if err != nil {
						t.Fatal(err)
					}
				}
				e = newEngine()
				before, err := store.Get(ctx, r.ID)
				if err != nil {
					t.Fatal(err)
				}
				// WAL 原信封重新投递：该公开 consumer 必须接受，且不得倒退业务状态。
				effect, err := NewStartEffect(request)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(nestwal.EffectEnvelope{EffectID: effect.ID, Topic: effect.Topic, Key: effect.Key, Payload: effect.Payload})
				if err != nil {
					t.Fatal(err)
				}
				if err := handleNestStart(ctx, &fnats.JetStreamMsg{Data: raw}, e); err != nil {
					t.Errorf("original start redelivery rejected after %s: %v", stage, err)
				}
				foreign := request
				foreign.Data = bytes.Clone(before.Data)
				foreign.DeadlineAt = before.DeadlineAt
				if _, err := e.StartSaga(ctx, foreign); !errors.Is(err, ErrIdentityConflict) {
					t.Errorf("runtime state accepted as a new start intent after %s: %v", stage, err)
				}
				after, err := store.Get(ctx, r.ID)
				if err != nil {
					t.Fatal(err)
				}
				if after.Version != before.Version || after.Status != before.Status || !bytes.Equal(after.Data, before.Data) || !after.DeadlineAt.Equal(before.DeadlineAt) {
					t.Fatalf("redelivery changed progress: before=%+v after=%+v", before, after)
				}
			})
		}
	}
}
