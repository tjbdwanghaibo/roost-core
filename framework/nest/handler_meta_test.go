// handler 注册与策略：Rollback / Durability 的解析与组合校验、默认策略、实例级注册、热更替换。

package nest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/hotcode"
)

// 手写 HandlerMeta 时，Rollback 不为 none 就必须显式写 Durability：注册时报 ErrDurabilityUnset，错误点出 handler 名
// 并列出可选值；显式写 DurabilityMemory 的照常可用，HandlerMeta{}（rollback=none）仍是 memory 快路径。
// 历史：RR-20261006-60。
func TestHandlerMetaRollbackRequiresExplicitDurability(t *testing.T) {
	t.Cleanup(ResetHandlersForTest)
	noop := func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) { return nil, nil }

	for _, rollback := range []RollbackPolicy{RollbackState, RollbackUndo} {
		name := NewHandlerName("rr60_missing_durability_" + rollback.String())
		err := RegisterHandlerWithMeta(name, noop, HandlerMeta{Rollback: rollback})
		if err == nil {
			t.Fatalf("rollback=%s without Durability was accepted at registration; want error", rollback)
		}
		if !errors.Is(err, ErrDurabilityUnset) {
			t.Fatalf("rollback=%s error %v, want ErrDurabilityUnset", rollback, err)
		}
		for _, want := range []string{name.String(), "DurabilityMemory", "DurabilityAsync", "DurabilityStrict", "DurabilityPipelined"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("rollback=%s error %q does not mention %q", rollback, err, want)
			}
		}
		mgr := &NestMgr{handlers: make(map[HandlerName]handlerEntry)}
		if err := mgr.RegisterHandlerWithMeta(name, noop, HandlerMeta{Rollback: rollback}); err == nil {
			t.Fatalf("instance registration rollback=%s without Durability was accepted", rollback)
		}
	}

	explicit := NewHandlerName("rr60_explicit_memory_undo")
	if err := RegisterHandlerWithMeta(explicit, noop, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityMemory}); err != nil {
		t.Fatalf("explicit undo+memory rejected: %v", err)
	}
	if entry, ok := GetHandlerEntry(explicit); !ok || entry.meta.Durability != DurabilityMemory || entry.meta.Rollback != RollbackUndo {
		t.Fatalf("explicit undo+memory entry = %+v, %v", entry.meta, ok)
	}

	fast := NewHandlerName("rr60_none_fast_path")
	if err := RegisterMemoryHandler(fast, noop); err != nil {
		t.Fatalf("RegisterMemoryHandler: %v", err)
	}
	if entry, ok := GetHandlerEntry(fast); !ok || entry.meta.Rollback != RollbackNone || entry.meta.Durability != DurabilityMemory {
		t.Fatalf("HandlerMeta{} entry = %+v, %v; want rollback=none durability=memory (fast path)", entry.meta, ok)
	}
}

