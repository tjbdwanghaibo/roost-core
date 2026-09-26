package entity

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/fctx"
)

// RR-20260926-26：快阶段的冷缺失本身不阻塞，应返回可 errors.Is 判别的 ErrColdLoadInLogic。
//
// RR-02 的契约（Getter 注释）是：快阶段访问未加载且配置了 loader 的目标时不做 I/O、
// 不等待 singleflight，返回 ErrColdLoadInLogic，业务可以据此降级（例如“好友离线”）。
// RR-06 修复为覆盖全部快 worker 访问入口，在 Get / GetMany 的这个分支之前插入了
// fctx.BlockingError 检查并直接 panic——这条路径本来就不阻塞，于是业务再也拿不到错误，
// 整个 handler 失败并回滚。只有真正会等待的入口（I/O、等待 flight/投影）才应 fail-fast。
func TestFastWorkerColdMissReturnsErrorInsteadOfPanicking(t *testing.T) {
	manager := NewEntityManager()
	access := NewManagerAccess(manager)
	loader := &countingAggregateLoader{manager: manager}
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	hot := newMgrTestEntity(5101, testEntityCategoryPlayer)
	manager.Add(hot)
	_, release := fctx.NewContext(fctx.WithFastWorker(), fctx.WithHandler("optional_friend"))
	defer release()

	for _, detached := range []bool{false, true} {
		ctx := WithLoadedEntitiesOnly(context.Background())
		if detached {
			// 保存的 Background ctx 不带加载约束，快 worker 标记本身也必须生效。
			ctx = context.Background()
		}
		checkColdMiss(t, "Get", func() error {
			_, err := access.Get(ctx, 5102, EntityCategoryNone)
			return err
		})
		checkColdMiss(t, "GetMany", func() error {
			_, err := access.GetMany(ctx, []int64{5101, 5102}, nil)
			return err
		})
	}
	if loads := loader.loads.Load(); loads != 0 {
		t.Fatalf("fast-stage cold miss reached the loader %d times", loads)
	}
	// 已加载实体仍按内存读取成功。
	if value, err := access.Get(context.Background(), 5101, EntityCategoryNone); err != nil || value != hot {
		t.Fatalf("hot Get: %v %v", value, err)
	}
}

func checkColdMiss(t *testing.T, name string, call func() error) {
	t.Helper()
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("%s: fast-stage cold miss panicked instead of returning ErrColdLoadInLogic: %v", name, r)
			}
		}()
		err = call()
	}()
	if !errors.Is(err, ErrColdLoadInLogic) || !errors.Is(err, fctx.ErrBlockingInFastWorker) {
		t.Fatalf("%s: err=%v, want ErrColdLoadInLogic with the fast-worker marker", name, err)
	}
}
