package movement

import (
	"errors"
	"strings"
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
	mb    *MapBasic
	move  *Move
	flow  *actionflow.BeatRunner
	ended []endedAction
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

// Remote 托管实体的共享模式不允许挂心跳（维护者 2026-10-21）：
//   - 共享期间带周期落库的进场景返回 ErrHeartbeatRemoteShared，什么都不改；
//   - 共享期间启动需要周期 tick 的动作（MoveTo）：组的心跳挂不上，动作以失败结束、它的移动被停掉；
//   - 运行中进入共享模式、框架停掉了心跳：动作留在 runner 里、移动不推进，Sync 在共享期间报错，回到独占后重新挂上。
//
// 框架在下一拍停心跳由 gameplay/heartbeat 的用例覆盖，这里直接 Stop 登记模拟它的结果；不经 Nest、不在事务里。
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
	if e.move, err = NewMove(e.mb, MoveConfig{}); err != nil {
		t.Fatal(err)
	}
	if e.flow, err = actionflow.NewBeatRunner(actionflow.ActionRunnerConfig{Registry: newRegistry(t), GroupForKind: groupForKind,
		Hooks: actionflow.ActionRunnerHooks{
			OnEnded: func(s actionflow.ActionSnapshot, reason actionflow.ActionReason) {
				r := reason.ToActionResult(actionflow.ActionStatusSuccess)
				e.ended = append(e.ended, endedAction{id: s.ID, kind: s.Kind, status: r.Status, reason: r.Reason})
			},
			OnError: func(error) {},
		}}, actionflow.BeatConfig{Host: host}); err != nil {
		t.Fatal(err)
	}
	moveTo := func(goal spatial.Point) error {
		_, err := e.flow.Runner().Start(actMoveTo, MoveToParam{Path: []spatial.Point{goal}, Speed: 10}, 0, time.Time{})
		return err
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
	if err := moveTo(spatial.Point{X: 10}); err != nil || !e.flow.Reg(groupMove).Active() {
		t.Fatalf("move: %v active %v", err, e.flow.Reg(groupMove).Active())
	}

	// 运行中进入共享模式，框架停掉心跳（模拟）：动作与移动都还在，共享期间 Sync 报错，回到独占后重新挂上。
	transition(t, e, entity.RemoteOwnershipSharing, entity.RemoteOwnershipShared)
	e.flow.Reg(groupMove).Stop()
	if err := e.flow.Sync(); !errors.Is(err, heartbeat.ErrHeartbeatRemoteShared) || e.flow.Reg(groupMove).Active() {
		t.Fatalf("runner sync in shared mode: %v active %v", err, e.flow.Reg(groupMove).Active())
	}
	if !e.move.Moving() || e.flow.Runner().Current(groupMove) == nil || len(e.ended) != 0 {
		t.Fatalf("the move or the action was dropped while the heartbeat was stopped: moving %v ended %+v", e.move.Moving(), e.ended)
	}

	// 共享期间启动新的移动：取代旧的（取消），新的挂不上心跳、以失败结束，它的移动被停掉。
	if err := moveTo(spatial.Point{X: 0, Y: 9}); err != nil {
		t.Fatal(err)
	}
	if len(e.ended) != 2 || e.ended[0].status != actionflow.ActionStatusCanceled || e.ended[1].status != actionflow.ActionStatusFailed ||
		!strings.Contains(e.ended[1].reason, "shared mode") || e.move.Moving() || e.flow.Runner().Current(groupMove) != nil {
		t.Fatalf("move started in shared mode: ended %+v moving %v", e.ended, e.move.Moving())
	}

	// 回到独占：照常挂心跳；之前停掉的组由 Sync 重新挂上。
	transition(t, e, entity.RemoteOwnershipDraining, entity.RemoteOwnershipLocalOwned)
	if err := moveTo(spatial.Point{X: 5}); err != nil || !e.flow.Reg(groupMove).Active() || !e.move.Moving() {
		t.Fatalf("move after leaving shared mode: %v active %v", err, e.flow.Reg(groupMove).Active())
	}
	e.flow.Reg(groupMove).Stop()
	if err := e.flow.Sync(); err != nil || !e.flow.Reg(groupMove).Active() {
		t.Fatalf("runner sync after leaving shared mode: %v active %v", err, e.flow.Reg(groupMove).Active())
	}
}
