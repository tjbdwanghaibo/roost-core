package movement

import (
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/gameplay/actionflow"
	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// Remote 托管、可进入共享模式的测试实体种类。
const kindRemoteMover entity.EntityKind = 242

func init() {
	entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
		Category:     entity.EntityCategoryRemote,
		Kind:         kindRemoteMover,
		RemotePolicy: entity.RemotePolicyManaged,
		NoPersist:    true,
		Builder:      func(*entity.EntityCreateParam) (entity.IThreadSafeEntity, error) { return nil, nil },
		Lifetime:     entity.EntityLifetimeRemoteManaged,
	})
}

type remoteMover struct {
	*entity.RemoteEntityBase
	mb   *MapBasic
	move *Move
	flow *actionflow.BeatRunner
}

func (e *remoteMover) Base() *entity.EntityBase { return &e.RemoteEntityBase.EntityBase }
func (e *remoteMover) MoveComponent() *Move     { return e.move }

func transition(t *testing.T, e *remoteMover, states ...entity.RemoteOwnershipState) {
	t.Helper()
	for _, s := range states {
		if err := e.TransitionRemoteOwnership(s); err != nil {
			t.Fatal(err)
		}
	}
}

// Remote 托管实体进入共享模式（不允许挂心跳，维护者 2026-10-21）：共享期间开始移动、带周期落库的进场景、runner 需要
// tick 时都返回 ErrHeartbeatRemoteShared 且什么都不改；运行中被框架停掉心跳后移动状态保留、不推进，回到独占后
// Move.Resume / BeatRunner.Sync 重新挂上，接着走。（框架在下一拍停心跳由 gameplay/heartbeat 的用例覆盖，这里直接
// Stop 登记模拟它的结果。）
func TestSharedRemoteEntityRefusesHeartbeatsAndResumesAfterLeavingSharedMode(t *testing.T) {
	d := newUnitDriver()
	id, err := entity.BuildEntityID(1, kindRemoteMover)
	if err != nil {
		t.Fatal(err)
	}
	e := &remoteMover{RemoteEntityBase: entity.NewRemoteEntityBase(id, entity.EntityCategoryRemote, true, kindRemoteMover)}
	transition(t, e, entity.RemoteOwnershipLocalOwned)
	host := d.NewHost(e)
	if e.mb, err = NewMapBasic(e, host, MapConfig{Fields: &MemFields{}, Save: func(int64, spatial.Point, Facing) error { return nil }, SaveEvery: time.Second}); err != nil {
		t.Fatal(err)
	}
	if e.move, err = NewMove(host, e.mb, MoveConfig{}); err != nil {
		t.Fatal(err)
	}
	registry := newRegistry(t)
	if e.flow, err = actionflow.NewBeatRunner(actionflow.ActionRunnerConfig{Registry: registry, GroupForKind: groupForKind,
		Hooks: actionflow.ActionRunnerHooks{OnError: func(error) {}}}, actionflow.BeatConfig{Host: host}); err != nil {
		t.Fatal(err)
	}

	// 共享模式下进场景（带周期落库）被拒，什么都没改。
	transition(t, e, entity.RemoteOwnershipSharing, entity.RemoteOwnershipShared)
	if err := e.mb.Enter(1, &spaceLog{}, spatial.Point{}); !errors.Is(err, heartbeat.ErrHeartbeatRemoteShared) || e.mb.InScene() {
		t.Fatalf("enter in shared mode: %v in scene %v", err, e.mb.InScene())
	}
	transition(t, e, entity.RemoteOwnershipDraining, entity.RemoteOwnershipLocalOwned)
	if err := e.mb.Enter(1, &spaceLog{}, spatial.Point{}); err != nil {
		t.Fatal(err)
	}
	first, err := e.move.MoveTo([]spatial.Point{{X: 10}}, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	tick := &tickAction{limit: 100}
	if _, err := e.flow.Runner().Start(actTick, tick, 0, time.Time{}); err != nil || !e.flow.Reg().Active() {
		t.Fatalf("tick action: %v active %v", err, e.flow.Reg().Active())
	}

	// 运行中进入共享模式，框架停掉全部心跳（模拟）。
	transition(t, e, entity.RemoteOwnershipSharing, entity.RemoteOwnershipShared)
	e.move.Reg().Stop()
	e.flow.Reg().Stop()
	if _, err := e.move.MoveTo([]spatial.Point{{X: 0, Y: 9}}, 10, nil); !errors.Is(err, heartbeat.ErrHeartbeatRemoteShared) || e.move.Token() != first {
		t.Fatalf("move in shared mode: %v token %d (want the first move %d kept)", err, e.move.Token(), first)
	}
	if err := e.move.Resume(); !errors.Is(err, heartbeat.ErrHeartbeatRemoteShared) || e.move.Reg().Active() {
		t.Fatalf("resume in shared mode: %v active %v", err, e.move.Reg().Active())
	}
	if err := e.flow.Sync(); !errors.Is(err, heartbeat.ErrHeartbeatRemoteShared) || e.flow.Reg().Active() {
		t.Fatalf("runner sync in shared mode: %v active %v", err, e.flow.Reg().Active())
	}
	if !e.move.Moving() || e.flow.Runner().Current(groupWork) == nil {
		t.Fatal("the move or the action was dropped while the heartbeat was stopped")
	}

	transition(t, e, entity.RemoteOwnershipDraining, entity.RemoteOwnershipLocalOwned)
	if err := e.move.Resume(); err != nil || !e.move.Reg().Active() {
		t.Fatalf("resume after leaving shared mode: %v active %v", err, e.move.Reg().Active())
	}
	if err := e.flow.Sync(); err != nil || !e.flow.Reg().Active() {
		t.Fatalf("runner sync after leaving shared mode: %v active %v", err, e.flow.Reg().Active())
	}
	_ = e.move.beat(beatOf(time.Second))
	if e.mb.Pos() != (spatial.Point{X: 10}) || e.move.Moving() {
		t.Fatalf("after resuming: pos %+v moving %v", e.mb.Pos(), e.move.Moving())
	}
}
