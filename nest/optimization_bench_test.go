package nest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

func BenchmarkSlowDispatchWatch(b *testing.B) {
	msg := &Msg{Name: "bench_watch"}
	b.ReportAllocs()
	for b.Loop() {
		watch := startSlowDispatchTraceWatch(msg, time.Now())
		watch.stop(time.Microsecond, "ok")
	}
}

func BenchmarkTickCallbackSnapshot(b *testing.B) {
	resetTickCallbacksForTest()
	b.Cleanup(resetTickCallbacksForTest)
	for i := range 8 {
		MustRegisterTickCallback(NewTickCallbackName(fmt.Sprint(i)), func(TickMsg) {})
	}
	ticker := NewTicker(time.Hour)
	b.ReportAllocs()
	for b.Loop() {
		ticker.doTick()
	}
}

func BenchmarkClientRequestMulti(b *testing.B) {
	benchmarkClientRequest(b, 4, false)
}

func BenchmarkClientRequestStageMetrics(b *testing.B) {
	for _, enabled := range []bool{false, true} {
		b.Run(fmt.Sprintf("enabled=%t", enabled), func(b *testing.B) {
			benchmarkClientRequest(b, 1, enabled)
		})
	}
}

func benchmarkClientRequest(b *testing.B, entityCount int, stages bool) {
	entity.MustRegisterEntityKindCategory(nestLocalKind, entity.EntityCategory(1))
	getter := newMockGetter()
	var ids []int64
	for i := range entityCount {
		id, err := entity.BuildEntityID(8300+int64(i), nestLocalKind)
		if err != nil {
			b.Fatal(err)
		}
		getter.Add(newMockEntity(id, entity.EntityCategory(1)))
		ids = append(ids, id)
	}
	engine := NewEngine(NestOptionWithGetter(getter), NestOptionWithWorkerNumAndMsgCap(1, 0, 1024), NestOptionWithStageMetrics(stages))
	name := NewHandlerName("bench_multi")
	engine.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) { return 1, nil }, HandlerMeta{})
	if err := engine.Start(); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
	b.ReportAllocs()
	for b.Loop() {
		if _, err := engine.RequestMulti(context.Background(), name, ids, nil); err != nil {
			b.Fatal(err)
		}
	}
}
