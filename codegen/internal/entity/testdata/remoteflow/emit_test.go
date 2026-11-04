package remoteflow

// Remote 实体 handler 里 Emit / EmitStart 的真实依赖端到端（联盟服务方案 §1.6、阶段 1 ④）：正式装配（ManagerAccess 作 Remote
// loader、文件 WAL、Mongo 投影与 outbox、真实 Redis / NATS）上，Remote 实体的 handler 改实体并 Emit 一个业务 effect、
// EmitStart 一个 saga 启动意图：
//   - effect 与 Remote 写在同一条提交记录里投影（投影被拦住时两者都不可见，放行后一起出现），outbox 各发布一次；
//   - 声明 memory 的 handler 因 Emit 升到 strict，同样成立；
//   - handler 业务失败时 Remote 写与 effect 都不提交，之后的写照常。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/saga"
)

// effectRecorder 是 outbox 的发布端替身；accept 关着时像 noEffects 一样拒绝。
type effectRecorder struct {
	accept atomic.Bool
	mu     sync.Mutex
	items  []engine.OutboxItem
}

func (r *effectRecorder) Publish(_ context.Context, item engine.OutboxItem) error {
	if !r.accept.Load() {
		return errors.New("unexpected effect")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = append(r.items, item)
	return nil
}

// byKey 返回已发布的、Key 为 key 的 effect。
func (r *effectRecorder) byKey(key string) []engine.OutboxItem {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []engine.OutboxItem
	for _, item := range r.items {
		if item.Effect.Key == key {
			out = append(out, item)
		}
	}
	return out
}

const (
	emitEffectTopic = "remoteflow.vault_changed"
	emitSagaType    = "remoteflow.vault_saga"
)

// emitMode 是 remote_emit_* 的参数：tag 是 effect 与 saga 业务键，fail 让 handler 在 Emit 之后以业务错误结束。
type emitMode struct {
	tag  string
	fail bool
}

var errEmitBusiness = errors.New("remoteflow: business failed after Emit")

func TestGeneratedRemoteHandlerEmitsEffectsWithTheRemoteWrite(t *testing.T) {
	for _, declared := range []nest.DurabilityPolicy{nest.DurabilityStrict, nest.DurabilityMemory} {
		t.Run(declared.String(), func(t *testing.T) {
			ctx, mongo, redis, database := rejectTestEnv(t)
			name := nest.NewHandlerName("remote_emit_" + declared.String())
			rig := newRejectRig(t, ctx, mongo, redis, database, nest.DurabilityStrict, nil, func(rig *rejectRig) {
				rig.scheduler.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
					mode := params[0].(emitMode)
					vault := es[0].(*SyncedVault)
					vault.balance.SetValue(vault.balance.GetValue() + 1)
					vault.items.SetValue(vault.items.GetValue() + 1)
					if err := nest.Emit(nest.Effect{ID: "fx:" + mode.tag, Topic: emitEffectTopic, Key: mode.tag, Payload: []byte(mode.tag)}); err != nil {
						return nil, err
					}
					if err := saga.EmitStart(saga.StartRequest{Type: emitSagaType, DefinitionVersion: 1, BusinessKey: mode.tag, Data: []byte(mode.tag)}); err != nil {
						return nil, err
					}
					if mode.fail {
						return nil, errEmitBusiness
					}
					return nil, nil
				}, nest.HandlerMeta{Rollback: nest.RollbackState, Durability: declared})
			})
			rig.effects.accept.Store(true)
			id := rig.seed(t, 9800)
			if err := rig.request(rig.ctx, id); err != nil { // 第一笔普通写：权威里有了这个实体
				t.Fatal(err)
			}
			rig.assertNoPending(t)
			baseVersion, baseBalance, _, _ := rig.stored(t, id)

			// 投影拦在 Remote 写所在的提交记录上：那条记录同时带着两个 effect；拦住期间 Mongo 与 outbox 都看不到。
			gate := &rig.projector.gate
			gate.arm(id, faultHold)
			t.Cleanup(gate.open)
			replied := make(chan error, 1)
			go func() {
				_, err := rig.scheduler.Request(rig.ctx, name, id, nest.Params{emitMode{tag: "ok-" + declared.String()}})
				replied <- err
			}()
			<-gate.held
			if got := rig.projector.heldEffects.Load(); got != 2 {
				t.Fatalf("the commit record holding the Remote write carries %d effects, want 2 (Emit and EmitStart in the same record)", got)
			}
			if version, _, _, _ := rig.stored(t, id); version != baseVersion {
				t.Fatalf("Mongo version %d while the projection is held, want %d", version, baseVersion)
			}
			if got := rig.effects.byKey("ok-" + declared.String()); len(got) != 0 {
				t.Fatalf("effects published before the projection: %v", got)
			}
			gate.open()
			if err := <-replied; err != nil {
				t.Fatalf("Remote handler with Emit / EmitStart: %v", err)
			}
			rig.assertNoPending(t)
			if version, balance, _, _ := rig.stored(t, id); version != baseVersion+1 || balance != baseBalance+1 {
				t.Fatalf("Mongo version=%d balance=%d, want %d/%d", version, balance, baseVersion+1, baseBalance+1)
			}
			eventually(t, "both effects published", func() bool { return len(rig.effects.byKey("ok-"+declared.String())) == 2 })
			assertEmitted(t, rig.effects.byKey("ok-"+declared.String()), "ok-"+declared.String())

			// 业务失败：Remote 写与 effect 都不提交。
			_, err := rig.scheduler.Request(rig.ctx, name, id, nest.Params{emitMode{tag: "fail-" + declared.String(), fail: true}})
			if !errors.Is(err, errEmitBusiness) {
				t.Fatalf("failing handler replied %v, want the business error", err)
			}
			rig.assertNoPending(t)
			if version, balance, _, _ := rig.stored(t, id); version != baseVersion+1 || balance != baseBalance+1 {
				t.Fatalf("Mongo after the failed handler version=%d balance=%d, want unchanged %d/%d", version, balance, baseVersion+1, baseBalance+1)
			}
			// 之后的写照常；它的 effect 发布之后，失败那笔仍然一个都没有（outbox 按提交顺序认领，漏进 outbox 的会先出现）。
			if _, err := rig.scheduler.Request(rig.ctx, name, id, nest.Params{emitMode{tag: "after-" + declared.String()}}); err != nil {
				t.Fatalf("write after the failed handler: %v", err)
			}
			rig.assertNoPending(t)
			eventually(t, "effects of the next write published", func() bool { return len(rig.effects.byKey("after-"+declared.String())) == 2 })
			if leaked := rig.effects.byKey("fail-" + declared.String()); len(leaked) != 0 {
				t.Fatalf("a rolled-back handler's effects were published: %v", leaked)
			}
			if got := rig.effects.byKey("ok-" + declared.String()); len(got) != 2 {
				t.Fatalf("effects of the first write published %d times, want exactly 2 items", len(got))
			}
			select {
			case err := <-rig.fatal:
				t.Fatalf("runtime fatal: %v", err)
			default:
			}
		})
	}
}

// assertEmitted 核对一笔写发布的两个 effect：业务 effect 原样，saga 启动意图能解码出原请求。
func assertEmitted(t *testing.T, items []engine.OutboxItem, tag string) {
	t.Helper()
	var business, start int
	for _, item := range items {
		switch item.Effect.Topic {
		case emitEffectTopic:
			business++
			if item.Effect.ID != "fx:"+tag || string(item.Effect.Payload) != tag {
				t.Errorf("business effect = %+v, want id fx:%s payload %s", item.Effect, tag, tag)
			}
		case saga.StartEffectTopic:
			start++
			request, err := saga.DecodeStartEffect(item.Effect.Payload)
			if err != nil || request.Type != emitSagaType || request.BusinessKey != tag || string(request.Data) != tag {
				t.Errorf("saga start = %+v, err=%v; want type %s key %s", request, err, emitSagaType, tag)
			}
		default:
			t.Errorf("unexpected effect topic %q", item.Effect.Topic)
		}
	}
	if business != 1 || start != 1 {
		t.Fatalf("published business=%d start=%d, want one of each: %s", business, start, fmt.Sprint(items))
	}
}
