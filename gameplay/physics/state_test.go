package physics

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
)

// 本测试进程独占 239；实例级 handler 不污染其他引擎。
var registerKind sync.Once

type stateEntity struct {
	*entity.EntityBase
	dao StateDAO
}

func (e *stateEntity) Base() *entity.EntityBase { return e.EntityBase }

type stateGetter struct{ e *stateEntity }

func (g stateGetter) Get(context.Context, int64, entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	return g.e, nil
}
func (g stateGetter) GetMany(_ context.Context, ids []int64, _ []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	out := make([]entity.IThreadSafeEntity, len(ids))
	for i := range out {
		out[i] = g.e
	}
	return out, nil
}
func TestNestRejectRollbackAndNextTick(t *testing.T) {
	registerKind.Do(func() { entity.MustRegisterEntityKindCategory(239, 1) })
	id, e := entity.BuildEntityID(1, 239)
	if e != nil {
		t.Fatal(e)
	}
	target := &stateEntity{EntityBase: entity.NewEntityBase(id, 1, true, 239)}
	engine := nest.NewEngine(nest.NestOptionWithGetter(stateGetter{target}), nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 8}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 8}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 8}))
	name := nest.NewHandlerName("physics.test.accept")
	err := engine.RegisterHandlerWithMeta(name, func(_ []entity.IThreadSafeEntity, args []any, _ ...nest.HandlerOption) (any, error) {
		if err := target.dao.Accept(args[0].(Frame)); err != nil {
			return nil, err
		}
		if args[1].(bool) {
			return nil, errors.New("reject after update")
		}
		return target.dao.Snapshot(), nil
	}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityMemory})
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.Start(); err != nil {
		t.Fatal(err)
	}
	defer engine.Shutdown(context.Background())
	f := Frame{Epoch: 1, Tick: 1, States: []physx.State{{ID: 1, Rotation: physx.Quat{0, 0, 0, 1}}}}
	if _, err = engine.Request(context.Background(), name, id, nest.NewParams(f, true)); err == nil {
		t.Fatal("expected rejection")
	}
	if target.dao.frame.Tick != 0 {
		t.Fatal("DAO did not roll back")
	}
	if _, err = engine.Request(context.Background(), name, id, nest.NewParams(f, false)); err != nil {
		t.Fatal(err)
	}
	f.Tick = 3
	if _, err = engine.Request(context.Background(), name, id, nest.NewParams(f, false)); err == nil {
		t.Fatal("gap accepted")
	}
	if target.dao.frame.Tick != 1 {
		t.Fatal("gap changed DAO")
	}
	f.Tick = 2
	if _, err = engine.Request(context.Background(), name, id, nest.NewParams(f, false)); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotUsesConfiguredChunkSizeAndNegativeCoordinates(t *testing.T) {
	s, err := Snapshot(Frame{Epoch: 1, States: []physx.State{{ID: 1, Position: physx.Vec3{-1, 2, 12}, Rotation: physx.Quat{0, 0, 0, 1}}}}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if s.Objects[0].Chunk.X != -1 || s.Objects[0].Chunk.Z != 1 {
		t.Fatalf("wrong chunk: %+v", s.Objects[0].Chunk)
	}
	if _, err = Snapshot(Frame{}, 0); err == nil {
		t.Fatal("zero chunk size accepted")
	}
}
