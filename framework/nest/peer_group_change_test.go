package nest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/internal/entitypermit"
)

// 对外契约套件另测 strict WAL 原子性。这里直接见证 Try 预留的物理组锁、同一 worker 归还、
// 以及其他入口不能替换 handler 的显式目标。
func TestPeerGroupChangeWorkerReservationAndAuthority(t *testing.T) {
	g := newMockGetter()
	var members []*mockEntity
	for i := 0; i < 2; i++ {
		id := mustBuildCastID(t, int64(8901+i), entity.EntityCategory(1), nestLocalKind)
		e := newMockEntity(id, entity.EntityCategory(1))
		g.Add(e)
		g.groups.Add(e)
		members = append(members, e)
	}
	a, b := members[0], members[1]
	mgr := NewEngine(NestOptionWithGetter(g), NestOptionWithTransactionCommitter(newPipelinedTestCommitter(true)))
	const group = 8951
	register := func(name string, meta HandlerMeta, fn func([]entity.IThreadSafeEntity) error) HandlerName {
		h := NewHandlerName("peer_change_" + name)
		mgr.MustRegisterHandlerWithMeta(h, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) { return nil, fn(es) }, meta)
		return h
	}
	join := register("join", HandlerMeta{}, func(es []entity.IThreadSafeEntity) error {
		// 业务切片可以被写。原消息和锁内冻结的实例才决定 subject。
		es[0] = b
		if err := JoinEntityGroup(group); err != nil {
			return err
		}
		if a.Base().GroupLockID() != 0 || a.Base().GroupTransitionPending() {
			return errors.New("staged join leaked")
		}
		if !errors.Is(JoinEntityGroup(group), ErrPeerGroupChangeBusy) {
			return errors.New("double stage accepted")
		}
		if !errors.Is(mgr.RequestMoveEntityLockGroup(b.ID(), group), ErrIndependentGroupRequestInHandler) {
			return errors.New("business handler escaped via top-level request")
		}
		if err := entity.WithGuardScope(entitypermit.System(), "business-created", func(*entity.GuardScope) error {
			if !errors.Is(LeaveEntityGroup(), ErrPeerGroupChangeUnavailable) {
				return errors.New("fresh guard acquired authority")
			}
			return nil
		}); err != nil {
			return err
		}
		if _, err := RunIsolatedTransaction(nil, &recordingCommitter{}, "nested", func() (any, error) {
			if !errors.Is(JoinEntityGroup(group+1), ErrPeerGroupChangeUnavailable) {
				return nil, errors.New("isolated transaction inherited subject")
			}
			return nil, nil
		}); err != nil {
			return err
		}
		result := make(chan error, 1)
		go func() {
			if release, ok := mgr.tryLockEntityLockGroups([]int64{group}); ok {
				release()
				result <- errors.New("another worker acquired reserved group")
				return
			}
			if !errors.Is(mgr.RequestJoinEntityLockGroup(a.ID(), group+1), ErrEntityGroupTransitionPending) {
				result <- errors.New("independent request stole member")
				return
			}
			result <- nil
		}()
		return <-result
	})
	multi := register("multi", HandlerMeta{}, func(es []entity.IThreadSafeEntity) error { es[1] = es[0]; return JoinEntityGroup(group) })
	leavePanic := register("leave_panic", HandlerMeta{}, func([]entity.IThreadSafeEntity) error {
		if err := LeaveEntityGroup(); err != nil {
			return err
		}
		if a.Base().GroupLockID() != group || a.Base().GroupTransitionPending() {
			return errors.New("leave leaked before success")
		}
		panic("peer-change rollback witness")
	})
	leave := register("leave", HandlerMeta{}, func([]entity.IThreadSafeEntity) error { return LeaveEntityGroup() })
	pipeline := register("pipeline", HandlerMeta{Rollback: RollbackState, Durability: DurabilityPipelined}, func([]entity.IThreadSafeEntity) error { return JoinEntityGroup(group) })
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Shutdown(nil) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	call := func(name HandlerName) error { _, err := mgr.Request(ctx, name, a.ID(), nil); return err }
	if err := JoinEntityGroup(group); !errors.Is(err, ErrPeerGroupChangeUnavailable) {
		t.Fatalf("outside Nest: %v", err)
	}
	if err := call(pipeline); !errors.Is(err, ErrPeerGroupChangeUnavailable) {
		t.Fatalf("pipelined: %v", err)
	}
	if _, err := mgr.RequestMulti(ctx, multi, []int64{a.ID(), b.ID()}, nil); !errors.Is(err, ErrPeerGroupChangeUnavailable) {
		t.Fatalf("multi: %v", err)
	}
	// 另一个 worker 正持目标组锁；不可在手里有实体锁时等待。
	release, ok := mgr.tryLockEntityLockGroups([]int64{group})
	if !ok {
		t.Fatal("setup group busy")
	}
	err := call(join)
	release()
	if !errors.Is(err, ErrPeerGroupChangeBusy) {
		t.Fatalf("conflict: %v", err)
	}
	if err := call(join); err != nil {
		t.Fatal(err)
	}
	if g.GetGroupEntity(group, a.ID()) != a || b.Base().GroupLockID() != 0 {
		t.Fatal("not the original explicit instance")
	}
	if err := call(leavePanic); err == nil || g.GetGroupEntity(group, a.ID()) != a {
		t.Fatalf("panic published leave: %v", err)
	}
	if err := call(leave); err != nil {
		t.Fatal(err)
	}
	if g.GetGroupEntity(group, a.ID()) != nil || a.Base().GroupLockID() != 0 {
		t.Fatal("confirmed leave was not published")
	}
}

