package physicsroom

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	gamephysics "github.com/tjbdwanghaibo/roost-core/gameplay/physics"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
	domain "github.com/tjbdwanghaibo/roost-core/service/physicsroom"
)

var hostKindOnce sync.Once

func TestEntityHostIsolationAndCleanup(t *testing.T) {
	// 241 与本包原生提交测试使用的 240 分开，支持重复运行。
	hostKindOnce.Do(func() {
		if err := entity.RegisterEntityKindCategory(241, 1); err != nil {
			panic(err)
		}
	})
	h, err := NewEntityHost(241, 1, 2, nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 2, QueueCap: 16}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 8}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 8}))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	defer h.Close(ctx)
	a, b := domain.Ref{ID: "a", Epoch: 1}, domain.Ref{ID: "b", Epoch: 2}
	f := gamephysics.Frame{Epoch: 1, Tick: 1, States: []physx.State{{ID: 1, Position: physx.Vec3{3, 0, 0}, Rotation: physx.Quat{0, 0, 0, 1}}}}
	if err = h.Commit(ctx, a, f); err != nil {
		t.Fatal(err)
	}
	f.States[0].Position[0] = 99 // 提交后调用者修改不能影响 DAO。
	f.Epoch = 2
	if err = h.Commit(ctx, b, f); err != nil {
		t.Fatal(err)
	}
	got, err := h.Snapshot(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if got.States[0].Position[0] != 3 {
		t.Fatalf("frame alias: %+v", got)
	}
	got.States[0].Position[0] = 50
	again, err := h.Snapshot(ctx, a)
	if err != nil || again.States[0].Position[0] != 3 {
		t.Fatalf("snapshot alias: %+v %v", again, err)
	}
	f.Epoch = 1
	f.Tick = 3
	if err = h.Commit(ctx, a, f); err == nil {
		t.Fatal("out of order accepted")
	}
	h.Forget(a)
	if _, err = h.Snapshot(ctx, a); !errors.Is(err, domain.ErrMissing) {
		t.Fatal(err)
	}
	got, err = h.Snapshot(ctx, b)
	if err != nil || got.States[0].Position[0] != 99 {
		t.Fatalf("other room damaged: %+v %v", got, err)
	}
	if err = h.Close(ctx); err != nil {
		t.Fatal(err)
	}
	f.Epoch = b.Epoch
	if err = h.Commit(ctx, b, f); !errors.Is(err, domain.ErrStopping) {
		t.Fatal(err)
	}
}
