package saga

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
	"testing"
	"time"
)

func TestStartIdentityCompatibilityAndForeignIntents(t *testing.T) {
	for _, name := range []string{"canonical", "explicit_auto_id", "data", "deadline", "version", "id", "legacy_pristine", "legacy_progressed"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			client := mongotest.NewClient()
			store, err := NewMongoStore(client, MongoStoreOptions{Database: "identity_compat"})
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
			d := testDefinition()
			if err := e.Register(d); err != nil {
				t.Fatal(err)
			}
			d.Version = 2
			if err := e.Register(d); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond)
			request := StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "compat", DeadlineAt: deadline}
			if name != "explicit_auto_id" {
				request.ID = "compat"
			}
			r, err := e.StartSaga(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			if len(r.StartDigest) != 64 {
				t.Fatalf("no original digest: %q", r.StartDigest)
			}
			var raw bson.M
			if err := store.sagas().FindOne(ctx, bson.M{"_id": r.ID}, &raw); err != nil {
				t.Fatal(err)
			}
			if raw["start_digest"] != r.StartDigest {
				t.Fatalf("digest not in BSON: %v", raw["start_digest"])
			}
			if name == "legacy_pristine" || name == "legacy_progressed" {
				delete(raw, "start_digest")
				if name == "legacy_progressed" {
					raw["version"] = int64(2)
					raw["completed_steps"] = int32(1)
					raw["step"] = int32(1)
				}
				if _, err := store.sagas().ReplaceOne(ctx, bson.M{"_id": r.ID}, raw); err != nil {
					t.Fatal(err)
				}
				old, err := store.Get(ctx, r.ID)
				if err != nil || old.StartDigest != "" {
					t.Fatalf("legacy decode=%+v err=%v", old, err)
				}
			}
			wantConflict := false
			switch name {
			case "canonical":
				request.ID = " compat "
				request.BusinessKey = " compat "
				request.Data = []byte{}
				request.Now = time.Now().Add(time.Minute)
				request.DeadlineAt = deadline.In(time.FixedZone("test", 8*60*60)).Add(400 * time.Microsecond)
			case "explicit_auto_id":
				request.ID = r.ID
			case "data":
				request.Data = []byte("different")
				wantConflict = true
			case "deadline":
				request.DeadlineAt = deadline.Add(time.Second)
				wantConflict = true
			case "version":
				request.DefinitionVersion = 2
				wantConflict = true
			case "id":
				request.ID = "other"
				wantConflict = true
			case "legacy_progressed":
				wantConflict = true
			}
			after, err := e.StartSaga(ctx, request)
			if wantConflict {
				if !errors.Is(err, ErrIdentityConflict) {
					t.Fatalf("foreign/unverifiable intent accepted: %v", err)
				}
			} else if err != nil || after.ID != r.ID {
				t.Fatalf("legal duplicate=%+v err=%v", after, err)
			}
			listed, err := store.List(ctx, Query{Limit: 10})
			if err != nil || len(listed) != 1 {
				t.Fatalf("duplicate created another saga: %+v err=%v", listed, err)
			}
			if name != "legacy_pristine" && name != "legacy_progressed" && listed[0].StartDigest != r.StartDigest {
				t.Fatal("List lost original digest")
			}
		})
	}
}

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

