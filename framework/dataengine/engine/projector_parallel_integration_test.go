//go:build integration

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// 真实副本集上的“后面的记录先投影完、前面的失败、重启后重放”：并行通道里成功的后缀靠 Mongo 事务标记幂等，
// 文档已是更高版本也不误报冲突；失败的记录在重启后补上，effect 恰好一条。
func TestRealMongoParallelProjectionReplaysSuccessfulSuffixAfterRestart(t *testing.T) {
	ctx, client, database := isolatedRealMongo(t)
	mongoStore, err := NewMongoStore(client, MongoStoreConfig{Databases: identityDatabases(t), DefaultDatabase: database})
	if err != nil {
		t.Fatal(err)
	}
	if err := mongoStore.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	heroes := client.Database(database).Collection("heroes")
	for _, id := range []int64{7, 8} {
		if _, err := heroes.InsertOne(ctx, bson.M{"_id": id, "_version": uint64(1), "level": int32(1)}); err != nil {
			t.Fatal(err)
		}
	}
	patch := func(sequence byte, id int64, from uint64) coredata.CommitRecord {
		record := batchRecord(t, sequence, id, from, from+1)
		record.Mutations[0].Key.Database = database
		return record
	}
	failed, first, special, last := patch(1, 7, 1), patch(2, 8, 1), patch(3, 8, 2), patch(4, 8, 3)
	special.Effects = []coredata.Effect{{ID: "effect-real-8", Topic: "hero.changed", Payload: []byte{8}}}
	records := []coredata.CommitRecord{failed, first, special, last}
	store := &failingOnceStore{MongoStore: mongoStore, failID: failed.ID, waitID: last.ID, waitApplied: make(chan struct{})}
	store.armed.Store(true)

	dir := t.TempDir()
	wal, err := nestwal.Open(nestwal.DefaultOptions(dir))
	if err != nil {
		t.Fatal(err)
	}
	for i := range records {
		if _, err := wal.Append(ctx, records[i]); err != nil {
			t.Fatal(err)
		}
	}
	projector, err := NewProjector(wal, store, ProjectorOptions{CloseWAL: true, ManualReplay: true})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := projector.ReplayPass(ctx); n != 0 || !errors.Is(err, errInjectedProjection) || !store.sawWait.Load() {
		t.Fatalf("first pass n=%d err=%v later-first=%v", n, err, store.sawWait.Load())
	}
	if err := projector.Close(ctx); err != nil {
		t.Fatal(err)
	}
	wal, err = nestwal.Open(nestwal.DefaultOptions(dir))
	if err != nil {
		t.Fatal(err)
	}
	assertWALReplayIDs(t, wal, []coredata.TransactionID{failed.ID, first.ID, special.ID, last.ID})
	store.armed.Store(false)
	restarted, err := NewProjector(wal, store, ProjectorOptions{CloseWAL: true, ManualReplay: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close(context.Background()) })
	if err := restarted.Flush(ctx); err != nil || restarted.fatal() != nil {
		t.Fatalf("replay after restart err=%v fatal=%v", err, restarted.fatal())
	}
	assertWALReplayCount(t, wal, 0)
	var seven, eight struct {
		Version uint64 `bson:"_version"`
		LastTx  string `bson:"_last_tx"`
	}
	if err := heroes.FindOne(ctx, bson.M{"_id": int64(7)}, &seven); err != nil {
		t.Fatal(err)
	}
	if err := heroes.FindOne(ctx, bson.M{"_id": int64(8)}, &eight); err != nil {
		t.Fatal(err)
	}
	if seven.Version != 2 || eight.Version != 4 || eight.LastTx != last.ID.String() {
		t.Fatalf("documents after replay: 7=%+v 8=%+v", seven, eight)
	}
	if n, err := client.Database(database).Collection(OutboxCollection).CountDocuments(ctx, bson.M{}); err != nil || n != 1 {
		t.Fatalf("outbox entries=%d err=%v", n, err)
	}
}

