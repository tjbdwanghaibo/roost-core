package persistflow

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"github.com/tjbdwanghaibo/roost-core/mongo/driver"
	"github.com/tjbdwanghaibo/roost-core/nest"
)

func TestGeneratedDataEngineReloadWaitsForProjection(t *testing.T) {
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" || os.Getenv("ROOST_GENERATED_PHASE") != "1" {
		t.Skip("generated phase 1 only")
	}
	if db := NewWalletDao().DbName(); !strings.HasPrefix(db, "roost_generated_it_") || db == "roost_generated_it_placeholder" {
		t.Fatal("refuse non-isolated database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, err := driver.NewClient(fmongo.DefaultConfig(uri), driver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close(context.Background())
	// The shared generated suite owns isolated database cleanup.
	RegisterEntity()
	for _, policy := range []nest.DurabilityPolicy{nest.DurabilityAsync, nest.DurabilityStrict, nest.DurabilityPipelined} {
		t.Run(policy.String(), func(t *testing.T) {
			h := newTradeFixture(t, ctx, client, t.TempDir(), "unload", policy, false)
			ids := h.seed(t, 31000+int(policy)*10, 2)
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

			// 取消的加载不得越过未投影事务读取旧 Mongo；不靠 sleep 判断是否进入屏障。
			canceled, stop := context.WithCancel(ctx)
			stop()
			if _, err := h.runtime.Repository.LoadEntity(canceled, ids[0], EntityKindTrader); err == nil {
				t.Fatal("reload crossed pending projection")
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
