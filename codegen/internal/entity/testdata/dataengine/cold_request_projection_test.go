package persistflow

// OPEN-ITEMS B11：RR-20260926-10 / RR-20260926-25 的正式 Nest 入口端到端回归（RR-25 当时用的是不入库的临时测试）。
// 投影挂起 → 普通卸载（deletePersisted=false）→ 经 Nest RequestMulti 访问冷目标：慢阶段经正式 EntityRepository
// 冷加载，必须等在途投影放行后才读 Mongo，结果建立在挂起事务之后的状态上（不读到旧版本、不在快池阻塞加载）。
// 默认发送（RR-25：声明目标里有冷实体自动走慢阶段）与显式 SendOptionSlow 两条入口都覆盖；正式生成 DAO / Entity + 真实 Mongo。

import (
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/nest"
)

func TestGeneratedDataEngineColdRequestWaitsForPendingProjection(t *testing.T) {
	ctx, client := reloadTestClient(t)
	for policyIndex, policy := range []nest.DurabilityPolicy{nest.DurabilityAsync, nest.DurabilityStrict, nest.DurabilityPipelined} {
		for entryIndex, entry := range []struct {
			name string
			opts []nest.SendOpt
		}{{name: "default", opts: nil}, {name: "slow", opts: []nest.SendOpt{nest.SendOptionSlow()}}} {
			t.Run(policy.String()+"/"+entry.name, func(t *testing.T) {
				// 34000 段本文件独用：同包其他用例的 seed 段都在 1000～33000 之内。
				base := 34000 + policyIndex*100 + entryIndex*10
				clearTraderDocuments(t, ctx, client, base, 2)
				h := newTradeFixture(t, ctx, client, t.TempDir(), "coldrequest-"+entry.name, policy, false)
				ids := h.seed(t, base, 2)
				pause := h.gate.pause()
				defer pause.resume()
				// 事务 A 已提交到 WAL（调用方已得到完成），投影停在闸门上。
				if err := h.request("trade_transfer", ids...); err != nil {
					t.Fatal(err)
				}
				waitTradeRecord(t, ctx, pause, ids)
				want := make([]tradeState, len(ids))
				for i, id := range ids {
					trader := h.access.Manager().Get(id).(*Trader)
					want[i] = tradeState{trader.wallet.GetCoins(), trader.inventory.GetItems(), trader.wallet.GetTransfers() + 1, trader.wallet.DirtyTracker().Version() + 1}
				}
				want[0].Coins, want[0].Items = want[0].Coins-3, want[0].Items+1
				want[1].Coins, want[1].Items = want[1].Coins+3, want[1].Items-1
				for _, id := range ids {
					if err := h.access.Destroy(ctx, h.access.Manager().Get(id), entity.DestroyReasonCommon, false); err != nil {
						t.Fatalf("unload %d: %v", id, err)
					}
				}

				done := make(chan error, 1)
				go func() {
					_, err := h.scheduler.RequestMulti(ctx, nest.NewHandlerName("trade_transfer"), ids, nil, entry.opts...)
					done <- err
				}()
				select {
				case err := <-done:
					t.Fatalf("cold request finished before the pending projection was released: err=%v", err)
				case <-time.After(300 * time.Millisecond):
				}
				for _, id := range ids {
					if h.access.Manager().Get(id) != nil {
						t.Fatalf("entity %d published from Mongo while its projection was still pending", id)
					}
				}
				pause.resume()
				select {
				case err := <-done:
					if err != nil {
						t.Fatalf("cold request after the projection was released: %v", err)
					}
				case <-time.After(20 * time.Second):
					t.Fatal("cold request did not finish after the projection was released")
				}
				if err := h.runtime.Flush(ctx); err != nil {
					t.Fatal(err)
				}
				for i, id := range ids {
					h.assertState(t, id, want[i], true)
				}
				select {
				case fatal := <-h.fatal:
					t.Fatal(fatal)
				default:
				}
				h.close(t, nil)
			})
		}
	}
}
