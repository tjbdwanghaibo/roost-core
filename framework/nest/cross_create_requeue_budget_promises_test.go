package nest

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// OPEN-ITEMS B24：RR-20260926-48 的交叉创建冲突靠“整条回滚 + 固定 5ms 重排、上限 400 次”解开，重排没有抖动，理论上对称的两侧
// 可能一直同时重试直到耗尽。2026-09-27 的统计（4 对交叉创建、首轮两侧都持有第一个新实体后才建第二个、-race -count=500，
// 另加 200µs / 1ms / 3ms 持锁变体，记录见 bf-48 §后续验证）没有出现耗尽，最多 234 次，所以 C09 按推荐不加抖动。
// 这里是轻量版本：一次运行里 4 对同时对称冲突，每对都在重排上限内解开（一方成功、一方 ErrEntityExists），没有请求以锁超时耗尽。
// 大样本统计（-count=500）不放进常规门禁。
// 更正（2026-10-05，U-0279）：v1.20.0 生成工程 TestGeneratedDataEngineCrossCreateResolvesOnRealWAL 正常负载下 35%～53% 的运行耗尽 400 次上限
// （v1.19.2 同样），按 C09 的预案给重排加了抖动（transientRequeueDelay：5ms 下限 + [0, 5ms) 均匀抖动）；
// 打破对称的确定性回归见 requeue_jitter_promises_test.go。
func TestSymmetricCrossCreatePairsResolveWithinRequeueBudget(t *testing.T) {
	const pairs = 4
	manager := entity.NewEntityManager()
	ids := addPilots(t, manager, 9900, 2*pairs)
	access := entity.NewManagerAccess(manager)
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(&recordsCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 2 * pairs, QueueCap: 64}, WorkerPoolConfig{}))
	attempts := make([]atomic.Int64, 2*pairs)
	names := make([]HandlerName, 2*pairs)
	held := make(chan struct{}, 2*pairs)
	proceed := make([]chan struct{}, pairs)
	for p := range pairs {
		proceed[p] = make(chan struct{})
		x := mustBuildCastID(t, 9950+int64(2*p), entity.EntityCategory(1), createdInScopeKind)
		y := mustBuildCastID(t, 9951+int64(2*p), entity.EntityCategory(1), createdInScopeKind)
		for side := range 2 {
			slot, first, second := 2*p+side, x, y
			if side == 1 {
				first, second = y, x
			}
			names[slot] = NewHandlerName("b24_cross_" + strconv.Itoa(slot))
			gate := proceed[p]
			mgr.MustRegisterHandlerWithMeta(names[slot], func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				n := attempts[slot].Add(1)
				if _, err := access.Create(createParam(first)); err != nil {
					return nil, err
				}
				if n == 1 {
					held <- struct{}{} // 两侧都持有自己的第一个新实体后才建第二个：首轮必然对称冲突
					<-gate
				}
				if _, err := access.Create(createParam(second)); err != nil {
					return nil, err
				}
				return "ok", nil
			}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
		}
	}
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	// 失败要快速报出来：停机有上限，超时只记错误、不再等；放行闸门的 defer 后注册、先执行，
	// 断言中途 t.Fatal 时停在 <-gate 的 handler 也会被放行，不让停机等它们（之前 Shutdown(Background) 会挂到包超时）。
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := mgr.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	}()
	var openGates sync.Once
	releaseGates := func() {
		openGates.Do(func() {
			for _, gate := range proceed {
				close(gate)
			}
		})
	}
	defer releaseGates()
	out := make(chan requestResult, 2*pairs)
	for slot := range 2 * pairs {
		sendRequest(mgr, strconv.Itoa(slot), names[slot], ids[slot], out)
	}
	for range 2 * pairs {
		select {
		case <-held:
		case <-time.After(10 * time.Second):
			t.Fatal("not every handler reached its first created entity")
		}
	}
	releaseGates()
	results := make(map[string]requestResult, 2*pairs)
	for len(results) < 2*pairs {
		select {
		case r := <-out:
			results[r.tag] = r
		case <-time.After(30 * time.Second):
			t.Fatalf("symmetric cross creates did not all finish within 30s: %d/%d", len(results), 2*pairs)
		}
	}
	for p := range pairs {
		winners, losers := 0, 0
		for side := range 2 {
			slot := 2*p + side
			r := results[strconv.Itoa(slot)]
			switch {
			case r.err == nil && r.ret == "ok":
				winners++
			case errors.Is(r.err, entity.ErrEntityExists):
				losers++
			case errors.Is(r.err, ErrLockTimeout):
				t.Fatalf("pair %d slot %d exhausted the requeue budget after %d attempts: %v", p, slot, attempts[slot].Load(), r.err)
			default:
				t.Fatalf("pair %d slot %d: want ok or ErrEntityExists, got ret=%v err=%v", p, slot, r.ret, r.err)
			}
			if a := attempts[slot].Load(); a > entityGroupDispatchRequeueMax {
				t.Fatalf("pair %d slot %d ran %d times, above the requeue budget %d", p, slot, a, entityGroupDispatchRequeueMax)
			}
		}
		if winners != 1 || losers != 1 {
			t.Fatalf("pair %d: winners=%d losers=%d, want one each", p, winners, losers)
		}
	}
}
