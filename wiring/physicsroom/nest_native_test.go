//go:build physx && cgo

package physicsroom

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	gamephysics "github.com/tjbdwanghaibo/roost-core/gameplay/physics"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
	domain "github.com/tjbdwanghaibo/roost-core/service/physicsroom"
)

type testEntity struct {
	*entity.EntityBase
	state gamephysics.StateDAO
}

func (e *testEntity) Base() *entity.EntityBase { return e.EntityBase }

type testGetter struct{ e *testEntity }

func (g testGetter) Get(context.Context, int64, entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	return g.e, nil
}
func (g testGetter) GetMany(_ context.Context, ids []int64, _ []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	out := make([]entity.IThreadSafeEntity, len(ids))
	for i := range out {
		out[i] = g.e
	}
	return out, nil
}

var kindOnce sync.Once

func TestRoomCommitsThroughNest(t *testing.T) {
	// 此测试包独占 240；注册仅一次，支持 -count 多轮运行。
	kindOnce.Do(func() {
		if err := entity.RegisterEntityKindCategory(240, 1); err != nil {
			panic(err)
		}
	})
	id, err := entity.BuildEntityID(1, 240)
	if err != nil {
		t.Fatal(err)
	}
	target := &testEntity{EntityBase: entity.NewEntityBase(id, 1, true, 240)}
	engine := nest.NewEngine(nest.NestOptionWithGetter(testGetter{target}), nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 16}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 8}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 8}))
	accepted := make(chan gamephysics.Frame, 1)
	blocked := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer func() { releaseOnce.Do(func() { close(release) }) }()
	name := nest.NewHandlerName("physicsroom.test_commit")
	err = engine.RegisterHandlerWithMeta(name, func(_ []entity.IThreadSafeEntity, args []any, _ ...nest.HandlerOption) (any, error) {
		v := args[0].(FrameCommit)
		if v.Frame.Tick == 2 {
			close(blocked)
			<-release
		}
		if err := target.state.Accept(v.Frame); err != nil {
			return nil, err
		}
		select {
		case accepted <- target.state.Snapshot():
		default:
		}
		return nil, nil
	}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityMemory})
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.Start(); err != nil {
		t.Fatal(err)
	}
	defer engine.Shutdown(context.Background())
	cfg := domain.DefaultConfig(NestCommit(engine, name, func(domain.Ref) (int64, error) { return id, nil }))
	s, err := domain.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown(context.Background())
	defer func() { releaseOnce.Do(func() { close(release) }) }()
	instance, err := s.Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := s.Create(context.Background(), domain.Spec{Instance: instance, ID: "nest", LifetimeSeconds: 5, Bodies: []physx.Body{{ID: 1, Shape: physx.Sphere, Position: physx.Vec3{0, 3, 0}, Dimensions: physx.Vec3{.5}, Mass: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-accepted:
		if frame.Epoch != ref.Epoch || frame.Tick != 1 || len(frame.States) != 1 {
			t.Fatalf("frame=%+v", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("Nest commit not reached")
	}
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("second commit did not block")
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err = s.Destroy(stopCtx, ref)
	stopCancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("destroy returned before handler settled: %v", err)
	}
	releaseOnce.Do(func() { close(release) })
	if err = s.Destroy(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
}

func TestRoomEntityReleasedAfterDestroy(t *testing.T) {
	hostKindOnce.Do(func() {
		if err := entity.RegisterEntityKindCategory(241, 1); err != nil {
			panic(err)
		}
	})
	host, err := NewEntityHost(241, 1, 1, nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 16}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 8}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 8}))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	defer host.Close(ctx)
	cfg := domain.DefaultConfig(host.Commit)
	cfg.Closed = host.Forget
	svc, err := domain.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Shutdown(ctx)
	instance, err := svc.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := svc.Create(ctx, domain.Spec{Instance: instance, ID: "owned", LifetimeSeconds: 5, Bodies: []physx.Body{{ID: 1, Shape: physx.Sphere, Position: physx.Vec3{0, 3, 0}, Dimensions: physx.Vec3{.5}, Mass: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		f, e := host.Snapshot(ctx, ref)
		if e == nil && f.Tick > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no committed entity: %v", e)
		}
		time.Sleep(time.Millisecond)
	}
	if err = svc.Destroy(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, err = host.Snapshot(ctx, ref); !errors.Is(err, domain.ErrMissing) {
		t.Fatalf("room entity retained: %v", err)
	}
}