func TestPeerGroupChangeRejectsBoundDomainAndIndependentRemote(t *testing.T) {
	f := newCompNestFixture(t, nil)
	h := f.on("peer_change_attached", HandlerMeta{}, func([]entity.IThreadSafeEntity) error { return JoinEntityGroup(8941) })
	f.start(t)
	for _, e := range []entity.IThreadSafeEntity{f.p, f.a} {
		if _, err := f.Request(f.ctx, h, e.ID(), nil); !errors.Is(err, ErrPeerGroupUnsupported) {
			t.Errorf("attached %T: %v", e, err)
		}
		if err := f.RequestJoinEntityLockGroup(e.ID(), 8941); !errors.Is(err, ErrPeerGroupUnsupported) || e.Base().GroupTransitionPending() {
			t.Errorf("independent attached %T: %v", e, err)
		}
	}
	getter := newMockGetter()
	mgr := NewEngine(NestOptionWithGetter(getter))
	id := mustBuildCastID(t, 8908, entity.EntityCategoryRemote, nestRemoteCapableKind)
	e := newMockEntityWithKind(id, entity.EntityCategoryRemote, nestRemoteCapableKind)
	getter.Add(e)
	getter.groups.Add(e)
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Shutdown(nil) })
	if err := mgr.RequestMoveEntityLockGroup(id, 8941); !errors.Is(err, ErrPeerGroupUnsupported) || e.Base().GroupTransitionPending() {
		t.Fatalf("independent remote: %v", err)
	}
}

func TestPeerGroupChangeOldQueuedInstanceCannotPublishOrAbortReplacement(t *testing.T) {
	g := newMockGetter()
	mgr := NewEngine(NestOptionWithGetter(g))
	id := mustBuildCastID(t, 8909, entity.EntityCategory(1), nestLocalKind)
	old, next := newMockEntity(id, entity.EntityCategory(1)), newMockEntity(id, entity.EntityCategory(1))
	g.Add(next)
	g.groups.Add(next)
	const target = 8939
	state := entity.EntityGroupTransitionJoin
	if !next.Base().BeginGroupTransition(entitypermit.System(), state, target) {
		t.Fatal("set up pending replacement")
	}
	req := &GroupTransitionRequest{EntityID: id, instance: old, State: state, TargetGroupID: target}
	if err := entity.WithGuardScope(entitypermit.System(), "old-queue", func(*entity.GuardScope) error { _, err := mgr.groupTransitionDispatch(req); return err }); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("stale async result: %v", err)
	}
	mgr.abortGroupTransition(req)
	if !next.Base().GroupTransitionPending() || g.GetGroupEntity(target, id) != nil {
		t.Fatal("stale asynchronous request touched replacement")
	}
	if err := g.groups.UpdateEntityGroup(entitypermit.System(), old, target); !errors.Is(err, entity.ErrEntityNotManaged) {
		t.Fatal(fmt.Errorf("stale manager update: %w", err))
	}
}
