package nest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// U-0110 · C2（空洞测试）· nightly gap map `nest` 13/20。
//
// Cast 是处理器里"顺手锁第二个实体"的门：没有守卫作用域、没有正在派发的
// 消息、消息没带 getter、目标 id 为 0、getter 找不到实体、实体的真实分类与
// id 所说的不一致（锁序会反）——每一条都必须拒绝，否则处理器拿着一个没锁
// 或锁反了的实体继续跑。CastTwo / CastThree 对第二、三个目标的类型断言也各
// 自要报 ErrCastTypeMismatch 而不是把零值交出去。DispatchBroadcast 对空 id
// 列表必须以 ErrInvalidMessage 拒绝。

// otherMockEntity is a distinct pointer type so a *mockEntity fails the cast.
type otherMockEntity struct{ *mockEntity }

func TestCastMultiRejectsEveryMissingPrecondition(t *testing.T) {
	withCastGroupFunc(t)
	getter := newMockGetter()
	otherID := mustBuildCastID(t, 310, castOtherCategory, castOtherKind)
	getter.Add(newMockEntityWithKind(otherID, castOtherCategory, castOtherKind))
	target := NewCastTarget(otherID)

	t.Run("dispatch message without guard scope", func(t *testing.T) {
		bindCastGetter(t, getter)
		if _, err := CastMulti(target); !errors.Is(err, ErrCastNoContext) {
			t.Fatalf("err=%v, want ErrCastNoContext", err)
		}
	})
	t.Run("guard scope without dispatch message", func(t *testing.T) {
		_, release := entity.NewGuardScope("cast_test")
		defer release()
		if _, err := CastMulti(target); !errors.Is(err, ErrCastNoContext) {
			t.Fatalf("err=%v, want ErrCastNoContext", err)
		}
	})
	t.Run("dispatch message without getter", func(t *testing.T) {
		release := pushCurrentNestDispatchMsg(&Msg{})
		defer release()
		_, releaseGuard := entity.NewGuardScope("cast_test")
		defer releaseGuard()
		if _, err := CastMulti(target); !errors.Is(err, ErrCastGetterNotSet) {
			t.Fatalf("err=%v, want ErrCastGetterNotSet", err)
		}
	})
	t.Run("zero target id", func(t *testing.T) {
		bindCastGetter(t, getter)
		_, release := entity.NewGuardScope("cast_test")
		defer release()
		_, err := CastMulti(NewCastTarget(0))
		// 门口的守卫给出干净的 "index=0 id=0"；越过它会撞上 NormalizeFullID，
		// 文案带上归一化错误——那是另一条守卫的话。
		if !errors.Is(err, ErrCastInvalidTarget) || !strings.HasSuffix(err.Error(), "index=0 id=0") {
			t.Fatalf("err=%v, want ErrCastInvalidTarget ending in \"index=0 id=0\"", err)
		}
	})
	t.Run("getter has no such entity", func(t *testing.T) {
		bindCastGetter(t, getter)
		_, release := entity.NewGuardScope("cast_test")
		defer release()
		missingID := mustBuildCastID(t, 311, castOtherCategory, castOtherKind)
		_, err := CastMulti(NewCastTarget(missingID))
		if !errors.Is(err, ErrEntityNotFound) || !strings.Contains(err.Error(), "index=0") {
			t.Fatalf("err=%v, want ErrEntityNotFound naming the index", err)
		}
	})
	t.Run("getter hands back an entity whose id contradicts the request", func(t *testing.T) {
		// The requested id says "other" (castable after alliance), but the entity
		// the getter returns carries a player id: locking it after an alliance
		// would invert the lock order. The id check passes; the lock check on the
		// actual entities must refuse.
		liar := &lyingGetter{mockGetter: newMockGetter(), swap: map[int64]entity.IThreadSafeEntity{}}
		bindCastGetter(t, liar)
		allianceID := mustBuildCastID(t, 212, castAllianceCategory, castAllianceKind)
		alliance := newMockEntityWithKind(allianceID, castAllianceCategory, castAllianceKind)
		requestedID := mustBuildCastID(t, 312, castOtherCategory, castOtherKind)
		actualID := mustBuildCastID(t, 112, castPlayerCategory, castPlayerKind)
		liar.Add(alliance)
		liar.swap[requestedID] = newMockEntityWithKind(actualID, castPlayerCategory, castPlayerKind)
		_, release := entity.NewGuardScope("cast_test")
		defer release()
		if !entity.GetEntityGuard().RequireEntity(alliance) {
			t.Fatal("lock alliance")
		}
		if _, err := CastMulti(NewCastTarget(requestedID)); !errors.Is(err, ErrCastDeadlockRisk) {
			t.Fatalf("err=%v, want ErrCastDeadlockRisk", err)
		}
	})
}

