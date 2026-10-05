package skill

import (
	"fmt"
	"strings"
)

func runSnapshotPass(context *compileContext) {
	context.artifacts.snapshots.reads = make(map[string]AttributeReadPlan)
	attributes := map[string]AttributeCatalogEntry{}
	for _, entry := range context.environment.Gameplay.Attributes.Entries {
		attributes[entry.Key] = entry
	}
	inCallback := readsInsideProcessCallbacks(context.artifacts.ir)
	slot := 0
	context.artifacts.ir.walkValues(func(value valueIR) {
		read, ok := value.(*attributeReadValueIR)
		if !ok {
			return
		}
		entry, ok := attributes[read.attribute]
		if !ok {
			context.addDiagnostic(DiagnosticCapabilityUnknown, read.source.Path+".read_attribute.attribute", "unknown attribute")
			return
		}
		point := snapshotPoint(read.snapshot)
		if point == "" {
			point = snapshotCurrent
		}
		if !containsString(entry.Snapshots, string(point)) {
			context.addDiagnostic(DiagnosticAttributeSnapshotInvalid, read.source.Path+".read_attribute.snapshot", "snapshot point is not allowed for attribute")
			return
		}
		if !snapshotCapturableWhereRead(context, read, point, inCallback[read.source.Path]) {
			return
		}
		context.artifacts.snapshots.reads[read.source.Path] = AttributeReadPlan{Entity: read.entity, Attribute: entry.Handle, Snapshot: point, SnapshotSlot: slot}
		slot++
	})
}

// snapshotCapturableWhereRead 检查缓存型快照点能否在采样点求出这次读取
// （RR-20261005-NC-220）。cast_start / phase_start / process_start 的整个读取（包括实体）
// 在采样点求值、之后读缓存（runtime_eval.go captureSnapshots / shouldCacheSnapshot）：
//   - 采样点上还没有任何流程局部变量，实体不能引用 `$local.*`；
//   - process_start 只在 owned 实体进程启动时采样（process_owned.go
//     captureOwnedProcessSnapshots），求值上下文是脱离施法的进程：只能写在 spawn 的进程
//     回调里。写在 phase 流程里时，有 spawn 就在进程上下文里求 `$input` / `$memory`
//     （ErrProgramInvariant），没有 spawn 就退化成“第一次读到的值”。
//
// cast_start / phase_start 写在进程回调里由 owned entity pass 拒绝（回调读不到施法的快照）。
// current / each_tick / on_hit / on_event 在读取处求值，不受这两条限制。
func snapshotCapturableWhereRead(context *compileContext, read *attributeReadValueIR, point snapshotPoint, insideProcessCallback bool) bool {
	if !shouldCacheSnapshot(point) {
		return true
	}
	capturable := true
	if point == snapshotProcessStart && !insideProcessCallback {
		context.addDiagnostic(DiagnosticAttributeSnapshotInvalid, read.source.Path+".read_attribute.snapshot", "process_start is captured when an owned entity process starts; read it only inside that spawn's process callbacks")
		capturable = false
	}
	walkValue(read.entity, func(value valueIR) {
		if reference, ok := value.(*referenceValueIR); ok && strings.HasPrefix(reference.reference, "$local.") && capturable {
			context.addDiagnostic(DiagnosticAttributeSnapshotInvalid, read.source.Path+".read_attribute.entity", fmt.Sprintf("a %s snapshot is captured before flow locals exist; read %s with snapshot current", point, reference.reference))
			capturable = false
		}
	})
	return capturable
}

// readsInsideProcessCallbacks 返回写在 spawn 进程回调（`on`）里的 read_attribute 源路径。
func readsInsideProcessCallbacks(ir *skillIR) map[string]bool {
	inside := make(map[string]bool)
	ir.walkFlows(func(flow flowIR) {
		effectFlow, ok := flow.(*effectFlowIR)
		if !ok || effectFlow.callbacks == nil {
			return
		}
		callbacks := effectFlow.callbacks
		for _, root := range []flowIR{callbacks.tick, callbacks.hit, callbacks.collision, callbacks.end, callbacks.cancel, callbacks.transition, callbacks.targetLost, callbacks.enter, callbacks.leave} {
			if root == nil {
				continue
			}
			root.walkValues(func(value valueIR) {
				if read, ok := value.(*attributeReadValueIR); ok {
					inside[read.source.Path] = true
				}
			})
		}
	})
	return inside
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
