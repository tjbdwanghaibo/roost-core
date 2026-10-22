package persistflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// reloadTestClient 连接隔离库；库名必须是生成脚本替换出的 roost_generated_it_*。
func reloadTestClient(t *testing.T) (context.Context, fmongo.IMongo) {
	t.Helper()
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" || os.Getenv("ROOST_GENERATED_PHASE") != "1" {
		t.Skip("generated phase 1 only")
	}
	if db := NewWalletDao().DbName(); !strings.HasPrefix(db, "roost_generated_it_") || db == "roost_generated_it_placeholder" {
		t.Fatal("refuse non-isolated database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	client, err := driver.NewClient(fmongo.DefaultConfig(uri), driver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	// The shared generated suite owns isolated database cleanup.
	RegisterEntity()
	return ctx, client
}

// clearTraderDocuments 删除本测试固定 seed ID 的上一轮文档，使测试可在同一隔离库重复运行
// （例如 -test.count=2）；其他测试的 ID 段不受影响。事务标记用新事务 ID，不会冲突。
func clearTraderDocuments(t *testing.T, ctx context.Context, client fmongo.IMongo, base, count int) {
	t.Helper()
	ids := make([]int64, count)
	for i := range ids {
		id, err := entity.BuildEntityID(int64(base+i), EntityKindTrader)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	for _, resource := range []string{"trade_wallets", "trade_inventories"} {
		if _, err := client.Database(NewWalletDao().DbName()).Collection(resource).DeleteMany(ctx, bson.M{"_id": bson.M{"$in": ids}}); err != nil {
			t.Fatal(err)
		}
	}
}

// RR-20260926-10：保留存档的卸载后，在 A 投影前重新加载必须等待 A 的投影，不能读 Mongo 旧版本。
// 断言用有截止时间的活 ctx：修前加载立刻成功（读到旧版本），修后在截止时间内等待而返回截止错误。
func TestGeneratedDataEngineReloadWaitsForProjection(t *testing.T) {
	ctx, client := reloadTestClient(t)
	for _, policy := range []nest.DurabilityPolicy{nest.DurabilityAsync, nest.DurabilityStrict, nest.DurabilityPipelined} {
		t.Run(policy.String(), func(t *testing.T) {
			base := 31000 + int(policy)*10
			clearTraderDocuments(t, ctx, client, base, 2)
			h := newTradeFixture(t, ctx, client, t.TempDir(), "unload", policy, false)
			ids := h.seed(t, base, 2)
			pause := h.gate.pause()
			defer pause.resume()
			// Transaction A: committed to WAL (and completed to the caller); its projection is held at the gate.
			if err := h.request("trade_transfer", ids...); err != nil {
				t.Fatal(err)
			}
			waitTradeRecord(t, ctx, pause, ids)
			before := h.access.Manager().Get(ids[0]).(*Trader)
			beforeCoins, beforeVersion := before.wallet.GetCoins(), before.wallet.DirtyTracker().Version()
			t.Logf("memory after A: coins=%d version=%d", beforeCoins, beforeVersion)
			// Ordinary unload (logout / eviction): deleteFromDB=false.
			for _, id := range ids {
				if err := h.access.Destroy(ctx, h.access.Manager().Get(id), entity.DestroyReasonCommon, false); err != nil {
					t.Fatalf("unload %d: %v", id, err)
				}
			}

			short, stop := context.WithTimeout(ctx, 200*time.Millisecond)
			_, err := h.runtime.Repository.LoadEntity(short, ids[0], EntityKindTrader)
			stop()
			if !errors.Is(err, context.DeadlineExceeded) || h.access.Manager().Get(ids[0]) != nil {
				t.Fatalf("reload crossed pending projection: err=%v", err)
			}
			pause.resume()
			for _, id := range ids {
				if _, err := h.runtime.Repository.LoadEntity(ctx, id, EntityKindTrader); err != nil {
					t.Fatal(err)
				}
			}
			after := h.access.Manager().Get(ids[0]).(*Trader)
			if after.wallet.GetCoins() != beforeCoins || after.wallet.DirtyTracker().Version() != beforeVersion {
				t.Fatalf("stale reload: coins=%d version=%d", after.wallet.GetCoins(), after.wallet.DirtyTracker().Version())
			}
			if err := h.request("trade_transfer", ids...); err != nil {
				t.Fatal(err)
			}
			if err := h.runtime.Flush(ctx); err != nil {
				t.Fatal(err)
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

// entity.unload（Runtime.UnloadEntity）走正式生成 Entity / DAO、Nest 事务、文件 WAL 与真实 Mongo：
//   - 已提交、投影卡在门上的修改先落库再卸载；卸载期间写到该实体的事务以可重试的 ErrEntityUnloading 拒绝并回滚，
//     其他实体照常写；卸载后冷加载读回卸载前的内存状态，之后照常写、投影无冲突。
//   - 投影在 timeout 内没完成：ErrEntityUnloadTimeout，实体留在内存、屏障撤销，之后的写入照常。
//
// strict / async / pipelined 各跑一次。
func TestGeneratedDataEngineAdminUnload(t *testing.T) {
	ctx, client := reloadTestClient(t)
	for _, policy := range []nest.DurabilityPolicy{nest.DurabilityStrict, nest.DurabilityAsync, nest.DurabilityPipelined} {
		t.Run(policy.String(), func(t *testing.T) {
			base := 34000 + int(policy)*10
			clearTraderDocuments(t, ctx, client, base, 2)
			h := newTradeFixture(t, ctx, client, t.TempDir(), "admin-unload", policy, false)
			ids := h.seed(t, base, 2)
			target, other := ids[0], ids[1]

			// 超时：一笔转账卡在投影门上，短 timeout 的卸载失败，实体不受影响。
			pause := h.gate.pause()
			if err := h.request("trade_transfer", ids...); err != nil {
				t.Fatal(err)
			}
			waitTradeRecord(t, ctx, pause, ids)
			short, stop := context.WithTimeout(ctx, 200*time.Millisecond)
			_, err := h.runtime.UnloadEntity(short, target)
			stop()
			if !errors.Is(err, engine.ErrEntityUnloadTimeout) || h.access.Manager().Get(target) == nil {
				t.Fatalf("unload with a pending projection = %v (resident=%v), want ErrEntityUnloadTimeout and the entity kept", err, h.access.Manager().Get(target) != nil)
			}
			if h.runtime.Projector.Stats().UnloadingEntities != 0 {
				t.Fatal("the barrier outlived the timed-out unload")
			}
			pause.resume()
			h.noFatal(t, h.runtime.Flush(ctx), "after the timed-out unload")

			// 排空后卸载。
			pause = h.gate.pause()
			defer pause.resume()
			if err := h.request("trade_transfer", ids...); err != nil {
				t.Fatal(err)
			}
			waitTradeRecord(t, ctx, pause, ids)
			before := h.memoryState(t, target)
			unloaded := make(chan error, 1)
			go func() {
				bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
				defer cancel()
				result, err := h.runtime.UnloadEntity(bounded, target)
				if err == nil && (!result.Resident || !result.Unloaded || result.Drained == 0) {
					err = fmt.Errorf("unload result %+v", result)
				}
				unloaded <- err
			}()
			deadline := time.Now().Add(5 * time.Second)
			for h.runtime.Projector.Stats().UnloadingEntities != 1 {
				if time.Now().After(deadline) {
					t.Fatal("the unload barrier was not raised")
				}
				time.Sleep(time.Millisecond)
			}
			err = h.request("trade_buy", target)
			if !errors.Is(err, coredata.ErrEntityUnloading) || !errors.Is(err, nest.ErrCommitRejected) {
				t.Fatalf("write during unload = %v, want ErrEntityUnloading", err)
			}
			if got := h.memoryState(t, target); got != before {
				t.Fatalf("the refused write was not rolled back: %+v, was %+v", got, before)
			}
			if err := h.request("trade_buy", other); err != nil {
				t.Fatalf("another entity during the unload: %v", err)
			}
			pause.resume()
			if err := <-unloaded; err != nil {
				t.Fatal(err)
			}
			if h.access.Manager().Get(target) != nil {
				t.Fatal("the entity is still resident after entity.unload")
			}
			if got := h.mongoState(t, target); got != before {
				t.Fatalf("Mongo after unload = %+v, memory before = %+v", got, before)
			}
			// 冷加载：读回卸载前的状态，写入照常。
			if err := h.request("trade_buy", target); err != nil {
				t.Fatalf("write after unload: %v", err)
			}
			after := h.memoryState(t, target)
			if after.Coins != before.Coins-5 || after.Version != before.Version+1 {
				t.Fatalf("after cold load and write = %+v, before = %+v", after, before)
			}
			h.noFatal(t, h.runtime.Flush(ctx), "after the unload")
			if got := h.mongoState(t, target); got != after {
				t.Fatalf("Mongo = %+v, memory = %+v", got, after)
			}
			if stats := h.runtime.Projector.Stats(); stats.UnloadingEntities != 0 || stats.UnloadAdmissionRejected != 1 {
				t.Fatalf("barrier stats = %+v", stats)
			}
			h.close(t, nil)
		})
	}
}

// RR-20260926-10：本地删档（deletePersisted=true）只把 tombstone 写进 WAL。它投影之前重新加载
// 不能从旧库“复活”实体；投影之后加载得到“删过档”（ErrEntityAggregateDeleted，D10）。走正式生成 Entity、Runtime 删档准入与真实 Mongo。
func TestGeneratedDataEngineReloadDoesNotResurrectPendingTombstone(t *testing.T) {
	ctx, client := reloadTestClient(t)
	const base = 32000
	clearTraderDocuments(t, ctx, client, base, 2)
	h := newTradeFixture(t, ctx, client, t.TempDir(), "tombstone", nest.DurabilityStrict, false)
	ids := h.seed(t, base, 2)
	pause := h.gate.pause()
	defer pause.resume()
	if err := h.request("trade_transfer", ids...); err != nil {
		t.Fatal(err)
	}
	waitTradeRecord(t, ctx, pause, ids)
	// A 卡在投影门上；删档 tombstone 排在它后面进入 WAL。
	if err := h.access.Destroy(ctx, h.access.Manager().Get(ids[0]), entity.DestroyReasonCommon, true); err != nil {
		t.Fatalf("delete %d: %v", ids[0], err)
	}
	short, stop := context.WithTimeout(ctx, 200*time.Millisecond)
	value, err := h.runtime.Repository.LoadEntity(short, ids[0], EntityKindTrader)
	stop()
	if value != nil || !errors.Is(err, context.DeadlineExceeded) || h.access.Manager().Get(ids[0]) != nil {
		t.Fatalf("reload before tombstone projection resurrected entity: value=%v err=%v", value, err)
	}
	pause.resume()
	if _, err := h.runtime.Repository.LoadEntity(ctx, ids[0], EntityKindTrader); !errors.Is(err, engine.ErrEntityAggregateDeleted) {
		t.Fatalf("reload after tombstone projection=%v, want ErrEntityAggregateDeleted", err)
	}
	if h.access.Manager().Get(ids[0]) != nil {
		t.Fatal("deleted entity published after reload")
	}
	if err := h.runtime.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case fatal := <-h.fatal:
		t.Fatal(fatal)
	default:
	}
	h.close(t, nil)
}

// 删档后同 ID 重建（D10，维护者 2026-10-17 决定方案 B，范围只守 GetOrCreate）。
// Destroy(deletePersisted=true) 留下墓碑后，冷加载得到“删过档”（不是 not found）：生成 lifecycle 的 GetOrCreate
// 不再去创建，发往这个 ID 的 Nest 请求回复 entity_deleted（码 3）；什么也不进 WAL，墓碑不变，投影器不 fence，
// 进程继续服务其他实体；冷重启照常起来，删过档的 ID 仍然不可用。
// 直接 Create（handler 内或 lifecycle Create）不读存储，不在本用例的保护范围内：它必须用 id-gen 发的新 ID，
// 复用删过档的 ID 仍会在投影时版本冲突、fence（KNOWN-LIMITS“删档后同 ID 重建”的约束与人工处置）。
// 走正式生成 Entity、Runtime 删档准入、文件 WAL 与真实 Mongo；strict / async / pipelined 各跑一次。
//
// 修前（bdc9003a）同一流程：重建后的首个 Put expected=0 next=1 撞上墓碑，ErrProjectionConflict、投影器 fence，
// 冷重启的启动投影恢复再次失败（KNOWN-LIMITS 原登记、DESIGN-TODO D10）。
func TestGeneratedDataEngineRecreateAfterDeleteSameID(t *testing.T) {
	ctx, client := reloadTestClient(t)
	for _, policy := range []nest.DurabilityPolicy{nest.DurabilityStrict, nest.DurabilityAsync, nest.DurabilityPipelined} {
		t.Run(policy.String(), func(t *testing.T) {
			base := 33000 + int(policy)*10
			clearTraderDocuments(t, ctx, client, base, 3)
			root := t.TempDir()
			h := newTradeFixture(t, ctx, client, root, "recreate", policy, false)
			ids := h.seed(t, base, 2)
			id, other := ids[0], ids[1]
			if err := h.request("trade_buy", id); err != nil {
				t.Fatal(err)
			}
			h.noFatal(t, h.runtime.Flush(ctx), "first life")
			first := h.mongoState(t, id)

			if err := h.access.Destroy(ctx, h.access.Manager().Get(id), entity.DestroyReasonCommon, true); err != nil {
				t.Fatalf("delete %d: %v", id, err)
			}
			h.noFatal(t, h.runtime.Flush(ctx), "delete")
			tombstone := readTombstone(t, ctx, h, id)
			if !tombstone.Deleted || tombstone.Version <= first.Version || tombstone.DeletedAt.IsZero() || tombstone.LastTx == "" {
				t.Fatalf("tombstone=%+v first=%+v", tombstone, first)
			}
			t.Logf("tombstone: %+v", tombstone)

			// 冷加载：删过档是独立哨兵，不满足 not found（“不存在就创建”的代码不能把它当成可创建）。
			_, err := h.runtime.Repository.LoadEntity(ctx, id, EntityKindTrader)
			if !errors.Is(err, engine.ErrEntityAggregateDeleted) || !errors.Is(err, entity.ErrAuthorityEntityDeleted) || errors.Is(err, engine.ErrEntityAggregateNotFound) {
				t.Fatalf("load deleted=%v, want ErrEntityAggregateDeleted only", err)
			}
			admitted := h.wal.Stats().Admitted

			// 生成 lifecycle 的 GetOrCreate（同一判断：只有确切的 not found 才创建）不重建。
			if _, created, err := lifecycleGetOrCreate(ctx, h.access, id); created || !errors.Is(err, entity.ErrAuthorityEntityDeleted) {
				t.Fatalf("GetOrCreate deleted: created=%v err=%v, want ErrAuthorityEntityDeleted", created, err)
			}
			// 发往删过档 ID 的 Nest 请求：冷加载得到删过档，客户端码 entity_deleted。
			err = h.request("trade_buy", id)
			if !errors.Is(err, entity.ErrAuthorityEntityDeleted) {
				t.Fatalf("request to a deleted id: %v, want ErrAuthorityEntityDeleted", err)
			}
			if code, _ := errcode.ClientError(err); code != errcode.CodeEntityDeleted {
				t.Fatalf("client code=%d, want CodeEntityDeleted=%d (err=%v)", code, errcode.CodeEntityDeleted, err)
			}
			if h.access.Manager().Get(id) != nil {
				t.Fatal("a deleted id was published again")
			}
			if delta := h.wal.Stats().Admitted - admitted; delta != 0 {
				t.Fatalf("refused recreate handed %d record(s) to the WAL", delta)
			}

			// 进程继续服务其他实体：已有实体照常写、新 ID 照常创建，没有 fence。
			if err := h.request("trade_buy", other); err != nil {
				t.Fatalf("other entity after the failed recreate: %v", err)
			}
			fresh, err := entity.BuildEntityID(int64(base+2), EntityKindTrader)
			if err != nil {
				t.Fatal(err)
			}
			if _, created, err := lifecycleGetOrCreate(ctx, h.access, fresh); err != nil || !created {
				t.Fatalf("GetOrCreate of a never-used id: created=%v err=%v", created, err)
			}
			if _, err := h.scheduler.Request(ctx, nest.NewHandlerName("trade_seed"), fresh, nil); err != nil {
				t.Fatal(err)
			}
			h.noFatal(t, h.runtime.Flush(ctx), "after the failed recreate")
			if h.scheduler.FenceError() != nil {
				t.Fatalf("engine fenced: %v", h.scheduler.FenceError())
			}
			if got := readTombstone(t, ctx, h, id); got != tombstone {
				t.Fatalf("the tombstone changed: %+v, was %+v", got, tombstone)
			}
			otherState, freshState := h.mongoState(t, other), h.mongoState(t, fresh)
			h.close(t, nil)

			// 冷重启：新 Runtime 打开同一 WAL 目录与库，启动投影恢复不遇到冲突记录。删过档的 ID 仍然不可用，其他实体照常加载与写。
			restarted := newTradeFixture(t, ctx, client, root, "recreate", policy, false)
			if _, err := restarted.runtime.Repository.LoadEntity(ctx, id, EntityKindTrader); !errors.Is(err, engine.ErrEntityAggregateDeleted) {
				t.Fatalf("reload deleted after restart: %v", err)
			}
			if _, _, err := lifecycleGetOrCreate(ctx, restarted.access, id); !errors.Is(err, entity.ErrAuthorityEntityDeleted) {
				t.Fatalf("GetOrCreate deleted after restart: %v", err)
			}
			for _, live := range []struct {
				id   int64
				want tradeState
			}{{other, otherState}, {fresh, freshState}} {
				if _, err := restarted.runtime.Repository.LoadEntity(ctx, live.id, EntityKindTrader); err != nil {
					t.Fatalf("reload %d after restart: %v", live.id, err)
				}
				if got := restarted.memoryState(t, live.id); got != live.want {
					t.Fatalf("reloaded %d=%+v want=%+v", live.id, got, live.want)
				}
			}
			if err := restarted.request("trade_buy", other); err != nil {
				t.Fatal(err)
			}
			restarted.noFatal(t, restarted.runtime.Flush(ctx), "after restart")
			if got := restarted.mongoState(t, other); got.Coins != otherState.Coins-5 || got.Version != otherState.Version+1 {
				t.Fatalf("after restart mongo=%+v before=%+v", got, otherState)
			}
			restarted.close(t, nil)
		})
	}
}

// storedTombstone 是墓碑保留的元数据（D10 审计结论见 DESIGN-TODO）。
type storedTombstone struct {
	Deleted   bool      `bson:"_deleted"`
	DeletedAt time.Time `bson:"_deleted_at"`
	Version   uint64    `bson:"_version"`
	LastTx    string    `bson:"_last_tx"`
	Coins     int64     `bson:"coins"`
}

func readTombstone(t *testing.T, ctx context.Context, h *tradeFixture, id int64) storedTombstone {
	t.Helper()
	var tombstone storedTombstone
	if err := h.client.Database(h.database).Collection("trade_wallets").FindOne(ctx, bson.M{"_id": id}, &tombstone); err != nil {
		t.Fatal(err)
	}
	return tombstone
}

// lifecycleGetOrCreate 与生成 lifecycle 的 GetOrCreate 同一判断（codegen/internal/roost/add_workflow.go）：
// 只有确切的 not found 才创建；删过档、读失败原样返回。
func lifecycleGetOrCreate(ctx context.Context, access *entity.ManagerAccess, id int64) (entity.IThreadSafeEntity, bool, error) {
	value, err := access.Get(ctx, id, entity.MustEntityCategoryOfKind(EntityKindTrader))
	if err == nil && value != nil {
		return value, false, nil
	}
	if err != nil && !errors.Is(err, engine.ErrEntityAggregateNotFound) {
		return nil, false, err
	}
	value, err = access.Create(&entity.EntityCreateParam{IsCreate: true, Kind: EntityKindTrader, Id: id})
	return value, err == nil, err
}
