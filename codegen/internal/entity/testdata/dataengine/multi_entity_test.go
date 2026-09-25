package persistflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"github.com/tjbdwanghaibo/roost-core/mongo/driver"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestGeneratedDataEngineMultiEntity(t *testing.T) {
	uri, phaseText := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI"), os.Getenv("ROOST_GENERATED_PHASE")
	if uri == "" || phaseText == "" || phaseText == "cleanup" {
		t.Skip("run scripts/test-dataengine-generated.sh")
	}
	phase, err := strconv.Atoi(phaseText)
	if err != nil || phase < 1 || phase > 3 {
		t.Fatal("invalid phase")
	}
	if database := NewWalletDao().DbName(); !strings.HasPrefix(database, "roost_generated_it_") || database == "roost_generated_it_placeholder" || NewInventoryDao().DbName() != database {
		t.Fatal("refuse non-isolated or mismatched databases")
	}
	root := os.Getenv("ROOST_GENERATED_WAL_DIR")
	if root == "" {
		t.Fatal("missing WAL root")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
	defer cancel()
	client, err := driver.NewClient(fmongo.DefaultConfig(uri), driver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close(context.Background())
	RegisterEntity()
	for policyIndex, policy := range []nest.DurabilityPolicy{nest.DurabilityAsync, nest.DurabilityStrict, nest.DurabilityPipelined} {
		t.Run(policy.String(), func(t *testing.T) {
			t.Run("shared_entity_trades", func(t *testing.T) { testTradeProcesses(t, ctx, client, root, policy, policyIndex, phase) })
			t.Run("lost_ack_restart", func(t *testing.T) { testTradeLostAck(t, ctx, client, root, policy, policyIndex, phase) })
			if phase == 1 {
				t.Run("backpressure_rollback", func(t *testing.T) { testTradeBackpressure(t, ctx, client, root, policy, policyIndex) })
				t.Run("last_document_conflict", func(t *testing.T) { testTradeConflict(t, ctx, client, root, policy, policyIndex) })
			}
		})
	}
}

// 八个并发请求方沿环形转移资源；相邻交易共享 Entity，末尾请求的 ID 顺序与排序相反。
func testTradeProcesses(t *testing.T, ctx context.Context, client fmongo.IMongo, root string, policy nest.DurabilityPolicy, policyIndex, phase int) {
	const count, rounds = 8, 12
	h := newTradeFixture(t, ctx, client, root, "normal", policy, false)
	var ids []int64
	if phase == 1 {
		ids = h.seed(t, 1000+policyIndex*100, count)
	} else {
		for i := 0; i < count; i++ {
			id, err := entity.BuildEntityID(int64(1000+policyIndex*100+i), EntityKindTrader)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = h.runtime.Repository.LoadEntity(ctx, id, EntityKindTrader); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
	}
	completedPhases := phase - 1
	expected := func(index int) tradeState {
		state := tradeState{Coins: 10000, Items: 1000, Transfers: int64(completedPhases * rounds * 2), Version: uint64(1 + completedPhases*rounds*2)}
		if index == 0 {
			state.Coins -= int64(5 * completedPhases)
			state.Items += int64(completedPhases)
			state.Version += uint64(completedPhases)
		}
		return state
	}
	for i, id := range ids {
		h.assertState(t, id, expected(i), true)
	}
	if phase < 3 {
		failures := make(chan error, count)
		var group sync.WaitGroup
		started := time.Now()
		for i := range ids {
			group.Go(func() {
				for range rounds {
					if err := h.request("trade_transfer", ids[i], ids[(i+1)%count]); err != nil {
						failures <- err
						return
					}
				}
			})
		}
		group.Wait()
		close(failures)
		for err := range failures {
			t.Error(err)
		}
		if t.Failed() {
			return
		}
		if err := h.request("trade_buy", ids[0]); err != nil {
			t.Fatal(err)
		}
		if err := h.runtime.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		completedPhases++
		t.Logf("phase=%d policy=%s shared_entities=%d DAOs/entity=2 transactions=%d four_document_trades=%d elapsed=%s", phase, policy, count, count*rounds+1, count*rounds, time.Since(started))
	}
	for _, name := range []string{"trade_reject", "trade_panic"} {
		before := h.runtime.Projector.Stats().Committed
		err := h.request(name, ids[1], ids[0])
		if name == "trade_reject" && !errors.Is(err, errTradeRejected) || name == "trade_panic" && err == nil {
			t.Fatalf("%s returned %v", name, err)
		}
		if err = h.runtime.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		if got := h.runtime.Projector.Stats().Committed; got != before {
			t.Fatalf("rejected business admitted WAL: before=%d after=%d", before, got)
		}
		for i, id := range ids {
			h.assertState(t, id, expected(i), true)
		}
	}
	if remaining := h.records(t); len(remaining) != 0 {
		t.Fatalf("remaining records=%d", len(remaining))
	}
	h.close(t, nil)
}

// 用正式 Nest 生成四 mutation 事务；第一次进程留下真实未 ack WAL，下一进程启动自动恢复。
func testTradeLostAck(t *testing.T, ctx context.Context, client fmongo.IMongo, root string, policy nest.DurabilityPolicy, policyIndex, phase int) {
	h := newTradeFixture(t, ctx, client, root, "lost-ack", policy, phase == 1)
	var ids []int64
	if phase == 1 {
		ids = h.seed(t, 10000+policyIndex*100, 2)
	} else {
		for i := 0; i < 2; i++ {
			id, err := entity.BuildEntityID(int64(10000+policyIndex*100+i), EntityKindTrader)
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
			if _, err = h.runtime.Repository.LoadEntity(ctx, id, EntityKindTrader); err != nil {
				t.Fatal(err)
			}
		}
	}
	want := func(i, transfers int) tradeState {
		sign := int64(1)
		if i == 0 {
			sign = -1
		}
		return tradeState{Coins: 10000 + sign*3*int64(transfers), Items: 1000 - sign*int64(transfers), Transfers: int64(transfers), Version: uint64(1 + transfers)}
	}
	if phase == 1 {
		pause := h.gate.pause()
		defer pause.resume()
		h.loseAck.Store(true)
		if err := h.request("trade_transfer", ids...); err != nil {
			t.Fatal(err)
		}
		record := waitTradeRecord(t, ctx, pause, ids)
		if records := h.records(t); len(records) != 1 || records[0].ID != record.ID {
			t.Fatalf("WAL did not retain exactly the four-DAO transaction")
		}
		pause.resume()
		select {
		case <-h.ackFailed:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		for i, id := range ids {
			h.assertState(t, id, want(i, 1), true)
		}
		h.close(t, errTradeAckLost)
		reopened, err := nestwal.Open(h.options)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close(context.Background())
		remaining := 0
		if err := reopened.Replay(ctx, func(_ nest.CommitFence, r nest.CommitRecord) error {
			remaining++
			if r.ID != record.ID || len(r.Mutations) != 4 {
				t.Fatal("unexpected pending transaction")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if remaining != 1 {
			t.Fatalf("unacked records=%d", remaining)
		}
		t.Logf("phase=1 policy=%s four DAO writes committed; one unacked transaction preserved for next OS process", policy)
		return
	}
	transfers := phase - 1
	for i, id := range ids {
		h.assertState(t, id, want(i, transfers), true)
	}
	if phase == 2 {
		if h.runtime.Projector.Stats().Projected != 1 {
			t.Fatalf("startup did not replay exactly one transaction: %+v", h.runtime.Projector.Stats())
		}
		if err := h.request("trade_transfer", ids...); err != nil {
			t.Fatal(err)
		}
		if err := h.runtime.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		for i, id := range ids {
			h.assertState(t, id, want(i, 2), true)
		}
	}
	if remaining := h.records(t); len(remaining) != 0 {
		t.Fatalf("pending after recovery=%d", len(remaining))
	}
	h.close(t, nil)
	t.Logf("phase=%d policy=%s replay/load correct, no duplicated transfer, WAL pending=0", phase, policy)
}

// 在投影进入 Mongo 前只抬高排序最后一份文档的版本；前三份写入必须随 Mongo 事务回滚。
func testTradeConflict(t *testing.T, ctx context.Context, client fmongo.IMongo, root string, policy nest.DurabilityPolicy, policyIndex int) {
	h := newTradeFixture(t, ctx, client, root, "conflict", policy, false)
	ids := h.seed(t, 20000+policyIndex*100, 2)
	pause := h.gate.pause()
	defer pause.resume()
	if err := h.request("trade_transfer", ids...); err != nil {
		t.Fatal(err)
	}
	record := waitTradeRecord(t, ctx, pause, ids)
	collection := client.Database(h.database).Collection("trade_wallets")
	result, err := collection.UpdateOne(ctx, bson.M{"_id": ids[1], "_version": 1}, bson.M{"$set": bson.M{"_version": 999}})
	if err != nil || result.MatchedCount != 1 {
		t.Fatalf("inject late conflict: result=%+v err=%v", result, err)
	}
	before := map[string]bson.M{}
	for _, id := range ids {
		for _, resource := range []string{"trade_inventories", "trade_wallets"} {
			var doc bson.M
			if err := client.Database(h.database).Collection(resource).FindOne(ctx, bson.M{"_id": id}, &doc); err != nil {
				t.Fatal(err)
			}
			before[fmt.Sprintf("%s/%d", resource, id)] = doc
		}
	}
	pause.resume()
	select {
	case err := <-h.fatal:
		if !errors.Is(err, engine.ErrProjectionConflict) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for _, id := range ids {
		for _, resource := range []string{"trade_inventories", "trade_wallets"} {
			var doc bson.M
			if err := client.Database(h.database).Collection(resource).FindOne(ctx, bson.M{"_id": id}, &doc); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(doc, before[fmt.Sprintf("%s/%d", resource, id)]) {
				t.Fatalf("partial transaction escaped: %s/%d=%v", resource, id, doc)
			}
		}
	}
	markers, err := client.Database(h.database).Collection(engine.TransactionCollection).CountDocuments(ctx, bson.M{"_id": record.ID.String()})
	if err != nil || markers != 0 {
		t.Fatalf("aborted transaction marker count=%d err=%v", markers, err)
	}
	if err := h.request("trade_transfer", ids...); !errors.Is(err, engine.ErrProjectionConflict) {
		t.Fatalf("new transaction not fenced: %v", err)
	}
	if records := h.records(t); len(records) != 1 || records[0].ID != record.ID {
		t.Fatalf("fatal projection must preserve original WAL only")
	}
	h.close(t, engine.ErrProjectionConflict)
	t.Logf("policy=%s last document conflict rolled back all four Mongo writes; fatal fence retained WAL", policy)
}

func testTradeBackpressure(t *testing.T, ctx context.Context, client fmongo.IMongo, root string, policy nest.DurabilityPolicy, policyIndex int) {
	h := newTradeFixture(t, ctx, client, root, "backpressure", policy, false, func(o *engine.ProjectorOptions) { o.MaxUnackedRecords = 2; o.WarnUnackedRecords = 1 })
	ids := h.seed(t, 6000+policyIndex*100, 2)
	pause := h.gate.pause()
	defer pause.resume()
	if err := h.request("trade_transfer", ids...); err != nil {
		t.Fatal(err)
	}
	waitTradeRecord(t, ctx, pause, ids)
	if err := h.request("trade_transfer", ids...); err != nil {
		t.Fatal(err)
	}
	if err := h.request("trade_transfer", ids...); !errors.Is(err, engine.ErrProjectionBackpressure) {
		t.Fatalf("rejected request=%v", err)
	}
	if stats := h.runtime.Projector.Stats(); stats.WALUnacked != 2 || stats.AdmissionRejected != 1 || !stats.BacklogWarning || stats.FatalProjectionConflicts != 0 {
		t.Fatalf("full stats=%+v", stats)
	}
	pause.resume()
	if err := h.runtime.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	h.assertState(t, ids[0], tradeState{Coins: 9994, Items: 1002, Transfers: 2, Version: 3}, true)
	h.assertState(t, ids[1], tradeState{Coins: 10006, Items: 998, Transfers: 2, Version: 3}, true)
	if err := h.request("trade_transfer", ids...); err != nil {
		t.Fatalf("capacity not recovered: %v", err)
	}
	if err := h.runtime.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	h.assertState(t, ids[0], tradeState{Coins: 9991, Items: 1003, Transfers: 3, Version: 4}, true)
	h.assertState(t, ids[1], tradeState{Coins: 10009, Items: 997, Transfers: 3, Version: 4}, true)
	if stats := h.runtime.Projector.Stats(); stats.WALUnacked != 0 || stats.BacklogWarning {
		t.Fatalf("drained stats=%+v", stats)
	}
	h.close(t, nil)
}
