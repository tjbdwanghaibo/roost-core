// Cast：按分类锁序取动态目标，拒绝逆序、未声明的 Remote 托管实体、缺失前置条件与类型不符。

package nest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

const (
	// Values are lock ranks, lowest first, and EntityCategoryRemote owns 1,
	// so the local categories start above it and keep their relative order.
	castPlayerCategory   entity.EntityCategory = entity.EntityCategoryRemote + 1
	castAllianceCategory entity.EntityCategory = entity.EntityCategoryRemote + 2
	castOtherCategory    entity.EntityCategory = entity.EntityCategoryRemote + 3
	castPlayerKind       entity.EntityKind     = 11
	castAllianceKind     entity.EntityKind     = 12
	castOtherKind        entity.EntityKind     = 13
)

func newMockEntityWithKind(id int64, category entity.EntityCategory, kind entity.EntityKind) *mockEntity {
	return &mockEntity{EntityBase: entity.NewEntityBase(id, category, false, kind)}
}

func mustBuildCastID(t *testing.T, uniqueID int64, category entity.EntityCategory, kind entity.EntityKind) int64 {
	t.Helper()
	if kind == entity.EntityKindNone {
		return int64(((uint64(uniqueID) & entity.UniqueIDMask) << entity.UniqueIDShift) | (uint64(category) & entity.EntityCategoryMask))
	}
	entity.MustRegisterEntityKindCategory(kind, category)
	id, err := entity.BuildEntityID(uniqueID, kind)
	if err != nil {
		t.Fatalf("BuildEntityID: %v", err)
	}
	return id
}

// withCastGroupFunc 只给测试用的分类命名（锁序就是分类值），让锁序断言失败时可读。
func withCastGroupFunc(t *testing.T) {
	t.Helper()
	entity.MustRegisterEntityCategories(
		entity.EntityCategoryDef{Category: entity.EntityCategoryRemote, Name: "remote"},
		entity.EntityCategoryDef{Category: castPlayerCategory, Name: "player"},
		entity.EntityCategoryDef{Category: castAllianceCategory, Name: "alliance"},
		entity.EntityCategoryDef{Category: castOtherCategory, Name: "other"},
	)
}

func bindCastGetter(t *testing.T, getter entity.Getter) {
	t.Helper()
	release := pushCurrentNestDispatchMsg(&Msg{getter: getter})
	t.Cleanup(release)
}

func TestCastPlayerCanCastAllianceAndOtherByCategoryOrder(t *testing.T) {
	withCastGroupFunc(t)
	getter := newMockGetter()
	bindCastGetter(t, getter)

	playerID := mustBuildCastID(t, 100, castPlayerCategory, castPlayerKind)
	allianceID := mustBuildCastID(t, 200, castAllianceCategory, castAllianceKind)
	otherID := mustBuildCastID(t, 300, castOtherCategory, castOtherKind)
	player := newMockEntityWithKind(playerID, castPlayerCategory, castPlayerKind)
	alliance := newMockEntityWithKind(allianceID, castAllianceCategory, castAllianceKind)
	other := newMockEntityWithKind(otherID, castOtherCategory, castOtherKind)
	getter.Add(player)
	getter.Add(alliance)
	getter.Add(other)

	_, release := entity.NewGuardScope("cast_test")
	defer release()
	guard := entity.GetEntityGuard()
	if !guard.RequireEntity(player) {
		t.Fatal("lock player")
	}

	got, err := guardFixtureCastMulti(NewCastTarget(allianceID), NewCastTarget(otherID))
	if err != nil {
		t.Fatalf("CastMulti alliance/other: %v", err)
	}
	if len(got) != 2 || got[0] != alliance || got[1] != other {
		t.Fatalf("casted = %#v, want alliance/other", got)
	}
	if guard.Entities()[allianceID] == nil {
		t.Fatal("alliance should be locked in current guard")
	}
	if guard.Entities()[otherID] == nil {
		t.Fatal("other should be locked in current guard")
	}
}

