package saga

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
)

// RR-20261006-43（F06-S7）：Nest 启动消费者对确定性错误 Term + 告警；EmitStart 在 Nest 事务里按协调器的上限拒绝。
//
// 承诺：一条重投永远不会被接收的启动意图（Data 超过 saga.max_payload_bytes、同一 (type, business_key) 已有另一份意图）
// 直接 Term，记 saga.start.rejected_total{saga_type,reason} 与点名 saga 的 ERROR；业务在 Nest 事务里 EmitStart 一份本进程协调器
// 必定拒绝的 Data 时，事务内就拿到 ErrInvalidRecord（实体修改随事务回滚），而不是提交之后启动意图被静默丢掉。
//
// 旧行为：handleNestStart 只把信封 / 解码错误标 Permanent，StartSaga 的 ErrInvalidRecord / ErrIdentityConflict 原样返回，
// 按 nak 退避重投约 8.7 天（MaxDeliver 25000、退避封顶 30s）才 Term，saga 从未创建，没有指标；EmitStart 只按 4 MiB 校验，
// 与缺省 64 KiB 的 MaxPayloadBytes 不一致。
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
