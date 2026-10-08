package nest

import (
	"context"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// 准入冷目标判定的热路径（RR-20260926-25 / 47）：真实 ManagerAccess + loader，与基线同一调用入口对照。
func BenchmarkAdmissionColdProbe(b *testing.B) {
	manager := entity.NewEntityManager()
	access := entity.NewManagerAccess(manager)
	if _, err := access.ConfigureLoader(&stageLoader{manager: manager}); err != nil {
		b.Fatal(err)
	}
	entity.MustRegisterEntityKindCategory(nestLocalKind, entity.EntityCategory(1))
	loaded := make([]int64, 4)
	for i := range loaded {
		id, err := entity.BuildEntityID(int64(8600+i), nestLocalKind)
		if err != nil {
			b.Fatal(err)
		}
		loaded[i] = id
		if err := manager.TryAdd(newMockEntityWithKind(id, entity.EntityCategory(1), nestLocalKind)); err != nil {
			b.Fatal(err)
		}
	}
	cold, err := entity.BuildEntityID(8699, nestLocalKind)
	if err != nil {
		b.Fatal(err)
	}
	mgr := NewEngine(NestOptionWithGetter(access))
	for _, bc := range []struct {
		name string
		msg  *Msg
		want bool
	}{
		{"single_loaded", &Msg{Type: MsgTypeSingle, Tid: loaded[0]}, false},
		{"multi4_loaded", &Msg{Type: MsgTypeMulti, Tids: loaded}, false},
		{"single_cold", &Msg{Type: MsgTypeSingle, Tid: cold}, true},
	} {
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if got := mgr.declaredTargetsNeedSlowPreparation(bc.msg); got != bc.want {
					b.Fatalf("cold=%v want %v", got, bc.want)
				}
			}
		})
	}
}

// 端到端：生成 sender 默认形态（不带 SendOpt）打到已加载目标，Getter 为真实 ManagerAccess。
func BenchmarkClientRequestManagerAccess(b *testing.B) {
	ResetHandlersForTest()
	b.Cleanup(ResetHandlersForTest)
	manager := entity.NewEntityManager()
	access := entity.NewManagerAccess(manager)
	if _, err := access.ConfigureLoader(&stageLoader{manager: manager}); err != nil {
		b.Fatal(err)
	}
	entity.MustRegisterEntityKindCategory(nestLocalKind, entity.EntityCategory(1))
	id, err := entity.BuildEntityID(8700, nestLocalKind)
	if err != nil {
		b.Fatal(err)
	}
	if err := manager.TryAdd(newMockEntityWithKind(id, entity.EntityCategory(1), nestLocalKind)); err != nil {
		b.Fatal(err)
	}
	engine := NewEngine(NestOptionWithGetter(access), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 1024}, WorkerPoolConfig{}))
	if err := engine.Start(); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
	name := NewHandlerName("benchmark_client_request_access")
	MustRegisterMemoryHandler(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		return int64(1), nil
	})
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := engine.Request(ctx, name, id, nil); err != nil {
			b.Fatal(err)
		}
	}
}