func TestCastAllianceCanCastOtherByCategoryOrder(t *testing.T) {
	withCastGroupFunc(t)
	getter := newMockGetter()
	bindCastGetter(t, getter)

	allianceID := mustBuildCastID(t, 201, castAllianceCategory, castAllianceKind)
	otherID := mustBuildCastID(t, 301, castOtherCategory, castOtherKind)
	alliance := newMockEntityWithKind(allianceID, castAllianceCategory, castAllianceKind)
	other := newMockEntityWithKind(otherID, castOtherCategory, castOtherKind)
	getter.Add(alliance)
	getter.Add(other)

	_, release := entity.NewGuardScope("cast_test")
	defer release()
	guard := entity.GetEntityGuard()
	if !guard.RequireEntity(alliance) {
		t.Fatal("lock alliance")
	}

	got, err := guardFixtureCastTargetOne[*mockEntity](NewCastTarget(otherID))
	if err != nil {
		t.Fatalf("CastTargetOne other: %v", err)
	}
	if got != other {
		t.Fatalf("casted other = %p, want %p", got, other)
	}
	if guard.Entities()[otherID] == nil {
		t.Fatal("other should be locked in current guard")
	}
}

func TestCastRejectsReverseCategoryOrder(t *testing.T) {
	withCastGroupFunc(t)
	getter := newMockGetter()

	playerID := mustBuildCastID(t, 101, castPlayerCategory, castPlayerKind)
	allianceID := mustBuildCastID(t, 202, castAllianceCategory, castAllianceKind)
	otherID := mustBuildCastID(t, 302, castOtherCategory, castOtherKind)
	player := newMockEntityWithKind(playerID, castPlayerCategory, castPlayerKind)
	alliance := newMockEntityWithKind(allianceID, castAllianceCategory, castAllianceKind)
	other := newMockEntityWithKind(otherID, castOtherCategory, castOtherKind)
	getter.Add(player)
	getter.Add(alliance)
	getter.Add(other)

	cases := []struct {
		name     string
		locked   *mockEntity
		targetID int64
	}{
		{name: "alliance_to_player", locked: alliance, targetID: playerID},
		{name: "other_to_player", locked: other, targetID: playerID},
		{name: "other_to_alliance", locked: other, targetID: allianceID},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bindCastGetter(t, getter)
			_, release := entity.NewGuardScope("cast_test")
			defer release()
			if !entity.GetEntityGuard().RequireEntity(tc.locked) {
				t.Fatalf("lock %s", tc.name)
			}
			_, err := guardFixtureCastTargetOne[*mockEntity](NewCastTarget(tc.targetID))
			if !errors.Is(err, ErrCastDeadlockRisk) {
				t.Fatalf("err = %v, want ErrCastDeadlockRisk", err)
			}
		})
	}
}

func TestCastRejectsRemoteAfterLocalBeforePreparingDistributedLock(t *testing.T) {
	withCastGroupFunc(t)
	getter := newMockGetter()
	bindCastGetter(t, getter)

	playerID := mustBuildCastID(t, 401, castPlayerCategory, castPlayerKind)
	remoteID := mustBuildCastID(t, 402, entity.EntityCategoryRemote, nestRemoteManagedKind)
	player := newMockEntityWithKind(playerID, castPlayerCategory, castPlayerKind)
	getter.Add(player)

	_, release := entity.NewGuardScope("cast_remote_order_test")
	defer release()
	if !entity.GetEntityGuard().RequireEntity(player) {
		t.Fatal("lock player")
	}
	_, err := guardFixtureCastTargetOne[*mockEntity](NewCastTarget(remoteID))
	if !errors.Is(err, ErrCastDeadlockRisk) {
		t.Fatalf("err = %v, want ErrCastDeadlockRisk", err)
	}
}

func TestCastRejectsInvalidTargetID(t *testing.T) {
	getter := newMockGetter()
	bindCastGetter(t, getter)

	_, release := entity.NewGuardScope("cast_test")
	defer release()

	_, err := guardFixtureCastTargetOne[*mockEntity](NewCastTarget(102))
	if !errors.Is(err, ErrCastInvalidTarget) {
		t.Fatalf("err = %v, want ErrCastInvalidTarget", err)
	}
}

// A remote-managed entity that the dispatch declared is already Guarded when a
// handler casts to it; the cast reuses that lock and must not refuse it. This
// is the only way a cast ever reaches a remote-managed entity — the gate never
// acquires one itself (see refuseUndeclaredRemoteTargets).
func TestCastReusesARemoteManagedEntityDeclaredBeforeDispatch(t *testing.T) {
	withCastGroupFunc(t)
	getter := newMockGetter()
	bindCastGetter(t, getter)

	remoteID := mustBuildCastID(t, 301, entity.EntityCategoryRemote, nestRemoteManagedKind)
	remote := newMockEntityWithKind(remoteID, entity.EntityCategoryRemote, nestRemoteManagedKind)
	getter.Add(remote)

	_, release := entity.NewGuardScope("cast_declared_remote_test")
	defer release()
	if !entity.GetEntityGuard().RequireEntity(remote) {
		t.Fatal("lock the declared remote entity")
	}
	got, err := guardFixtureCastTargetOne[*mockEntity](NewCastTarget(remoteID))
	if err != nil {
		t.Fatalf("cast to a declared remote entity = %v, want nil", err)
	}
	if got != remote {
		t.Fatalf("cast returned %p, want the guarded entity %p", got, remote)
	}
}

