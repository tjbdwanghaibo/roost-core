package nest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/internal/entitypermit"
)

func TestPeerLegacyContinuationCommitsBeforeReplyTransfer(t *testing.T) {
	g := newMockGetter()
	id := mustBuildCastID(t, 8432, entity.EntityCategory(1), nestLocalKind)
	e := newMockEntity(id, entity.EntityCategory(1))
	g.Add(e)
	g.groups.Add(e)
	reject := errors.New("business declined after staging")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	mgr := NewEngine(NestOptionWithGetter(g), NestOptionWithTransactionCommitter(&recordingCommitter{err: reject}))
	start := NewHandlerName("peer_continuation_fail")
	after := NewHandlerName("peer_rejected_must_not_run")
	wasRun := make(chan struct{}, 1)
	mgr.MustRegisterHandlerWithMeta(start, func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		if err := mgr.RequestJoinEntityLockGroup(id, 92, GroupTransitionWithContinuation(after, nil)); err != nil {
			return nil, err
		}
		if e.Base().GroupLockID() != 0 {
			return nil, errors.New("exposed unconfirmed membership")
		}
		return nil, reject
	}, HandlerMeta{})
	mgr.MustRegisterHandlerWithMeta(after, func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		wasRun <- struct{}{}
		return nil, nil
	}, HandlerMeta{})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(ctx)
	if _, err := mgr.Request(ctx, start, id, nil); !errors.Is(err, reject) {
		t.Fatalf("original caller must receive failure: %v", err)
	}
	if e.Base().GroupLockID() != 0 {
		t.Fatal("rejected membership published")
	}
	select {
	case <-wasRun:
		t.Fatal("uncommitted continuation ran")
	default:
	}
}

func TestPeerGuardRejectsEmptyCapabilityWithoutDroppingFrameworkLock(t *testing.T) {
	id := mustBuildCastID(t, 8433, entity.EntityCategory(1), nestLocalKind)
	e := newMockEntity(id, entity.EntityCategory(1))
	s, closeScope := entity.NewGuardScope(entitypermit.System(), "frame")
	defer closeScope()
	g := s.Guard()
	if !g.RequireEntity(entitypermit.System(), e) {
		t.Fatal("setup")
	}
	var zero entitypermit.Token
	for _, op := range []func(){
		func() { g.ReleaseEntityInstance(zero, e) }, func() { g.ReleaseEntity(zero, id) },
		func() { g.ReleaseAll(zero) }, func() { entity.EntityGuardRelease(zero, g) },
		func() { g.RequireEntity(zero, e) }, func() { g.TryRequireEntity(zero, e) },
		func() { entity.NewGuardScope(zero, "business") },
		func() { entity.PushCompanionScope(zero, nil, nil) },
		func() { g.SwapCreatedEntityCapturer(zero, nil) },
		func() { g.BindCreatedEntityCapturer(zero, nil) },
	} {
		func() {
			defer func() {
				if v := recover(); v != entity.ErrEntityLockScope {
					t.Fatalf("manual guard operation did not reject: %v", v)
				}
			}()
			op()
		}()
	}
	done := make(chan bool, 1)
	go func() {
		m := physicalTestLock(e)
		ok := m.TryLock()
		if ok {
			m.Unlock()
		}
		done <- ok
	}()
	if <-done {
		t.Fatal("rejected call released the actual framework lock")
	}
}
