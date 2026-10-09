package nestgame

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

type spatialSink struct{}

func (spatialSink) Push(context.Context, entitysync.SessionID, []byte) error { return nil }

// 正式生成 DAO + 实体 + Nest Guard/undo；Spatial 组件本身完全手写。
// 功能测试无网络依赖，不是性能负载，也不修改生成器。
func TestSpatialComponentNestCommitAndRollback(t *testing.T) {
	RegisterEntity()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	access := entity.NewManagerAccess(entity.NewEntityManager())
	id, err := entity.BuildEntityID(1, kindUnit)
	if err != nil {
		t.Fatal(err)
	}
	value, err := access.Create(&entity.EntityCreateParam{IsCreate: true, Kind: kindUnit, Id: id})
	if err != nil {
		t.Fatal(err)
	}
	unit := value.(*Unit)
	syncer, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: spatialSink{}, Mode: entitysync.ModeOnChange})
	if err != nil {
		t.Fatal(err)
	}
	defer syncer.Close(ctx)
	unit.EnableSync(entity.EntitySyncCreateParam{Enabled: true, EntityID: id, Namespace: "unit", Packer: entity.SubjectSyncPackFunc{
		Snapshot: func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
			return entity.TakeFrozenSyncPayload(1, unit.state.MarshalSync(dataengine.AllFields)), nil
		},
		Delta: func(_ entity.SyncProfile, mask uint64) (entity.FrozenSyncPayload, error) {
			return entity.TakeFrozenSyncPayload(1, unit.state.MarshalSync(mask)), nil
		},
	}})
	if err := syncer.Register(unit.Sync()); err != nil {
		t.Fatal(err)
	}
	if err := syncer.OpenSession(42); err != nil {
		t.Fatal(err)
	}
	interest, err := policy.NewInterest(policy.InterestConfig{Manager: syncer, BatchSize: 1, AOI: policy.AOIConfig{Bounds: spatial.Rect{Max: spatial.Point{X: 1000, Y: 1000}}, BlockSize: 100, EnterRadius: 50, LeaveRadius: 70}})
	if err != nil {
		t.Fatal(err)
	}
	defer interest.Close()
	if err := interest.Enter(42, spatial.Point{X: 10, Y: 10}); err != nil {
		t.Fatal(err)
	}
	if err := interest.Show(id, spatial.Point{X: 500, Y: 10}); err != nil {
		t.Fatal(err)
	}
	component, err := policy.NewSpatialComponent(unit, interest, false, func(at spatial.Point) { unit.state.SetX(at.X) })
	if err != nil {
		t.Fatal(err)
	}
	if err := component.Move(spatial.Point{X: 600, Y: 10}); !errors.Is(err, policy.ErrSpatialScope) {
		t.Fatalf("outside scope: %v", err)
	}
	engine := nest.NewEngine(nest.NestOptionWithGetter(access), nest.NestOptionWithEntitySync(syncer), nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 8}, nest.WorkerPoolConfig{}))
	rejected := errors.New("rollback movement")
	name := nest.NewHandlerName("spatial.component.move")
	engine.MustRegisterHandlerWithMeta(name, func(_ []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
		// 同一handler的两次移动不能被BatchSize=1切开。
		if err := component.Move(spatial.Point{X: 550, Y: 10}); err != nil {
			return nil, err
		}
		if err := component.MoveWithObservedBlocks(spatial.Point{X: 600, Y: 10}, []int64{interest.BlockAt(spatial.Point{X: 10, Y: 10})}); err != nil {
			return nil, err
		}
		// DAO 已改，空间事实仍不能提前到达观察者。
		if err := syncer.Flush(ctx); err != nil {
			return nil, err
		}
		if slices.Contains(syncer.Subscribers(id), entitysync.SessionID(42)) {
			return nil, errors.New("uncommitted coverage escaped")
		}
		if params[0].(bool) {
			return nil, rejected
		}
		return nil, nil
	}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityMemory})
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	defer engine.Shutdown(ctx)
	if _, err := engine.Request(ctx, name, id, nest.NewParams(true)); !errors.Is(err, rejected) {
		t.Fatalf("rollback=%v", err)
	}
	if err := syncer.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if unit.state.GetX() != 0 || len(interest.ObservedBlocks(id)) != 0 {
		t.Fatal("DAO/coverage rollback diverged")
	}
	if _, err := engine.Request(ctx, name, id, nest.NewParams(false)); err != nil {
		t.Fatal(err)
	}
	if err := syncer.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if unit.state.GetX() != 600 || !slices.Contains(syncer.Subscribers(id), entitysync.SessionID(42)) {
		t.Fatal("committed DAO/coverage not published")
	}
}