func TestDefaultHandlerRegistrationUsesDurableAsyncPolicy(t *testing.T) {
	ResetHandlersForTest()
	defer ResetHandlersForTest()
	name := NewHandlerName("production_default_durability")
	if err := RegisterHandler(name, func(nilEntities []entity.IThreadSafeEntity, nilParams []any, options ...HandlerOption) (any, error) {
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	entry, ok := GetHandlerEntry(name)
	if !ok || entry.meta.Rollback != RollbackState || entry.meta.Durability != DurabilityAsync {
		t.Fatalf("unsafe default handler meta: %+v", entry.meta)
	}
}

func TestTransactionPolicyParsingRejectsLegacyAndUnknownValues(t *testing.T) {
	if _, err := ParseRollbackPolicy("dirty"); err == nil {
		t.Fatal("legacy rollback=dirty must be rejected")
	}
	if policy, err := ParseRollbackPolicy("undo"); err != nil || policy != RollbackUndo {
		t.Fatalf("parse undo = %v, %v", policy, err)
	}
	if _, err := ParseDurabilityPolicy("best_effort"); err == nil {
		t.Fatal("unknown durability must be rejected")
	}
}

func TestHandlerHotcodePatch(t *testing.T) {
	ResetHandlersForTest()
	defer ResetHandlersForTest()

	name := NewHandlerName("test_hotcode_handler")
	if err := RegisterMemoryHandler(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		return "origin", nil
	}); err != nil {
		t.Fatalf("RegisterHandler: %v", err)
	}
	if err := hotcode.Replace(HandlerPatchName(name), BaseHandler(func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		return "patched", nil
	}), hotcode.Meta{Version: "test"}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	handler := GetHandler(name)
	got, err := handler(nil, nil)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if got != "patched" {
		t.Fatalf("handler = %v, want patched", got)
	}
}

func TestInstanceScopedHandlersDoNotCollideAcrossEngines(t *testing.T) {
	getter := newMockGetter()
	id := mustBuildCastID(t, 321, entity.EntityCategory(1), nestLocalKind)
	getter.Add(&rollbackTestEntity{
		EntityBase: entity.NewEntityBase(id, entity.EntityCategory(1), false, nestLocalKind),
		dao:        &rollbackTestDao{id: id},
	})
	name := NewHandlerName("test_instance_scoped")
	newEngine := func(reply string) *NestMgr {
		engine := NewEngine(
			NestOptionWithGetter(getter),
			NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}),
			NestOptionWithTickDuration(100*time.Millisecond),
		)
		engine.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
			return reply, nil
		}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityMemory})
		if err := engine.Start(); err != nil {
			t.Fatal(err)
		}
		return engine
	}
	// The same name registers on two engines without touching global state.
	first := newEngine("first")
	defer func() { _ = first.Shutdown(context.Background()) }()
	second := newEngine("second")
	defer func() { _ = second.Shutdown(context.Background()) }()
	if ret, err := first.Request(context.Background(), name, id, nil); err != nil || ret != "first" {
		t.Fatalf("first engine: %v %v", ret, err)
	}
	if ret, err := second.Request(context.Background(), name, id, nil); err != nil || ret != "second" {
		t.Fatalf("second engine: %v %v", ret, err)
	}
	// Post-start registration is refused, and duplicates are refused pre-start.
	if err := first.RegisterHandlerWithMeta(NewHandlerName("late"), nil, HandlerMeta{}); err == nil {
		t.Fatal("post-start registration accepted")
	}
	idle := NewEngine(NestOptionWithGetter(getter), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}), NestOptionWithTickDuration(time.Second))
	if err := idle.RegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) { return nil, nil }, HandlerMeta{}); err != nil {
		t.Fatal(err)
	}
	if err := idle.RegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) { return nil, nil }, HandlerMeta{}); err == nil {
		t.Fatal("duplicate instance handler accepted")
	}
}

// RollbackNone 只能配 memory 持久级别，所以本地不能回滚的 handler 只有 memory 快路径与带 Remote 批次的 memory handler 两种形态。
func TestRollbackNoneRequiresMemoryDurability(t *testing.T) {
	for _, d := range []DurabilityPolicy{DurabilityStrict, DurabilityPipelined} {
		mgr := NewEngine(NestOptionWithGetter(newMockGetter()))
		err := mgr.RegisterHandlerWithMeta(NewHandlerName(fmt.Sprintf("rr73_none_%d", d)), func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) { return nil, nil }, HandlerMeta{Rollback: RollbackNone, Durability: d})
		if !errors.Is(err, ErrRollbackUnsupported) {
			t.Fatalf("RollbackNone + durability %d: register err=%v, want ErrRollbackUnsupported", d, err)
		}
	}
}

func TestParseDurabilityPolicyPipelined(t *testing.T) {
	policy, err := ParseDurabilityPolicy("pipelined")
	if err != nil || policy != DurabilityPipelined {
		t.Fatalf("policy=%v err=%v", policy, err)
	}
	if policy.String() != "pipelined" {
		t.Fatalf("String()=%q", policy.String())
	}
}
