package nest

import (
	"errors"
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

type dispatchLifetimeEntity struct {
	*entity.EntityBase
	touches   int
	untouches int
}

func (e *dispatchLifetimeEntity) Base() *entity.EntityBase { return e.EntityBase }

func newDispatchLifetimeEntity(t *testing.T, unique int64) *dispatchLifetimeEntity {
	t.Helper()
	id := mustBuildCastID(t, unique, entity.EntityCategory(1), nestLocalKind)
	return &dispatchLifetimeEntity{EntityBase: entity.NewEntityBase(id, entity.EntityCategory(1), false, nestLocalKind)}
}

func (e *dispatchLifetimeEntity) Touch() bool {
	if !e.EntityBase.Touch() {
		return false
	}
	e.touches++
	return true
}

func (e *dispatchLifetimeEntity) UnTouch() {
	e.untouches++
	e.EntityBase.UnTouch()
}

func TestBroadcastReleasePanicBalancesTouchesAndContinues(t *testing.T) {
	a, b := newDispatchLifetimeEntity(t, 9960), newDispatchLifetimeEntity(t, 9961)
	manager := entity.NewEntityManager()
	if err := manager.TryAdd(a); err != nil {
		t.Fatal(err)
	}
	defer manager.RegisterOnEntityRelease(func(entity.IThreadSafeEntity) { panic("release hook") })()
	getter := nilForMissingGetter{present: map[int64]entity.IThreadSafeEntity{a.ID(): a, b.ID(): b}}
	mgr := NewEngine(NestOptionWithGetter(getter))
	name := NewHandlerName("broadcast_release_panic")
	var calls []int64
	mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		calls = append(calls, es[0].ID())
		return nil, nil
	}, HandlerMeta{})
	scope, end := entity.NewGuardScope("broadcast-release")
	defer end()
	var escaped any
	func() {
		defer func() { escaped = recover() }()
		mgr.broadcastDispatch(name.String(), []int64{a.ID(), b.ID()}, nil)
	}()
	if escaped != nil {
		t.Errorf("release panic escaped per-entity boundary: %v", escaped)
	}
	if !slices.Equal(calls, []int64{a.ID(), b.ID()}) {
		t.Errorf("broadcast calls = %v, want both entities", calls)
	}
	for _, e := range []*dispatchLifetimeEntity{a, b} {
		if e.touches != 1 || e.untouches != 1 {
			t.Errorf("entity %d touches/untouches = %d/%d, want 1/1", e.ID(), e.touches, e.untouches)
		}
		if scope.Guard().Guarded(e.GUId()) {
			t.Errorf("entity %d remained locked", e.ID())
		}
	}
}

// 锁顺序可以排序，但业务参数的顺序、重复项和分组中的空位置必须保留。
// handler 即使重写自己的参数切片，框架也必须按原取得的资源完成清理。
func TestDispatchRoutesPreserveArgumentsAndReleaseOnEveryOutcome(t *testing.T) {
	for _, route := range []string{"single", "multi", "groups"} {
		for _, outcome := range []string{"success", "error", "panic"} {
			t.Run(route+"/"+outcome, func(t *testing.T) {
				a, b := newDispatchLifetimeEntity(t, 9971), newDispatchLifetimeEntity(t, 9970)
				missing := mustBuildCastID(t, 9972, entity.EntityCategory(1), nestLocalKind)
				mgr := NewEngine(NestOptionWithGetter(nilForMissingGetter{present: map[int64]entity.IThreadSafeEntity{a.ID(): a, b.ID(): b}}))
				name := NewHandlerName("route_lifecycle")
				failure := errors.New("business failed")
				mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, opts ...HandlerOption) (any, error) {
					want := []entity.IThreadSafeEntity{a}
					if route != "single" {
						want = []entity.IThreadSafeEntity{a, nil, b, a}
					}
					if !slices.Equal(es, want) {
						t.Errorf("handler argument order = %v, want %v", es, want)
					}
					var options HandlerOptionParam
					for _, opt := range opts {
						opt(&options)
					}
					if route == "groups" && (!options.IsGroup || !slices.Equal(options.GroupLen, []int{2, 0, 2})) {
						t.Errorf("group options = %+v", options)
					}
					if route != "groups" && options.IsGroup {
						t.Error("ordinary dispatch received group options")
					}
					clear(es)
					switch outcome {
					case "error":
						return nil, failure
					case "panic":
						panic(failure)
					}
					return "ok", nil
				}, HandlerMeta{})
				scope, end := entity.NewGuardScope("route-lifecycle")
				defer end()
				var ret any
				var err error
				var panicked any
				func() {
					defer func() { panicked = recover() }()
					switch route {
					case "single":
						ret, err = mgr.singleDispatch(name.String(), a.ID(), nil)
					case "multi":
						ret, err = mgr.multiDispatch(name.String(), []int64{a.ID(), missing, b.ID(), a.ID()}, nil)
					case "groups":
						ret, err = mgr.multiGroupDispatch(name.String(), [][]int64{{a.ID(), missing}, {}, {b.ID(), a.ID()}}, nil)
					}
				}()
				if outcome == "success" && (ret != "ok" || err != nil || panicked != nil) {
					t.Fatalf("success = %v/%v/%v", ret, err, panicked)
				}
				if outcome == "error" && (!errors.Is(err, failure) || panicked != nil) {
					t.Fatalf("error = %v/%v", err, panicked)
				}
				if outcome == "panic" && panicked != failure {
					t.Fatalf("panic = %v", panicked)
				}
				wantA, wantB := 2, 1
				if route == "single" {
					wantA, wantB = 1, 0
				}
				if a.touches != wantA || a.untouches != wantA || b.touches != wantB || b.untouches != wantB {
					t.Errorf("unbalanced touches: a=%d/%d, b=%d/%d", a.touches, a.untouches, b.touches, b.untouches)
				}
				if scope.Guard().GuardedCount() != 0 {
					t.Error("dispatch retained entity locks")
				}
			})
		}
	}
}
