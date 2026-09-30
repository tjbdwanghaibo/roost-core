package persistflow

// B27 第 2 批：handler 内新建实体在真实 WAL group-commit + Mongo 链路上的端到端。
//   - RR-20260926-35 的未验证项“真实 WAL group-commit + 真实 Mongo 下的端到端（pipelined 新建实体、断电恢复）未跑”：
//     handler 内经 ManagerAccess.Create（生成 Lifecycle 的 Create / GetOrCreate 走同一入口）新建 Trader 并写生成 DAO，
//     提交后新实体常驻并投影进 Mongo；handler 失败时撤销发布、WAL 没有记录、Mongo 没有文档。断电做不了，用“ack 丢失、
//     进程重启”代替：新建实体的记录留在 WAL，重启时启动恢复投影，之后从 Mongo 重载得到新建时的值。
//   - RR-20260926-48 的未验证项“未在真实 WAL + Mongo 的端到端环境里构造交叉创建”：两条消息各自先建 X 再建 Y / 先建 Y 再建 X，
//     两侧都持有第一个新实体后才建第二个（首轮必然对称冲突）；一方成功、一方 ErrEntityExists，没有请求以锁超时耗尽，
//     胜者建的两份文档各投影一次。

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const createdCoins, createdItems = 500, 50

// createProbe 是 trade_create 的参数：在 handler 内依次新建 ids，fail 时在新建之后返回业务错误。
type createProbe struct {
	ids  []int64
	fail bool
}

func registerCreateHandlers(h *tradeFixture, policy nest.DurabilityPolicy) {
	h.scheduler.MustRegisterHandlerWithMeta(nest.NewHandlerName("trade_create"), func(_ []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
		probe := params[0].(*createProbe)
		for _, id := range probe.ids {
			created, err := h.access.Create(&entity.EntityCreateParam{IsCreate: true, Kind: EntityKindTrader, Id: id})
			if err != nil {
				return nil, err
			}
			trader := created.(*Trader)
			trader.wallet.SetCoins(createdCoins)
			trader.inventory.SetItems(createdItems)
		}
		if probe.fail {
			return nil, errTradeRejected
		}
		return "ok", nil
	}, nest.HandlerMeta{Rollback: nest.RollbackState, Durability: policy})
}