// lyingGetter returns a substitute entity for the ids in swap.
type lyingGetter struct {
	*mockGetter
	swap map[int64]entity.IThreadSafeEntity
}

func (g *lyingGetter) GetMany(ctx context.Context, ids []int64, categories []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	es, err := g.mockGetter.GetMany(ctx, ids, categories)
	if err != nil {
		return nil, err
	}
	for i, id := range ids {
		if e, ok := g.swap[id]; ok {
			es[i] = e
		}
	}
	return es, nil
}

func TestCastThreeReportsTypeMismatchOnEveryPosition(t *testing.T) {
	withCastGroupFunc(t)
	getter := newMockGetter()
	bindCastGetter(t, getter)
	playerID := mustBuildCastID(t, 120, castPlayerCategory, castPlayerKind)
	allianceID := mustBuildCastID(t, 220, castAllianceCategory, castAllianceKind)
	otherID := mustBuildCastID(t, 320, castOtherCategory, castOtherKind)
	other2ID := mustBuildCastID(t, 321, castOtherCategory, castOtherKind)
	player := newMockEntityWithKind(playerID, castPlayerCategory, castPlayerKind)
	getter.Add(player)
	getter.Add(newMockEntityWithKind(allianceID, castAllianceCategory, castAllianceKind))
	getter.Add(newMockEntityWithKind(otherID, castOtherCategory, castOtherKind))
	getter.Add(newMockEntityWithKind(other2ID, castOtherCategory, castOtherKind))
	_, release := entity.NewGuardScope("cast_test")
	defer release()
	if !entity.GetEntityGuard().RequireEntity(player) {
		t.Fatal("lock player")
	}
	a, o1, o2 := NewCastTarget(allianceID), NewCastTarget(otherID), NewCastTarget(other2ID)

	if _, _, _, err := CastThree[*mockEntity, *mockEntity, *mockEntity](a, o1, o2); err != nil {
		t.Fatalf("baseline CastThree = %v", err)
	}
	if _, e2, _, err := CastThree[*mockEntity, *otherMockEntity, *mockEntity](a, o1, o2); !errors.Is(err, ErrCastTypeMismatch) || e2 != nil {
		t.Fatalf("second position: err=%v e2=%v, want ErrCastTypeMismatch", err, e2)
	}
	if _, _, e3, err := CastThree[*mockEntity, *mockEntity, *otherMockEntity](a, o1, o2); !errors.Is(err, ErrCastTypeMismatch) || e3 != nil {
		t.Fatalf("third position: err=%v e3=%v, want ErrCastTypeMismatch", err, e3)
	}
	if _, e2, err := CastTwo[*mockEntity, *otherMockEntity](a, o1); !errors.Is(err, ErrCastTypeMismatch) || e2 != nil {
		t.Fatalf("CastTwo second position: err=%v e2=%v", err, e2)
	}
}

func TestDispatchBroadcastRejectsEmptyTargets(t *testing.T) {
	var mgr *NestMgr
	err := mgr.DispatchBroadcast(context.Background(), HandlerName{}, nil, nil)
	if !errors.Is(err, ErrInvalidMessage) || !strings.Contains(err.Error(), "empty entity ids") {
		t.Fatalf("DispatchBroadcast(nil ids) = %v, want ErrInvalidMessage", err)
	}
}
