package persistflow

// B27 第 2 批：RR-20260926-53 的未验证项“真实生成工程里没有构造‘提交后 release hook 失败’”。这里走正式生成 Entity / DAO、
// Nest、文件 WAL 与真实 Mongo：业务经公开 API RegisterOnEntityRelease 登记的 hook 在本地事务持久提交之后 panic。
// 承诺：回复带 ErrAfterCommitFailed（保留 hook 原因），已提交的修改留在内存并投影进 Mongo；
// 未提交路径（handler 报错）上同样的 hook panic 不带哨兵，内存与 Mongo 都回到修改前。

import (
	"errors"
	"sync/atomic"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/nest"
)

var errReleaseHook = errors.New("persistflow: release hook failed after commit")

// armReleaseHookPanic 登记一个只对 armed 里的实体 ID 触发一次的 release hook panic。
func armReleaseHookPanic(t *testing.T, h *tradeFixture) *atomic.Int64 {
	t.Helper()
	var armed atomic.Int64
	unhook, err := h.access.RegisterOnEntityRelease(func(e entity.IThreadSafeEntity) {
		if id := e.ID(); id != 0 && armed.CompareAndSwap(id, 0) {
			panic(errReleaseHook)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unhook)
	return &armed
}

func TestGeneratedDataEngineReleaseHookPanicAfterCommitCarriesSentinel(t *testing.T) {
	ctx, client := reloadTestClient(t)
	for _, policy := range []nest.DurabilityPolicy{nest.DurabilityAsync, nest.DurabilityStrict, nest.DurabilityPipelined} {
		t.Run(policy.String(), func(t *testing.T) {
			base := 34000 + int(policy)*10
			clearTraderDocuments(t, ctx, client, base, 2)
			h := newTradeFixture(t, ctx, client, t.TempDir(), "release-hook", policy, false)
			ids := h.seed(t, base, 2)
			id, other := ids[0], ids[1]
			seeded := h.mongoState(t, id)
			armed := armReleaseHookPanic(t, h)

			// 已提交路径：trade_buy 持久提交后释放实体，hook panic。
			armed.Store(id)
			_, err := h.scheduler.Request(ctx, nest.NewHandlerName("trade_buy"), id, nil)
			if armed.Load() != 0 {
				t.Fatal("premise: the release hook did not run")
			}
			if !errors.Is(err, nest.ErrAfterCommitFailed) || !errors.Is(err, errReleaseHook) {
				t.Fatalf("committed %s transaction with a release hook panic replied %v: want ErrAfterCommitFailed with the hook cause", policy, err)
			}
			t.Logf("committed reply: %v", err)
			bought := tradeState{Coins: seeded.Coins - 5, Items: seeded.Items + 1, Transfers: seeded.Transfers, Version: seeded.Version + 1}
			if memory := h.memoryState(t, id); memory != bought {
				t.Fatalf("memory after the committed transaction=%+v, want %+v (the hook failure must not roll back a committed transaction)", memory, bought)
			}
			h.noFatal(t, h.runtime.Flush(ctx), "after the hook failure")
			if stored := h.mongoState(t, id); stored != bought {
				t.Fatalf("Mongo after the committed transaction=%+v, want %+v", stored, bought)
			}

			// 未提交路径保持性：handler 报错后同样的 hook panic，不带哨兵，内存与 Mongo 不变（回复是否保留业务错误见下一个用例）。
			armed.Store(id)
			err = h.request("trade_reject", id, other)
			if armed.Load() != 0 {
				t.Fatal("premise: the release hook did not run on the rolled-back path")
			}
			if err == nil || errors.Is(err, nest.ErrAfterCommitFailed) {
				t.Fatalf("rolled-back %s transaction with a release hook panic replied %v: want an error without ErrAfterCommitFailed", policy, err)
			}
			t.Logf("rolled-back reply: %v", err)
			if memory := h.memoryState(t, id); memory != bought {
				t.Fatalf("memory after the rolled-back transaction=%+v, want %+v", memory, bought)
			}
			h.noFatal(t, h.runtime.Flush(ctx), "after the rolled-back transaction")
			if stored := h.mongoState(t, id); stored != bought {
				t.Fatalf("Mongo after the rolled-back transaction=%+v, want %+v", stored, bought)
			}

			// hook 之后同一实体照常可写，投影不 fatal，内存 == Mongo。
			if _, err := h.scheduler.Request(ctx, nest.NewHandlerName("trade_buy"), id, nil); err != nil {
				t.Fatalf("trade_buy after the hook failure: %v", err)
			}
			h.noFatal(t, h.runtime.Flush(ctx), "after the next trade_buy")
			if memory, stored := h.memoryState(t, id), h.mongoState(t, id); memory != stored || stored.Coins != bought.Coins-5 {
				t.Fatalf("after the next trade_buy memory=%+v Mongo=%+v", memory, stored)
			}
			if remaining := h.records(t); len(remaining) != 0 {
				t.Fatalf("remaining WAL records=%d", len(remaining))
			}
			h.close(t, nil)
		})
	}
}

// 未提交路径上 release hook panic 时，回复仍应带业务错误（调用方要能分辨“业务拒绝”与“释放失败”）。
// 2026-09-30 实跑（B27 第 2 批）：async / strict / pipelined 三种策略下回复都只有 hook 错误
// （`persistflow: release hook failed after commit`），业务错误 errTradeRejected 丢失；remoteflow 的 strict Remote 消息同样
// （见 remoteflow/after_commit_test.go）。链路：handler 返回业务错误 → dispatchLoadedEntities 的 `defer release()`
// （nest/nest_dispatch.go:404-406）→ releaseDispatchEntities → Guard.ReleaseEntity → runOnEntityRelease 不 recover，hook 的 panic
// 一路传到 runNestLogic 的 recover（nest/nest_dispatch.go:203-209 `err = recoveredErr`），在途的 handler 错误被整个替换。
func TestGeneratedDataEngineRolledBackReleaseHookPanicKeepsBusinessError(t *testing.T) {
	ctx, client := reloadTestClient(t)
	for _, policy := range []nest.DurabilityPolicy{nest.DurabilityAsync, nest.DurabilityStrict, nest.DurabilityPipelined} {
		t.Run(policy.String(), func(t *testing.T) {
			base := 34100 + int(policy)*10
			clearTraderDocuments(t, ctx, client, base, 2)
			h := newTradeFixture(t, ctx, client, t.TempDir(), "release-hook-business", policy, false)
			ids := h.seed(t, base, 2)
			armed := armReleaseHookPanic(t, h)
			armed.Store(ids[0])
			err := h.request("trade_reject", ids...)
			if armed.Load() != 0 {
				t.Fatal("premise: the release hook did not run")
			}
			if !errors.Is(err, errTradeRejected) || errors.Is(err, nest.ErrAfterCommitFailed) {
				t.Fatalf("rolled-back %s transaction with a release hook panic replied %v: want the business error kept", policy, err)
			}
			h.close(t, nil)
		})
	}
}