func TestHandleNestStartTermsDeterministicStartRefusalsAndAlarms(t *testing.T) {
	envelope := func(t *testing.T, request StartRequest) *fnats.JetStreamMsg {
		t.Helper()
		// 不经 NewStartEffect：模拟另一个进程（不托管这个类型的协调器，或配置不同）发出的启动意图。
		payload, err := json.Marshal(startEffectPayload{Version: WireVersion, Start: request})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(nestwal.EffectEnvelope{EffectID: "saga-start:" + request.BusinessKey, Topic: StartEffectTopic, Payload: payload})
		if err != nil {
			t.Fatal(err)
		}
		return &fnats.JetStreamMsg{Subject: "roost.effect.saga.start", Data: raw}
	}
	t.Run("data over MaxPayloadBytes", func(t *testing.T) {
		engine, _ := idleEngine(t)
		before := counterValue("saga.start.rejected_total")
		err := handleNestStart(context.Background(), envelope(t, StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "s7-big", Data: make([]byte, DefaultOptions().MaxPayloadBytes+1)}), engine)
		if !errors.Is(err, ErrInvalidRecord) || !fnats.IsPermanent(err) {
			t.Fatalf("oversized start data: err=%v permanent=%v, want a permanent ErrInvalidRecord (Term, not nak until MaxDeliver)", err, fnats.IsPermanent(err))
		}
		if grown := counterValue("saga.start.rejected_total") - before; grown != 1 {
			t.Errorf("saga.start.rejected_total grew by %d, want 1", grown)
		}
	})
	t.Run("identity conflict", func(t *testing.T) {
		engine, _ := idleEngine(t)
		if _, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "s7-dup", Data: []byte("one")}); err != nil {
			t.Fatal(err)
		}
		before := counterValue("saga.start.rejected_total")
		err := handleNestStart(context.Background(), envelope(t, StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "s7-dup", Data: []byte("two")}), engine)
		if !errors.Is(err, ErrIdentityConflict) || !fnats.IsPermanent(err) {
			t.Fatalf("conflicting start intent: err=%v permanent=%v, want a permanent ErrIdentityConflict", err, fnats.IsPermanent(err))
		}
		if grown := counterValue("saga.start.rejected_total") - before; grown != 1 {
			t.Errorf("saga.start.rejected_total grew by %d, want 1", grown)
		}
	})
	// 守卫：定义缺失是滚动发布中的暂时状态（与结果消费者同一口径），仍按可重试错误 nak。
	t.Run("definition missing stays retryable", func(t *testing.T) {
		engine, _ := idleEngine(t)
		err := handleNestStart(context.Background(), envelope(t, StartRequest{Type: "rally", DefinitionVersion: 9, BusinessKey: "s7-rolling"}), engine)
		if !errors.Is(err, ErrDefinitionMissing) || fnats.IsPermanent(err) {
			t.Fatalf("start for a definition this coordinator does not have yet: err=%v permanent=%v, want a retryable ErrDefinitionMissing", err, fnats.IsPermanent(err))
		}
	})
}

// EmitStart 在 Nest 事务里按本进程协调器为这个类型注册的 MaxPayloadBytes 拒绝；事务不提交。
func TestEmitStartRefusesDataTheCoordinatorWouldRefuse(t *testing.T) {
	engine, _ := idleEngine(t) // 注册 rally/1，MaxPayloadBytes 缺省 64 KiB
	_ = engine
	limit := DefaultOptions().MaxPayloadBytes
	if _, err := NewStartEffect(StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "s7-limit", Data: make([]byte, limit)}); err != nil {
		t.Fatalf("data at the limit refused: %v", err)
	}
	committer := &stepFenceCommitter{}
	_, err := corenest.RunIsolatedTransaction(context.Background(), committer, "start-big", func() (any, error) {
		return nil, EmitStart(StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "s7-emit", Data: make([]byte, limit+1)})
	})
	if !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("EmitStart with data over the coordinator's MaxPayloadBytes inside a Nest transaction = %v, want ErrInvalidRecord before commit", err)
	}
	if len(committer.record.Effects) != 0 {
		t.Fatalf("the refused start intent was committed: %+v", committer.record.Effects)
	}
	// 本进程没有注册的类型只按线上的硬上限（4 MiB）校验，余下由启动消费者 Term + 告警兜底。
	if _, err := NewStartEffect(StartRequest{Type: "elsewhere", DefinitionVersion: 1, BusinessKey: "s7-remote", Data: make([]byte, limit+1)}); err != nil {
		t.Fatalf("type without a coordinator in this process refused below the wire cap: %v", err)
	}
}
