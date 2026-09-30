package persistflow

// B27 第 2 批：嵌套独立事务与 fence 在真实 WAL + Mongo 链路上的端到端。
//   - RR-20260926-74 / RR-20260927-07 的未验证项“未在真实 WAL + Mongo 环境验证，回归使用受控 committer”：外层可回滚 handler 里
//     RunIsolatedTransaction 持久写外层已快照的实体——经生成 DAO setter（RR-74）或直接 AddMutation 原始 mutation（RR-07）——
//     在交给 committer 之前以 ErrNestedTransactionRollbackConflict 拒绝，WAL 没有多出记录，外层回滚后内存 == Mongo，下一笔照常投影。
//     memory 外层不检查（RR-74 保持性）：嵌套照常提交，外层随后失败的回复带 ErrNestedTransactionCommitted（RR-20260926-65）。
//   - RR-20260927-06 的未验证项“没有在 Mongo 投影 / 生成链路上跑”：引擎 fence 之后，消息自己的事务在交给 committer 之前返回
//     ErrNestFenced，WAL 没有记录，修改已回滚。

import (
	"context"
	"errors"
	"testing"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type nestedForm uint8

const (
	nestedFormDAO nestedForm = iota // 生成 DAO setter（MarkPersist 登记参与方）
	nestedFormRaw                   // 直接 AddMutation 原始 mutation（DocumentKey 形式）
)

// nestedProbe 是 trade_nested / trade_nested_memory 的参数与观察结果。
type nestedProbe struct {
	form          nestedForm
	isoErr        error
	isoRuns       int
	coinsAfterIso int64
}

func registerNestedHandlers(h *tradeFixture, policy nest.DurabilityPolicy) {
	runNested := func(e *Trader, probe *nestedProbe) {
		_, probe.isoErr = nest.RunIsolatedTransaction(context.Background(), h.runtime.Projector, "trade_nested_inner", func() (any, error) {
			probe.isoRuns++
			if probe.form == nestedFormDAO {
				e.wallet.SetCoins(e.wallet.GetCoins() - 100)
				return nil, nil
			}
			data, err := bson.Marshal(bson.M{"coins": e.wallet.GetCoins() - 100})
			if err != nil {
				return nil, err
			}
			version := e.wallet.DirtyTracker().Version()
			return nil, nest.CurrentRollbackTx().AddMutation(nest.EntityMutation{
				Key:  coredata.DocumentKey{Database: h.database, Resource: "trade_wallets", ID: e.ID()},
				Kind: coredata.MutationPut, ExpectedVersion: version, NextVersion: version + 1, Mask: 1, Schema: 1, Codec: "bson", Data: data,
			})
		})
		probe.coinsAfterIso = e.wallet.GetCoins()
	}
	// 外层可回滚（RollbackState）：先改自己的 DAO，再让嵌套事务写同一实体，随后外层失败。
	h.scheduler.MustRegisterHandlerWithMeta(nest.NewHandlerName("trade_nested"), func(es []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
		e, probe := es[0].(*Trader), params[0].(*nestedProbe)
		e.wallet.SetCoins(e.wallet.GetCoins() - 1)
		runNested(e, probe)
		return nil, errTradeRejected
	}, nest.HandlerMeta{Rollback: nest.RollbackState, Durability: policy})
	// 外层 memory（RollbackNone、无 RollbackTx）：不登记快照，嵌套事务照常提交；外层随后失败。
	h.scheduler.MustRegisterHandlerWithMeta(nest.NewHandlerName("trade_nested_memory"), func(es []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
		runNested(es[0].(*Trader), params[0].(*nestedProbe))
		return nil, errTradeRejected
	}, nest.HandlerMeta{})
}

func TestGeneratedDataEngineNestedIsolatedWriteToOuterSnapshotIsRefused(t *testing.T) {
	ctx, client := reloadTestClient(t)
	for _, policy := range []nest.DurabilityPolicy{nest.DurabilityAsync, nest.DurabilityStrict, nest.DurabilityPipelined} {
		t.Run(policy.String(), func(t *testing.T) {
			base := 35000 + int(policy)*10
			clearTraderDocuments(t, ctx, client, base, 1)
			h := newTradeFixtureRegistering(t, ctx, client, t.TempDir(), "nested", policy, false, func(h *tradeFixture) { registerNestedHandlers(h, policy) })
			id := h.seed(t, base, 1)[0]
			seeded := h.mongoState(t, id)
			for _, tc := range []struct {
				name string
				form nestedForm
			}{{name: "dao_setter", form: nestedFormDAO}, {name: "raw_mutation", form: nestedFormRaw}} {
				t.Run(tc.name, func(t *testing.T) {
					probe := &nestedProbe{form: tc.form}
					admitted := h.wal.Stats().Admitted
					_, err := h.scheduler.Request(ctx, nest.NewHandlerName("trade_nested"), id, nest.Params{probe})
					if probe.isoRuns != 1 {
						t.Fatalf("premise: the isolated transaction ran %d time(s)", probe.isoRuns)
					}
					if !errors.Is(probe.isoErr, nest.ErrNestedTransactionRollbackConflict) {
						t.Fatalf("RunIsolatedTransaction writing the outer snapshotted Trader returned %v, want ErrNestedTransactionRollbackConflict", probe.isoErr)
					}
					if delta := h.wal.Stats().Admitted - admitted; delta != 0 {
						t.Fatalf("the refused isolated transaction still handed %d record(s) to the WAL", delta)
					}
					if !errors.Is(err, errTradeRejected) || errors.Is(err, nest.ErrNestedTransactionCommitted) || errors.Is(err, nest.ErrAfterCommitFailed) {
						t.Fatalf("outer reply=%v, want the business error alone", err)
					}
					if memory := h.memoryState(t, id); memory != seeded {
						t.Fatalf("memory after the outer rollback=%+v, want the seeded state %+v (coins after the refused isolated call=%d)", memory, seeded, probe.coinsAfterIso)
					}
					h.noFatal(t, h.runtime.Flush(ctx), "after the refused isolated transaction")
					if stored := h.mongoState(t, id); stored != seeded {
						t.Fatalf("Mongo after the refused isolated transaction=%+v, want %+v", stored, seeded)
					}
				})
			}
			// 之后同一实体照常写入并投影：没有分叉的版本。
			if _, err := h.scheduler.Request(ctx, nest.NewHandlerName("trade_buy"), id, nil); err != nil {
				t.Fatal(err)
			}
			h.noFatal(t, h.runtime.Flush(ctx), "after trade_buy")
			if memory, stored := h.memoryState(t, id), h.mongoState(t, id); memory != stored || stored.Coins != seeded.Coins-5 || stored.Version != seeded.Version+1 {
				t.Fatalf("after trade_buy memory=%+v Mongo=%+v seeded=%+v", memory, stored, seeded)
			}
			h.close(t, nil)
		})
	}
}

// RR-74 保持性 + RR-65：memory 外层没有快照，嵌套独立事务照常持久提交（WAL 多一条、投影进 Mongo）；外层随后失败时回复带
// ErrNestedTransactionCommitted，且不重排（handler 只执行一次）。
func TestGeneratedDataEngineNestedIsolatedWriteUnderMemoryOuterCommits(t *testing.T) {
	ctx, client := reloadTestClient(t)
	const base = 35100
	clearTraderDocuments(t, ctx, client, base, 1)
	h := newTradeFixtureRegistering(t, ctx, client, t.TempDir(), "nested-memory", nest.DurabilityStrict, false, func(h *tradeFixture) { registerNestedHandlers(h, nest.DurabilityStrict) })
	id := h.seed(t, base, 1)[0]
	seeded := h.mongoState(t, id)
	probe := &nestedProbe{form: nestedFormDAO}
	admitted := h.wal.Stats().Admitted
	_, err := h.scheduler.Request(ctx, nest.NewHandlerName("trade_nested_memory"), id, nest.Params{probe})
	if probe.isoRuns != 1 || probe.isoErr != nil {
		t.Fatalf("isolated transaction under a memory outer: runs=%d err=%v, want one committed run", probe.isoRuns, probe.isoErr)
	}
	if delta := h.wal.Stats().Admitted - admitted; delta != 1 {
		t.Fatalf("WAL admitted %d record(s) for the committed isolated transaction, want 1", delta)
	}
	if !errors.Is(err, errTradeRejected) || !errors.Is(err, nest.ErrNestedTransactionCommitted) {
		t.Fatalf("outer reply=%v, want the business error with ErrNestedTransactionCommitted", err)
	}
	want := tradeState{Coins: seeded.Coins - 100, Items: seeded.Items, Transfers: seeded.Transfers, Version: seeded.Version + 1}
	if memory := h.memoryState(t, id); memory != want {
		t.Fatalf("memory after the committed isolated transaction=%+v, want %+v", memory, want)
	}
	h.noFatal(t, h.runtime.Flush(ctx), "after the committed isolated transaction")
	if stored := h.mongoState(t, id); stored != want {
		t.Fatalf("Mongo after the committed isolated transaction=%+v, want %+v", stored, want)
	}
	h.close(t, nil)
}

var errFenceInjected = errors.New("persistflow: injected engine fence")

// RR-20260927-06：handler 执行期间引擎被 fence（这里由 handler 自己触发，等价于别的消息 fence 了引擎），消息自己的事务
// 在交给 committer 之前被拒绝：回复 ErrNestFenced、不带 ErrCommitIndeterminate，WAL 没有记录，修改已回滚，Mongo 不变。
func TestGeneratedDataEngineOuterCommitAfterFenceIsRefusedBeforeWAL(t *testing.T) {
	ctx, client := reloadTestClient(t)
	for _, policy := range []nest.DurabilityPolicy{nest.DurabilityStrict, nest.DurabilityPipelined} {
		t.Run(policy.String(), func(t *testing.T) {
			base := 35200 + int(policy)*10
			clearTraderDocuments(t, ctx, client, base, 1)
			h := newTradeFixtureRegistering(t, ctx, client, t.TempDir(), "fence-outer", policy, false, func(h *tradeFixture) {
				h.scheduler.MustRegisterHandlerWithMeta(nest.NewHandlerName("trade_fence_outer"), func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
					e := es[0].(*Trader)
					e.wallet.SetCoins(e.wallet.GetCoins() - 1)
					h.scheduler.Fence(errFenceInjected)
					return nil, nil
				}, nest.HandlerMeta{Rollback: nest.RollbackState, Durability: policy})
			})
			id := h.seed(t, base, 1)[0]
			seeded := h.mongoState(t, id)
			admitted := h.wal.Stats().Admitted
			_, err := h.scheduler.Request(ctx, nest.NewHandlerName("trade_fence_outer"), id, nil)
			if !errors.Is(err, nest.ErrNestFenced) || errors.Is(err, nest.ErrCommitIndeterminate) || errors.Is(err, nest.ErrAfterCommitFailed) {
				t.Fatalf("outer transaction after the fence replied %v, want ErrNestFenced without an indeterminate / committed sentinel", err)
			}
			if delta := h.wal.Stats().Admitted - admitted; delta != 0 {
				t.Fatalf("after the engine was fenced the outer transaction was still handed to the WAL: admitted %d record(s)", delta)
			}
			if memory := h.memoryState(t, id); memory != seeded {
				t.Fatalf("memory after the refused commit=%+v, want %+v", memory, seeded)
			}
			h.noFatal(t, h.runtime.Flush(ctx), "after the refused commit")
			if stored := h.mongoState(t, id); stored != seeded {
				t.Fatalf("Mongo after the refused commit=%+v, want %+v", stored, seeded)
			}
			if _, err := h.scheduler.Request(ctx, nest.NewHandlerName("trade_buy"), id, nil); !errors.Is(err, nest.ErrNestFenced) {
				t.Fatalf("request after the fence=%v, want ErrNestFenced", err)
			}
			h.close(t, nil)
		})
	}
}