// TestRealMongoProjectorParallelThroughput 测投影器在真实 Mongo 上排空一段预写 WAL 的速率（条/s），即积压追平速率。
// 只在 ROOST_PROJECTOR_PERF=1 时运行（scripts/perf/projector-parallel.sh），连接串只从 ROOST_DATAENGINE_IT_MONGO_URI 读，不打印。
//
//	ROOST_PROJECTOR_PERF_SHAPE   plain（单 mutation，实体均匀）| hot（一半记录落在同一实体）| receipt（mutation + 回执）| special（mutation + 回执 + effect）
//	ROOST_PROJECTOR_PERF_WORKERS 逗号分隔的 ProjectionWorkers 档位，缺省 1,2,4,8,16
//	ROOST_PROJECTOR_PERF_RECORDS / _ENTITIES 记录数（缺省 20000）/ 实体数（缺省 4096）；_OUT 结果 JSON 行追加到的文件
//
// 每档用一次性库（<ROOST_PROJECTOR_PERF_DB_PREFIX，须以 roost_sagaperf_projpar_ 开头>_<形状>_w<档位>），结束删库；排空后逐实体核对 _version、effect 条数与 WAL 已清空。
func TestRealMongoProjectorParallelThroughput(t *testing.T) {
	if os.Getenv("ROOST_PROJECTOR_PERF") != "1" {
		t.Skip("set ROOST_PROJECTOR_PERF=1 (scripts/perf/projector-parallel.sh)")
	}
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Skip("ROOST_DATAENGINE_IT_MONGO_URI is not set")
	}
	shape := envOr("ROOST_PROJECTOR_PERF_SHAPE", "plain")
	records := envInt(t, "ROOST_PROJECTOR_PERF_RECORDS", 20000)
	entities := envInt(t, "ROOST_PROJECTOR_PERF_ENTITIES", 4096)
	client, err := driver.NewClient(fmongo.DefaultConfig(uri), driver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal("connect failed") // 不带驱动错误：其中可能有主机名
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	for _, field := range strings.Split(envOr("ROOST_PROJECTOR_PERF_WORKERS", "1,2,4,8,16"), ",") {
		workers, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || workers < 1 {
			t.Fatalf("invalid workers %q", field)
		}
		result := runProjectorThroughput(t, client, shape, records, entities, workers)
		line, _ := json.Marshal(result)
		t.Logf("%s", line)
		if out := os.Getenv("ROOST_PROJECTOR_PERF_OUT"); out != "" {
			file, err := os.OpenFile(out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = file.Write(append(line, '\n'))
			_ = file.Close()
		}
	}
}

type projectorThroughput struct {
	Shape           string  `json:"shape"`
	Workers         int     `json:"workers"`
	Records         int     `json:"records"`
	Entities        int     `json:"entities"`
	Seconds         float64 `json:"seconds"`
	RecordsPerSec   float64 `json:"records_per_sec"`
	Projected       uint64  `json:"projected"`
	ParallelWindows uint64  `json:"parallel_windows"`
	AvgLanes        float64 `json:"avg_lanes"`
	Verified        bool    `json:"verified"`
	Started         string  `json:"started"`
}

func runProjectorThroughput(t *testing.T, client fmongo.IMongo, shape string, total, entities, workers int) projectorThroughput {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	prefix := envOr("ROOST_PROJECTOR_PERF_DB_PREFIX", fmt.Sprintf("roost_sagaperf_projpar_%d", os.Getpid()))
	if !strings.HasPrefix(prefix, "roost_sagaperf_projpar_") {
		t.Fatal("ROOST_PROJECTOR_PERF_DB_PREFIX must start with roost_sagaperf_projpar_")
	}
	database := fmt.Sprintf("%s_%s_w%d", prefix, shape, workers)
	defer func() { _ = client.Database(database).Drop(context.Background()) }()
	store, err := NewMongoStore(client, MongoStoreConfig{Databases: identityDatabases(t), DefaultDatabase: database})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	wal, err := nestwal.Open(nestwal.DefaultOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	random := rand.New(rand.NewPCG(uint64(total), uint64(entities)))
	versions := make(map[int64]uint64, entities)
	effects := 0
	for sequence := range total {
		entity := int64(random.IntN(entities)) + 1
		if shape == "hot" && sequence%2 == 0 {
			entity = 1
		}
		var id coredata.TransactionID
		id[0] = byte(workers)
		id[8], id[9], id[10], id[11] = byte(sequence>>24), byte(sequence>>16), byte(sequence>>8), byte(sequence)
		id[15] = 1
		mutation := coredata.Mutation{
			Key:             coredata.DocumentKey{Database: database, Resource: "perf_accounts", ID: entity},
			ExpectedVersion: versions[entity], NextVersion: versions[entity] + 1, Mask: 1, Schema: 1, Codec: "bson-v2",
		}
		if versions[entity] == 0 {
			mutation.Kind = coredata.MutationPut
			mutation.Data, _ = bson.Marshal(bson.M{"_id": entity, "bal": int64(sequence)})
		} else {
			mutation.Kind = coredata.MutationPatch
			mutation.Patch.SetBSON, _ = bson.Marshal(bson.D{{Key: "bal", Value: int64(sequence)}})
		}
		versions[entity]++
		record := coredata.CommitRecord{ID: id, Durability: corenest.DurabilityAsync.Record(), Mutations: []coredata.Mutation{mutation}}
		switch shape {
		case "plain", "hot":
		case "receipt", "special":
			record.Receipts = []coredata.Receipt{{Namespace: "projpar-perf", ID: fmt.Sprintf("op-%d", sequence), Digest: []byte{1}, Payload: []byte{2}}}
			if shape == "special" {
				record.Effects = []coredata.Effect{{ID: fmt.Sprintf("%s-effect-%d", database, sequence), Topic: "perf.changed", Payload: []byte{3}}}
				effects++
			}
		default:
			t.Fatalf("unknown shape %q", shape)
		}
		if _, err := wal.Append(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	if err := wal.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	projector, err := NewProjector(wal, store, ProjectorOptions{CloseWAL: true, ManualReplay: true, ProjectionWorkers: workers})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = projector.Close(context.Background()) }()
	started := time.Now()
	if err := projector.Flush(ctx); err != nil {
		t.Fatalf("workers=%d flush: %v", workers, err)
	}
	elapsed := time.Since(started)
	stats := projector.Stats()
	result := projectorThroughput{
		Shape: shape, Workers: workers, Records: total, Entities: entities, Seconds: elapsed.Seconds(),
		RecordsPerSec: float64(total) / elapsed.Seconds(), Projected: stats.Projected, ParallelWindows: stats.ParallelWindows,
		Started: started.UTC().Format(time.RFC3339),
	}
	if stats.ParallelWindows > 0 {
		result.AvgLanes = float64(stats.ParallelLanes) / float64(stats.ParallelWindows)
	}
	// 核对：每个实体的最终版本等于写给它的记录数；effect 恰好每条一次；WAL 已全部确认。
	var docs []struct {
		ID      int64  `bson:"_id"`
		Version uint64 `bson:"_version"`
	}
	if err := client.Database(database).Collection("perf_accounts").Find(ctx, bson.M{}, &docs, fmongo.FindOption{BatchSize: 4096}); err != nil {
		t.Fatal(err)
	}
	verified := len(docs) == len(versions) && stats.FatalProjectionConflicts == 0
	for _, doc := range docs {
		verified = verified && versions[doc.ID] == doc.Version
	}
	if outbox, err := client.Database(database).Collection(OutboxCollection).CountDocuments(ctx, bson.M{}); err != nil || int(outbox) != effects {
		verified = false
	}
	remaining := 0
	_ = wal.Replay(ctx, func(corenest.CommitFence, coredata.CommitRecord) error { remaining++; return nil })
	verified = verified && remaining == 0
	result.Verified = verified
	if !verified {
		t.Errorf("workers=%d verification failed: docs=%d entities=%d remaining=%d stats=%+v", workers, len(docs), len(versions), remaining, stats)
	}
	return result
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envInt(t *testing.T, name string, fallback int) int {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		t.Fatalf("invalid %s", name)
	}
	return parsed
}
