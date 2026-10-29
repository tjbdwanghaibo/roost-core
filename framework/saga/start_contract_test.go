package saga

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

// 启动身份：同一 (Type, BusinessKey) 的启动按规范化的启动摘要（类型、业务键、定义版本、Data、毫秒截止）判定“已存在”或身份冲突；
// 业务键与 Data 的规范化（去空白、空 Data 与 nil 相同）、Now 与截止的时区 / 亚毫秒不参与身份。saga ID 恒为 IDOf(type, bk)。
// 历史：TestStartIdentityCompatibilityAndForeignIntents 的 explicit_auto_id / id（StartRequest.ID 删除，选项 ID-A）与
// legacy_pristine / legacy_progressed（没有启动摘要的旧记录，格式不兼容后不再存在）四个子用例随对象删除。
func TestStartIdentityCompatibilityAndForeignIntents(t *testing.T) {
	for _, name := range []string{"canonical", "data", "deadline", "version"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			e := newSimEngine(t, nil)
			d := testDefinition()
			d.Version = 2
			if err := e.e.Register(d); err != nil {
				t.Fatal(err)
			}
			deadline := e.clock.Now().Add(time.Hour).Truncate(time.Millisecond)
			request := StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "compat", DeadlineAt: deadline}
			r, err := e.StartSaga(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			if len(r.StartDigest) != 64 || r.ID != IDOf("rally", "compat") {
				t.Fatalf("no original digest or a foreign id: %+v", r)
			}
			wantConflict := false
			switch name {
			case "canonical":
				request.BusinessKey = " compat "
				request.Data = []byte{}
				request.Now = e.clock.Now().Add(time.Minute)
				request.DeadlineAt = deadline.In(time.FixedZone("test", 8*60*60)).Add(400 * time.Microsecond)
			case "data":
				request.Data = []byte("different")
				wantConflict = true
			case "deadline":
				request.DeadlineAt = deadline.Add(time.Second)
				wantConflict = true
			case "version":
				request.DefinitionVersion = 2
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
			if len(e.p.sagas) != 1 {
				t.Fatalf("duplicate created another saga: %d sagas", len(e.p.sagas))
			}
			if listed := listRecords(e.p.sagas, Query{Limit: 10}); listed[0].StartDigest != r.StartDigest {
				t.Fatal("List lost original digest")
			}
		})
	}
}

// 启动意图的原信封在 saga 推进、完成、Resume 之后重投：中继接受（已存在），不倒退业务状态；拿运行期的 Data / 截止拼出的“启动”
// 是另一份意图（身份冲突）。历史：同名用例在 memory 与 Redis 两种存储上各跑一遍（含重启后从持久状态恢复）；分区的重放与重启
// 由真实用例 TestRealJetStreamCoordinatorKill9Replays 覆盖。
func TestStartIdentitySurvivesProgressAndResume(t *testing.T) {
	for _, stage := range []string{"progress", "completed", "resume_deadline", "resume_clear"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			e := newSimEngine(t, nil)
			now := e.clock.Now().Truncate(time.Millisecond)
			request := StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "identity", Data: []byte("initial"), DeadlineAt: now.Add(time.Hour), Now: now}
			r, err := e.StartSaga(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			complete := func(success bool) Record {
				t.Helper()
				waiting := e.record(r.ID)
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
				if r, err = e.Resume(ctx, resume); err != nil {
					t.Fatal(err)
				}
			}
			before := e.record(r.ID)
			// WAL 原信封重新投递：启动中继必须接受，且不得倒退业务状态。
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
			after := e.record(r.ID)
			if after.Version != before.Version || after.Status != before.Status || !bytes.Equal(after.Data, before.Data) || !after.DeadlineAt.Equal(before.DeadlineAt) {
				t.Fatalf("redelivery changed progress: before=%+v after=%+v", before, after)
			}
		})
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
	// 守卫：定义缺失是滚动发布中的暂时状态，仍按可重试错误 nak；准入关闭与判定未达同样 nak（意图延迟而不是丢失，设计 §7.4）。
	t.Run("definition missing stays retryable", func(t *testing.T) {
		engine, _ := idleEngine(t)
		err := handleNestStart(context.Background(), envelope(t, StartRequest{Type: "rally", DefinitionVersion: 9, BusinessKey: "s7-rolling"}), engine)
		if !errors.Is(err, ErrDefinitionMissing) || fnats.IsPermanent(err) {
			t.Fatalf("start for a definition this coordinator does not have yet: err=%v permanent=%v, want a retryable ErrDefinitionMissing", err, fnats.IsPermanent(err))
		}
	})
	for name, refusal := range map[string]error{"backpressure": ErrBackpressure, "unconfirmed": ErrStartUnconfirmed} {
		t.Run(name+" stays retryable", func(t *testing.T) {
			err := handleNestStart(context.Background(), envelope(t, StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "s7-" + name}), refusingStarter{refusal})
			if !errors.Is(err, refusal) || fnats.IsPermanent(err) {
				t.Fatalf("%s: err=%v permanent=%v, want a retryable error (nak, the intent waits)", name, err, fnats.IsPermanent(err))
			}
		})
	}
}

