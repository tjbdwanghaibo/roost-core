package nest

import (
	"errors"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// RR-20261006-60（F02-8）：手写 HandlerMeta 时，Rollback 不为 none 就必须显式写 Durability。
// 修前 Durability 零值即 memory，{Rollback: RollbackUndo} 被当成 undo+memory 收下，
// 要到运行期第一次改持久字段才被 RR-41 的 memory 拒绝拦住；修后注册时就报错，
// 错误点出 handler 名并列出可选值。显式写 DurabilityMemory 的照常可用，
// HandlerMeta{}（rollback=none）仍是 memory 快路径。
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
