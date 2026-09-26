package nest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// RR-20260926-26：handler 在快阶段用动态 Cast 访问未加载目标时，业务要能拿到
// ErrColdLoadInLogic 并降级。
//
// 动态 Cast 的目标不在消息声明里，按 RR-02 契约只读已加载实体：冷目标返回可
// errors.Is 判别的 ErrColdLoadInLogic，不做 I/O。RR-06 把 ManagerAccess.GetMany
// 的这条非阻塞分支改成了 panic，Nest 把它变成整个 handler 的失败，“好友离线”这类
// 降级逻辑不再可能执行（修前 fallback=false，请求返回 panic 转成的错误）。
func TestFastCastColdTargetLetsBusinessFallBack(t *testing.T) {
	manager := entity.NewEntityManager()
	access := entity.NewManagerAccess(manager)
	loader := &stageLoader{manager: manager}
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	withCastGroupFunc(t)
	hot := mustBuildCastID(t, 8301, castPlayerCategory, castPlayerKind)
	cold := mustBuildCastID(t, 8302, castAllianceCategory, castAllianceKind)
	if err := manager.TryAdd(newMockEntityWithKind(hot, castPlayerCategory, castPlayerKind)); err != nil {
		t.Fatal(err)
	}
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithWorkerPools(WorkerPoolConfig{1, 8}, WorkerPoolConfig{1, 8}))
	fallback := false
	mgr.MustRegisterHandlerWithMeta(NewHandlerName("optional_friend"), func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		if _, err := CastOne[entity.IThreadSafeEntity](cold); err != nil {
			if errors.Is(err, entity.ErrColdLoadInLogic) {
				fallback = true
				return "friend offline", nil
			}
			return nil, err
		}
		return "friend online", nil
	}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = mgr.Shutdown(ctx)
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	ret, err := mgr.Request(ctx, NewHandlerName("optional_friend"), hot, nil)
	if err != nil || ret != "friend offline" || !fallback {
		t.Fatalf("ret=%v err=%v fallback=%v: fast-stage cold Cast miss did not reach the business as ErrColdLoadInLogic", ret, err, fallback)
	}
	if loads := loader.calls.Load(); loads != 0 {
		t.Fatalf("undeclared dynamic Cast target was loaded %d times", loads)
	}
}