func traderID(t *testing.T, raw int) int64 {
	t.Helper()
	id, err := entity.BuildEntityID(int64(raw), EntityKindTrader)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (h *tradeFixture) assertNoDocuments(t *testing.T, id int64) {
	t.Helper()
	for _, collection := range []string{"trade_wallets", "trade_inventories"} {
		var doc bson.M
		err := h.client.Database(h.database).Collection(collection).FindOne(h.ctx, bson.M{"_id": id}, &doc)
		if !errors.Is(err, fmongo.ErrNotFound) {
			t.Fatalf("%s/%d exists in Mongo (err=%v doc=%v), want no document", collection, id, err, doc)
		}
	}
}

func TestGeneratedDataEngineCreateInHandlerIsTransactional(t *testing.T) {
	ctx, client := reloadTestClient(t)
	for _, policy := range []nest.DurabilityPolicy{nest.DurabilityAsync, nest.DurabilityStrict, nest.DurabilityPipelined} {
		t.Run(policy.String(), func(t *testing.T) {
			base := 36000 + int(policy)*10
			clearTraderDocuments(t, ctx, client, base, 4)
			h := newTradeFixtureRegistering(t, ctx, client, t.TempDir(), "create", policy, false, func(h *tradeFixture) { registerCreateHandlers(h, policy) })
			caller := h.seed(t, base, 1)[0]
			committed, rolledBack := traderID(t, base+1), traderID(t, base+2)

			// 提交：新实体常驻，投影后 Mongo 有两份文档，版本 1。
			if _, err := h.scheduler.Request(ctx, nest.NewHandlerName("trade_create"), caller, nest.Params{&createProbe{ids: []int64{committed}}}); err != nil {
				t.Fatalf("create inside the handler: %v", err)
			}
			if h.access.Manager().Get(committed) == nil {
				t.Fatal("the entity created inside the committed handler is not resident")
			}
			h.noFatal(t, h.runtime.Flush(ctx), "after the committed create")
			h.assertState(t, committed, tradeState{Coins: createdCoins, Items: createdItems, Version: 1}, true)

			// 回滚：handler 失败，新实体撤销发布，WAL 没有记录，Mongo 没有文档。
			admitted := h.wal.Stats().Admitted
			_, err := h.scheduler.Request(ctx, nest.NewHandlerName("trade_create"), caller, nest.Params{&createProbe{ids: []int64{rolledBack}, fail: true}})
			if !errors.Is(err, errTradeRejected) {
				t.Fatalf("failed handler replied %v", err)
			}
			if h.access.Manager().Get(rolledBack) != nil {
				t.Fatal("the entity created inside the rolled-back handler is still published")
			}
			if delta := h.wal.Stats().Admitted - admitted; delta != 0 {
				t.Fatalf("the rolled-back handler handed %d record(s) to the WAL", delta)
			}
			h.noFatal(t, h.runtime.Flush(ctx), "after the rolled-back create")
			h.assertNoDocuments(t, rolledBack)
			// 同一 ID 之后可以重新创建并提交。
			if _, err := h.scheduler.Request(ctx, nest.NewHandlerName("trade_create"), caller, nest.Params{&createProbe{ids: []int64{rolledBack}}}); err != nil {
				t.Fatalf("re-create after the rollback: %v", err)
			}
			h.noFatal(t, h.runtime.Flush(ctx), "after the re-create")
			h.assertState(t, rolledBack, tradeState{Coins: createdCoins, Items: createdItems, Version: 1}, true)
			h.close(t, nil)
		})
	}
}

// RR-35 “断电恢复”的可做部分：新建实体的记录已在 WAL、ack 丢失，进程重启后启动恢复把它投影进 Mongo，重载得到新建时的值。
func TestGeneratedDataEngineCreatedEntityRecoversAfterLostAckRestart(t *testing.T) {
	ctx, client := reloadTestClient(t)
	for _, policy := range []nest.DurabilityPolicy{nest.DurabilityStrict, nest.DurabilityPipelined} {
		t.Run(policy.String(), func(t *testing.T) {
			base := 36100 + int(policy)*10
			clearTraderDocuments(t, ctx, client, base, 2)
			root := t.TempDir()
			h := newTradeFixtureRegistering(t, ctx, client, root, "create-recover", policy, true, func(h *tradeFixture) { registerCreateHandlers(h, policy) })
			caller := h.seed(t, base, 1)[0]
			created := traderID(t, base+1)
			pause := h.gate.pause()
			defer pause.resume()
			h.loseAck.Store(true)
			if _, err := h.scheduler.Request(ctx, nest.NewHandlerName("trade_create"), caller, nest.Params{&createProbe{ids: []int64{created}}}); err != nil {
				t.Fatalf("create inside the handler: %v", err)
			}
			select {
			case record := <-pause.entered:
				if len(record.Mutations) != 2 {
					t.Fatalf("the create record carries %d mutation(s), want the two generated DAOs", len(record.Mutations))
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			pause.resume()
			select {
			case <-h.ackFailed:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			h.assertState(t, created, tradeState{Coins: createdCoins, Items: createdItems, Version: 1}, true)
			h.close(t, errTradeAckLost)
			// ack 丢失后 Runtime 已关闭 WAL；像 testTradeLostAck 一样重新打开同一目录核对未 ack 记录，再关掉让“重启”接管。
			reopened, err := nestwal.Open(h.options)
			if err != nil {
				t.Fatal(err)
			}
			remaining := 0
			if err := reopened.Replay(ctx, func(_ nest.CommitFence, r nest.CommitRecord) error {
				remaining++
				if len(r.Mutations) != 2 {
					t.Fatalf("unacked record carries %d mutation(s), want the two generated DAOs of the created entity", len(r.Mutations))
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := reopened.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			if remaining != 1 {
				t.Fatalf("unacked WAL records=%d, want the create record only", remaining)
			}

			// “重启”：同一 WAL 目录、新的 Runtime / Nest / EntityManager，启动恢复重放那条记录。
			restarted := newTradeFixture(t, ctx, client, root, "create-recover", policy, false)
			if stats := restarted.runtime.Projector.Stats(); stats.Projected != 1 {
				t.Fatalf("startup recovery projected %d record(s), want 1: %+v", stats.Projected, stats)
			}
			value, err := restarted.runtime.Repository.LoadEntity(ctx, created, EntityKindTrader)
			if err != nil {
				t.Fatal(err)
			}
			if trader := value.(*Trader); trader.wallet.GetCoins() != createdCoins || trader.inventory.GetItems() != createdItems {
				t.Fatalf("reloaded created entity coins=%d items=%d", trader.wallet.GetCoins(), trader.inventory.GetItems())
			}
			restarted.assertState(t, created, tradeState{Coins: createdCoins, Items: createdItems, Version: 1}, true)
			if remaining := restarted.records(t); len(remaining) != 0 {
				t.Fatalf("pending WAL after recovery=%d", len(remaining))
			}
			restarted.close(t, nil)
		})
	}
}

func TestGeneratedDataEngineCrossCreateResolvesOnRealWAL(t *testing.T) {
	ctx, client := reloadTestClient(t)
	for _, policy := range []nest.DurabilityPolicy{nest.DurabilityStrict, nest.DurabilityPipelined} {
		t.Run(policy.String(), func(t *testing.T) {
			base := 36200 + int(policy)*10
			clearTraderDocuments(t, ctx, client, base, 4)
			x, y := traderID(t, base+2), traderID(t, base+3)
			var attempts [2]atomic.Int64
			held := make(chan struct{}, 2)
			proceed := make(chan struct{})
			h := newTradeFixtureRegistering(t, ctx, client, t.TempDir(), "cross-create", policy, false, func(h *tradeFixture) {
				for side := range 2 {
					first, second := x, y
					if side == 1 {
						first, second = y, x
					}
					h.scheduler.MustRegisterHandlerWithMeta(nest.NewHandlerName("trade_cross_"+strconv.Itoa(side)), func([]entity.IThreadSafeEntity, []any, ...nest.HandlerOption) (any, error) {
						n := attempts[side].Add(1)
						for i, id := range []int64{first, second} {
							created, err := h.access.Create(&entity.EntityCreateParam{IsCreate: true, Kind: EntityKindTrader, Id: id})
							if err != nil {
								return nil, err
							}
							trader := created.(*Trader)
							trader.wallet.SetCoins(createdCoins + int64(side))
							trader.inventory.SetItems(createdItems)
							if i == 0 && n == 1 {
								held <- struct{}{} // 两侧都持有自己的第一个新实体后才建第二个：首轮必然对称冲突
								<-proceed
							}
						}
						return "ok", nil
					}, nest.HandlerMeta{Rollback: nest.RollbackState, Durability: policy})
				}
			})
			callers := h.seed(t, base, 2)
			type result struct {
				side int
				ret  any
				err  error
			}
			out := make(chan result, 2)
			var group sync.WaitGroup
			for side := range 2 {
				group.Go(func() {
					ret, err := h.scheduler.Request(ctx, nest.NewHandlerName("trade_cross_"+strconv.Itoa(side)), callers[side], nil)
					out <- result{side: side, ret: ret, err: err}
				})
			}
			for range 2 {
				select {
				case <-held:
				case <-time.After(10 * time.Second):
					t.Fatal("not every handler reached its first created entity")
				}
			}
			close(proceed)
			group.Wait()
			close(out)
			winners, losers, winner := 0, 0, -1
			for r := range out {
				switch {
				case r.err == nil && r.ret == "ok":
					winners++
					winner = r.side
				case errors.Is(r.err, entity.ErrEntityExists):
					losers++
				case errors.Is(r.err, nest.ErrLockTimeout):
					t.Fatalf("side %d exhausted the requeue budget after %d attempts: %v", r.side, attempts[r.side].Load(), r.err)
				default:
					t.Fatalf("side %d: want ok or ErrEntityExists, got ret=%v err=%v", r.side, r.ret, r.err)
				}
			}
			if winners != 1 || losers != 1 {
				t.Fatalf("winners=%d losers=%d, want one each (attempts=%d/%d)", winners, losers, attempts[0].Load(), attempts[1].Load())
			}
			t.Logf("policy=%s winner=side %d attempts=%d/%d", policy, winner, attempts[0].Load(), attempts[1].Load())
			h.noFatal(t, h.runtime.Flush(ctx), "after the cross creates")
			for _, id := range []int64{x, y} {
				if h.access.Manager().Get(id) == nil {
					t.Fatalf("created entity %d is not resident", id)
				}
				h.assertState(t, id, tradeState{Coins: createdCoins + int64(winner), Items: createdItems, Version: 1}, true)
			}
			if remaining := h.records(t); len(remaining) != 0 {
				t.Fatalf("remaining WAL records=%d", len(remaining))
			}
			h.close(t, nil)
		})
	}
}