type refusingStarter struct{ err error }

func (s refusingStarter) StartSaga(context.Context, StartRequest) (Record, error) {
	return Record{}, s.err
}

// startEffectCommitter 记下事务交给 DataEngine 的记录（空记录 Nest 不交给它）。
type startEffectCommitter struct{ record corenest.CommitRecord }

func (c *startEffectCommitter) Commit(_ context.Context, record corenest.CommitRecord) error {
	c.record = record
	return nil
}

// EmitStart 在 Nest 事务里按本进程协调器为这个类型注册的 MaxPayloadBytes 拒绝；事务不提交。
func TestEmitStartRefusesDataTheCoordinatorWouldRefuse(t *testing.T) {
	engine, _ := idleEngine(t) // 注册 rally/1，MaxPayloadBytes 缺省 64 KiB
	_ = engine
	limit := DefaultOptions().MaxPayloadBytes
	if _, err := NewStartEffect(StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "s7-limit", Data: make([]byte, limit)}); err != nil {
		t.Fatalf("data at the limit refused: %v", err)
	}
	committer := &startEffectCommitter{}
	_, err := corenest.RunIsolatedTransaction(context.Background(), committer, "start-big", func() (any, error) {
		return nil, EmitStart(StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "s7-emit", Data: make([]byte, limit+1)})
	})
	if !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("EmitStart with data over the coordinator's MaxPayloadBytes inside a Nest transaction = %v, want ErrInvalidRecord before commit", err)
	}
	if len(committer.record.Effects) != 0 {
		t.Fatalf("the refused start intent was committed: %+v", committer.record.Effects)
	}
	// 本进程没有注册的类型只按线上的硬上限（4 MiB）校验，余下由启动中继 Term + 告警兜底。
	if _, err := NewStartEffect(StartRequest{Type: "elsewhere", DefinitionVersion: 1, BusinessKey: "s7-remote", Data: make([]byte, limit+1)}); err != nil {
		t.Fatalf("type without a coordinator in this process refused below the wire cap: %v", err)
	}
}

// 发起方按业务键查结果：saga ID 由 (Type, BusinessKey) 派生，GetByBusinessKey 就是按派生 ID 问所有者；启动前、或换一个类型时
// 是 ErrNotFound。
func TestEngineFindsTheSagaAnInitiatorStartedByBusinessKey(t *testing.T) {
	ctx := context.Background()
	e := newSimEngine(t, nil)
	if _, err := e.e.GetByBusinessKey(ctx, "rally", "gift-req-7"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("before the start: GetByBusinessKey = %v; want ErrNotFound", err)
	}
	started, err := e.StartSaga(ctx, StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "gift-req-7"})
	if err != nil {
		t.Fatal(err)
	}
	found, err := e.e.GetByBusinessKey(ctx, "rally", "gift-req-7")
	if err != nil {
		t.Fatalf("GetByBusinessKey: %v", err)
	}
	if found.ID != started.ID || found.ID != IDOf("rally", "gift-req-7") || found.BusinessKey != "gift-req-7" || found.Status != started.Status {
		t.Fatalf("GetByBusinessKey = %+v; want the started saga %s", found, started.ID)
	}
	if _, err := e.e.GetByBusinessKey(ctx, "other", "gift-req-7"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another type with the same key: GetByBusinessKey = %v; want ErrNotFound", err)
	}
}
