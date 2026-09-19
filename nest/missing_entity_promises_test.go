package nest

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// RR-20260919-09：缺失实体在 single / broadcast 分派处变成 nil 解引用。
//
// 生产的 `ManagerAccess.Get` 在 manager 未命中、且没有 loader（或 loader 合法
// 地返回 nil）时返回 `(nil, nil)`。single 与 broadcast 只看 err 就调 `e.Touch()`：
//
//   - single 的调用方拿到的是顶层 recover 出来的 runtime nil pointer，而不是
//     `ErrEntityNotFound`——"没有这个实体"和"框架出错了"分不开；
//   - broadcast 的逐实体 recover 在 `Touch` 之后，所以一个缺失的 id 会逃到整次
//     分派的外层 recover，**后面的实体一个都不再执行**。
//
// 同一个包里的 multi / multiGroup 已经是 `e != nil && e.Touch()`，所以这不是
// 契约不清，是两条路径漏了。替身必须是生产那一个的形状：现有 mockGetter 对缺失
// 返回 error，恰好掩盖了它。

// nilForMissingGetter is the production contract: not found is (nil, nil).
type nilForMissingGetter struct{ present map[int64]entity.IThreadSafeEntity }

func (g nilForMissingGetter) Get(_ context.Context, id int64, _ entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	return g.present[id], nil
}

func (g nilForMissingGetter) GetMany(_ context.Context, ids []int64, _ []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	out := make([]entity.IThreadSafeEntity, len(ids))
	for i, id := range ids {
		out[i] = g.present[id]
	}
	return out, nil
}

func TestASingleDispatchToAMissingEntitySaysNotFound(t *testing.T) {
	ResetHandlersForTest()
	defer ResetHandlersForTest()

	id := mustBuildCastID(t, 7301, entity.EntityCategory(1), nestLocalKind)
	name := NewHandlerName("test_missing_entity_single")
	MustRegisterMemoryHandler(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		t.Error("the handler ran for an entity that does not exist")
		return nil, nil
	})

	mgr := &NestMgr{getter: nilForMissingGetter{}}
	_, err := mgr.singleDispatch(name.String(), id, nil)
	if !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("dispatch to a missing entity = %v, want ErrEntityNotFound", err)
	}
}

// One missing id must not take the rest of the broadcast with it.
func TestABroadcastSkipsMissingEntitiesAndKeepsGoing(t *testing.T) {
	ResetHandlersForTest()
	defer ResetHandlersForTest()

	presentID := mustBuildCastID(t, 7303, entity.EntityCategory(1), nestLocalKind)
	present := newMockEntity(presentID, entity.EntityCategory(1))
	missingID := mustBuildCastID(t, 7302, entity.EntityCategory(1), nestLocalKind)

	calls := 0
	name := NewHandlerName("test_missing_entity_broadcast")
	MustRegisterMemoryHandler(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		calls++
		if len(es) != 1 || es[0] != present {
			t.Errorf("the handler ran for %v, want only the entity that exists", es)
		}
		return nil, nil
	})

	mgr := &NestMgr{getter: nilForMissingGetter{present: map[int64]entity.IThreadSafeEntity{presentID: present}}}
	// The missing one is FIRST: that is the order in which it used to abort
	// everything behind it.
	mgr.broadcastDispatch(name.String(), []int64{missingID, presentID}, nil)
	if calls != 1 {
		t.Fatalf("the handler ran %d times, want once: a missing id aborted the rest of the broadcast", calls)
	}
}
