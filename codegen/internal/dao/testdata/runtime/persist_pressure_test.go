//go:build daoruntime

package testdata

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// 本用例测生成 DAO 的批量积压追平，不把它当成 Nest 请求或实时端到端延迟。
// 先通过正式事务冻结变化并写文件 WAL，再让正式 Projector 排空，逐文档核对结果。
func TestPersistPathsPressure(t *testing.T) {
	if os.Getenv("ROOST_PERSIST_PRESSURE") != "1" {
		t.Skip("set ROOST_PERSIST_PRESSURE=1 and isolated Mongo URI")
	}
	for _, shape := range []string{"sparse", "nested", "dense"} {
		t.Run(shape, func(t *testing.T) { runPersistPressure(t, shape) })
	}
}

func runPersistPressure(t *testing.T, shape string) {
	const entities = 128
	const size = 1000
	const records = 10000
	env := newStoredKeysEnv(t)
	daos := make([]*HeroDao, entities)
	for i := range daos {
		d := collectionHero(size)
		d.SetId(int64(i + 1))
		if _, err := env.write(t, d, true); err != nil {
			t.Fatal(err)
		}
		daos[i] = d
	}
	wal, err := nestwal.Open(nestwal.DefaultOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wal.Close(context.Background()) })
	latencies := make([]time.Duration, records)
	bytes := 0
	started := time.Now()
	for i := range records {
		d := daos[i%entities]
		seq := int64(i + 2000)
		call := time.Now()
		record := capturePersist(t, d, func(d *HeroDao) {
			switch shape {
			case "sparse":
				d.SetItems(5, int32(seq))
				d.SetFriendsAt(50, seq)
			case "nested":
				e, _ := d.GetSquad(50)
				e.SetStar(int32(seq))
				item, _ := d.GetEquips(2)
				gem, _ := item.GetGems(3)
				gem.SetLevel(int32(seq))
			case "dense":
				for j := range size {
					d.SetItems(int64(j), int32(seq))
					d.SetFriendsAt(j, seq)
				}
			}
		})
		record.Durability = dataengine.DurabilityAsync
		for j := range record.Mutations {
			m := &record.Mutations[j]
			m.Key.Database = env.database
			m.Key.Scope = dataengine.DatabaseGlobal
			bytes += len(m.Patch.SetBSON)
		}
		if err := dataengine.ValidateCommitRecord(record); err != nil {
			t.Fatal(err)
		}
		if _, err := wal.Append(env.ctx, record); err != nil {
			t.Fatal(err)
		}
		d.DirtyTracker().AcknowledgeSyncChanges(d.DirtyTracker().SyncChangeVersion())
		latencies[i] = time.Since(call)
	}
	if err := wal.Sync(env.ctx); err != nil {
		t.Fatal(err)
	}
	prepare := time.Since(started)
	projector, err := engine.NewProjector(wal, env.store, engine.ProjectorOptions{ManualReplay: true, ProjectionWorkers: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = projector.Close(context.Background()) })
	drainStart := time.Now()
	if err := projector.Flush(env.ctx); err != nil {
		t.Fatal(err)
	}
	drain := time.Since(drainStart)
	stats := projector.Stats()
	if stats.Projected != records || stats.ProjectionFailures != 0 || stats.FatalProjectionConflicts != 0 || stats.WALUnacked != 0 {
		t.Fatalf("projection: %+v", stats)
	}
	remaining := 0
	if err := wal.Replay(env.ctx, func(nest.CommitFence, dataengine.CommitRecord) error { remaining++; return nil }); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("remaining WAL=%d", remaining)
	}
	for i, d := range daos {
		doc := env.stored(t, d.Id())
		wantVersion := int64(1 + (records-1-i)/entities + 1)
		if doc["_version"] != wantVersion {
			t.Fatalf("id=%d version=%v want=%d", d.Id(), doc["_version"], wantVersion)
		}
		delete(doc, "_version")
		delete(doc, "_last_tx")
		raw, err := bson.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if !samePersistBSON(t, d.Marshal(), raw) {
			t.Fatalf("id=%d Mongo differs from DAO", d.Id())
		}
	}
	slices.Sort(latencies)
	result := map[string]any{"shape": shape, "entities": entities, "collection_elements": size, "records": records, "projection_workers": 8, "prepare_seconds": prepare.Seconds(), "drain_seconds": drain.Seconds(), "drain_records_per_second": float64(records) / drain.Seconds(), "prepare_append_p99_ms": float64(latencies[(records-1)*99/100]) / float64(time.Millisecond), "prepare_append_max_ms": float64(latencies[records-1]) / float64(time.Millisecond), "bson_set_bytes_per_record": bytes / records, "verified_documents": len(daos), "wal_remaining": remaining, "projection_failures": stats.ProjectionFailures}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("PERSIST_PRESSURE %s", raw)
}
