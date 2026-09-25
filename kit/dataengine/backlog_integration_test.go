//go:build integration

package dataengine

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// 独立运行的大积压验收：保持同一实体的 WAL 顺序，跨 segment 重开，
// 慢存储取消不推进 ack，下一次启动完整投影到真实 Mongo。
func TestRealDataEngineLargeBacklogRecovery(t *testing.T) {
	if os.Getenv("ROOST_DATAENGINE_IT_BACKLOG") != "1" {
		t.Skip("set ROOST_DATAENGINE_IT_BACKLOG=1 for the 100k WAL recovery workload")
	}
	fx := newRealFixture(t)
	defer fx.close()
	fx.cancel()
	fx.ctx, fx.cancel = context.WithTimeout(context.Background(), 3*time.Minute)
	const recordCount, entityCount, window = 100_000, 10_000, 256
	records, _ := realMixedRatioRecords(t, fx.database, "backlog_entities", 100, recordCount, entityCount, 0)
	collections := []string{"backlog_entities"}
	if os.Getenv("ROOST_DATAENGINE_IT_BACKLOG_MULTI") == "1" {
		collections = append(collections, "backlog_inventory")
		for i := range records {
			mutation := records[i].Mutations[0]
			mutation.Key.Resource = "backlog_inventory"
			records[i].Mutations = append(records[i].Mutations, mutation)
		}
	}

	options := nestwal.DefaultOptions(t.TempDir())
	options.WriterVersion = nestwal.WriterVersionV2
	options.SegmentBytes = 1 << 20 // 强制跨段恢复，而不是只检查单一文件。
	options.MaxRecordBytes = 64 << 10
	wal, err := nestwal.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wal.Close(context.Background()) })
	seedStarted := time.Now()
	for start := 0; start < len(records); start += window {
		var tickets []corenest.CommitTicket
		for i := start; i < min(start+window, len(records)); i++ {
			records[i].Durability = corenest.DurabilityPipelined
			ticket, err := wal.Enqueue(fx.context(), records[i])
			if err != nil {
				t.Fatal(err)
			}
			tickets = append(tickets, ticket)
		}
		for _, ticket := range tickets {
			select {
			case <-ticket.Done():
				if err := ticket.Err(); err != nil {
					t.Fatal(err)
				}
			case <-fx.context().Done():
				t.Fatal(fx.context().Err())
			}
		}
	}
	seedElapsed := time.Since(seedStarted)
	segments := wal.Stats().SegmentFiles
	if segments < 2 {
		t.Fatalf("workload did not rotate WAL: segments=%d", segments)
	}
	if err := wal.Close(fx.context()); err != nil {
		t.Fatal(err)
	}
	records = nil

	wal, err = nestwal.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	blocked := &blockingProjectionOnlyMongoStore{delegate: fx.runtime.Store, attempted: make(chan struct{}, 1), release: make(chan struct{})}
	projector, err := engine.NewProjector(wal, blocked, engine.ProjectorOptions{CloseWAL: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = projector.Close(context.Background()) })
	select {
	case <-blocked.attempted:
	case <-fx.context().Done():
		t.Fatal("slow projection never started")
	}
	deadline, cancel := context.WithTimeout(fx.context(), 20*time.Millisecond)
	err = projector.Flush(deadline)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slow store flush=%v", err)
	}
	if err := projector.Close(fx.context()); err != nil {
		t.Fatal(err)
	}
	for _, collection := range collections {
		assertCollectionCount(t, fx, collection, 0)
	}

	wal, err = nestwal.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	assertWALReplayCount(t, wal, recordCount)
	acks := 0
	// 先停止后台循环，再计时手动 Flush，ack 数不与后台 goroutine 竞争。
	projector, err = engine.NewProjector(wal, fx.runtime.Store, engine.ProjectorOptions{CloseWAL: false, IdlePoll: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := projector.Close(fx.context()); err != nil {
		t.Fatal(err)
	}
	before := projector.Stats().Projected
	projector.OverrideAck(func(ctx context.Context, fence corenest.CommitFence) error {
		acks++
		return wal.Ack(ctx, fence)
	})
	started := time.Now()
	if err := projector.Flush(fx.context()); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	if stats := projector.Stats(); stats.Projected != recordCount {
		t.Fatalf("projection stats=%+v", stats)
	}
	assertWALReplayCount(t, wal, 0)
	for _, collection := range collections {
		assertCollectionCount(t, fx, collection, entityCount)
		count, err := fx.mongo.Database(fx.database).Collection(collection).CountDocuments(fx.context(), bson.M{"_version": recordCount / entityCount})
		if err != nil || count != entityCount {
			t.Fatalf("%s final versions: count=%d err=%v", collection, count, err)
		}
		for _, id := range []int64{1, entityCount / 2, entityCount} {
			doc := findDocument(t, fx, collection, id)
			if doc["value"] != int64(recordCount-entityCount)+id-1 {
				t.Fatalf("%s entity %d final payload=%v", collection, id, doc["value"])
			}
		}
	}
	if err := wal.Close(fx.context()); err != nil {
		t.Fatal(err)
	}
	wal, err = nestwal.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	assertWALReplayCount(t, wal, 0)
	t.Logf("records=%d entities=%d daos_per_entity=%d segments=%d seed=%s recovery=%s timed_records=%d throughput=%.0f records/s checkpoint_acks=%d; slow-store cancellation preserved all records; final restart pending=0",
		recordCount, entityCount, len(collections), segments, seedElapsed, elapsed, recordCount-before, float64(recordCount-before)/elapsed.Seconds(), acks)
}
