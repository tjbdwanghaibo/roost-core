package persistflow

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"github.com/tjbdwanghaibo/roost-core/mongo/driver"
	"github.com/tjbdwanghaibo/roost-core/nest"
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

// RR-20260926-10：本地删档（deletePersisted=true）只把 tombstone 写进 WAL。它投影之前重新加载
// 不能从旧库“复活”实体；投影之后加载得到 not found。走正式生成 Entity、Runtime 删档准入与真实 Mongo。
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
	if _, err := h.runtime.Repository.LoadEntity(ctx, ids[0], EntityKindTrader); !errors.Is(err, engine.ErrEntityAggregateNotFound) {
		t.Fatalf("reload after tombstone projection=%v, want ErrEntityAggregateNotFound", err)
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