func TestCastRejectsDynamicRemoteManagedEntity(t *testing.T) {
	getter := newMockGetter()

	remoteID := mustBuildCastID(t, 300, entity.EntityCategoryRemote, nestRemoteManagedKind)

	msg := &Msg{getter: getter}
	releaseCurrent := pushCurrentNestDispatchMsg(msg)
	defer releaseCurrent()
	_, releaseGuard := entity.NewGuardScope("cast_test")
	defer releaseGuard()
	_, err := guardFixtureCastTargetOne[*mockEntity](NewCastTarget(remoteID))
	if !errors.Is(err, entity.ErrRemoteWriteCapabilityDisabled) {
		t.Fatalf("dynamic remote cast error=%v", err)
	}
}

// Cast 的前置条件：没有守卫作用域、没有正在派发的消息、消息没带 getter、目标 id 为 0、getter 找不到实体、
// 实体的真实分类与 id 不一致（锁序会反）都必须拒绝；CastTwo / CastThree 每个位置的类型不符都报
// ErrCastTypeMismatch 而不交出零值；DispatchBroadcast 对空 id 列表以 ErrInvalidMessage 拒绝。

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
		if _, err := guardFixtureCastMulti(target); !errors.Is(err, ErrCastNoContext) {
			t.Fatalf("err=%v, want ErrCastNoContext", err)
		}
	})
	t.Run("guard scope without dispatch message", func(t *testing.T) {
		_, release := entity.NewGuardScope("cast_test")
		defer release()
		if _, err := guardFixtureCastMulti(target); !errors.Is(err, ErrCastNoContext) {
			t.Fatalf("err=%v, want ErrCastNoContext", err)
		}
	})
	t.Run("dispatch message without getter", func(t *testing.T) {
		release := pushCurrentNestDispatchMsg(&Msg{})
		defer release()
		_, releaseGuard := entity.NewGuardScope("cast_test")
		defer releaseGuard()
		if _, err := guardFixtureCastMulti(target); !errors.Is(err, ErrCastGetterNotSet) {
			t.Fatalf("err=%v, want ErrCastGetterNotSet", err)
		}
	})
	t.Run("zero target id", func(t *testing.T) {
		bindCastGetter(t, getter)
		_, release := entity.NewGuardScope("cast_test")
		defer release()
		_, err := guardFixtureCastMulti(NewCastTarget(0))
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
		_, err := guardFixtureCastMulti(NewCastTarget(missingID))
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
		if _, err := guardFixtureCastMulti(NewCastTarget(requestedID)); !errors.Is(err, ErrCastDeadlockRisk) {
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

	if _, _, _, err := guardFixtureCastThree[*mockEntity, *mockEntity, *mockEntity](a, o1, o2); err != nil {
		t.Fatalf("baseline CastThree = %v", err)
	}
	if _, e2, _, err := guardFixtureCastThree[*mockEntity, *otherMockEntity, *mockEntity](a, o1, o2); !errors.Is(err, ErrCastTypeMismatch) || e2 != nil {
		t.Fatalf("second position: err=%v e2=%v, want ErrCastTypeMismatch", err, e2)
	}
	if _, _, e3, err := guardFixtureCastThree[*mockEntity, *mockEntity, *otherMockEntity](a, o1, o2); !errors.Is(err, ErrCastTypeMismatch) || e3 != nil {
		t.Fatalf("third position: err=%v e3=%v, want ErrCastTypeMismatch", err, e3)
	}
	if _, e2, err := guardFixtureCastTwo[*mockEntity, *otherMockEntity](a, o1); !errors.Is(err, ErrCastTypeMismatch) || e2 != nil {
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

// otherKindEntity is a second concrete entity type so a Cast can ask for the
// wrong one.
type otherKindEntity struct {
	*entity.EntityBase
}

func (m *otherKindEntity) Base() *entity.EntityBase { return m.EntityBase }

// shortGetter returns fewer entities than asked, the way a getter with a
// bug (or a partial remote answer) would.
type shortGetter struct{ *mockGetter }

func (g shortGetter) GetMany(ctx context.Context, ids []int64, categories []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	es, err := g.mockGetter.GetMany(ctx, ids, categories)
	if err != nil || len(es) == 0 {
		return es, err
	}
	return es[:len(es)-1], nil
}

func castFixture(t *testing.T, getter entity.Getter) (playerID, allianceID, otherID int64) {
	t.Helper()
	withCastGroupFunc(t)
	bindCastGetter(t, getter)
	playerID = mustBuildCastID(t, 100, castPlayerCategory, castPlayerKind)
	allianceID = mustBuildCastID(t, 200, castAllianceCategory, castAllianceKind)
	otherID = mustBuildCastID(t, 300, castOtherCategory, castOtherKind)
	return
}

// The typed Cast helpers promise a typed result or ErrCastTypeMismatch that
// names the id and the actual type. A wrong assertion that fell through would
// hand the handler a zero entity to write into.
func TestTypedCastsRefuseTheWrongEntityType(t *testing.T) {
	getter := newMockGetter()
	playerID, allianceID, otherID := castFixture(t, getter)
	player := newMockEntityWithKind(playerID, castPlayerCategory, castPlayerKind)
	getter.Add(player)
	getter.Add(newMockEntityWithKind(allianceID, castAllianceCategory, castAllianceKind))
	getter.Add(newMockEntityWithKind(otherID, castOtherCategory, castOtherKind))
	_, release := entity.NewGuardScope("cast_test")
	defer release()
	if !entity.GetEntityGuard().RequireEntity(player) {
		t.Fatal("lock player")
	}

	if _, err := guardFixtureCastTargetOne[*otherKindEntity](NewCastTarget(allianceID)); !errors.Is(err, ErrCastTypeMismatch) || !strings.Contains(err.Error(), "*nest.mockEntity") {
		t.Fatalf("CastTargetOne wrong type = %v", err)
	}
	if _, _, err := guardFixtureCastTwo[*mockEntity, *otherKindEntity](NewCastTarget(allianceID), NewCastTarget(otherID)); !errors.Is(err, ErrCastTypeMismatch) {
		t.Fatalf("CastTwo second wrong = %v", err)
	}
	if _, _, err := guardFixtureCastTwo[*otherKindEntity, *mockEntity](NewCastTarget(allianceID), NewCastTarget(otherID)); !errors.Is(err, ErrCastTypeMismatch) {
		t.Fatalf("CastTwo first wrong = %v", err)
	}
	// CastThree needs a third same-order target; reuse other twice is a
	// deadlock-risk shape, so only the first-position mismatch is exercised.
	if _, _, _, err := guardFixtureCastThree[*otherKindEntity, *mockEntity, *mockEntity](NewCastTarget(allianceID), NewCastTarget(otherID), NewCastTarget(otherID)); err == nil {
		t.Fatal("CastThree with a wrong first type must not succeed")
	}
	if _, err := guardFixtureCastMulti(); !errors.Is(err, ErrCastInvalidTarget) || !strings.Contains(err.Error(), "empty targets") {
		t.Fatalf("guardFixtureCastMulti() = %v", err)
	}
	if _, err := guardFixtureCastMulti(NewCastTarget(allianceID), CastTarget{}); !errors.Is(err, ErrCastInvalidTarget) || !strings.Contains(err.Error(), "index=1 id=0") {
		t.Fatalf("CastMulti with a zero id = %v", err)
	}
}

// A getter that answers with the wrong number of entities is a broken
// contract, not "some were missing": the positions could no longer be
// matched to targets.
func TestCastMultiRefusesAGetterThatReturnsTheWrongCount(t *testing.T) {
	base := newMockGetter()
	playerID, allianceID, otherID := castFixture(t, shortGetter{base})
	player := newMockEntityWithKind(playerID, castPlayerCategory, castPlayerKind)
	base.Add(player)
	base.Add(newMockEntityWithKind(allianceID, castAllianceCategory, castAllianceKind))
	base.Add(newMockEntityWithKind(otherID, castOtherCategory, castOtherKind))
	_, release := entity.NewGuardScope("cast_test")
	defer release()
	if !entity.GetEntityGuard().RequireEntity(player) {
		t.Fatal("lock player")
	}
	_, err := guardFixtureCastMulti(NewCastTarget(allianceID), NewCastTarget(otherID))
	if !errors.Is(err, ErrCastInvalidTarget) || !strings.Contains(err.Error(), "returned 1 entities for 2") {
		t.Fatalf("CastMulti with a short getter = %v", err)
	}
}
