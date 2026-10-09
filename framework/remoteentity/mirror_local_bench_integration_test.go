//go:build integration

package remoteentity

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
	gojs "github.com/nats-io/nats.go/jetstream"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	natsdriver "github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus"
	syncdriver "github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus/driver"
)

// Mirror 第 6 步本机替代的性能对照（docs/feature/MIRROR-STEP-6-LOCAL-2026-10-06.md §4）：只读方的读延迟
// （L1 命中 / L2 回源 / 权威回源）与推送端到端延迟、订阅扇出。由 scripts/mirror-local.sh bench 在私有依赖进程上
// 运行（ROOST_DATAENGINE_IT=1 与 _MONGO_URI / _NATS_URL / _REDIS_ADDR），同一份文件在基线（v1.20.2）上原样再跑
// 一次，benchstat 对比——所以只用两个版本都有的入口：Assemble / Assembly.Start / Manager.ReadRemoteSnapshot /
// afterRemoteCommit（owner 提交后的发布：L1 + L2 CAS + 按兴趣推送）。只读方在两个版本里都是装配的 Manager：
// v1.20.2 经 BindSync 普通订阅（DeliverAll durable），v1.21.0 经 SnapshotClient 可确认订阅（DeliverNew durable）。
//
//	go test -tags integration -run '^$' -bench '^BenchmarkMirrorLocal' -benchtime 1x -count 6 ./framework/remoteentity
//
// 每个基准自己控制工作量（-benchtime 1x：b.N=1 时内部循环固定次数，ns/op 换成每次操作的平均值并另报 p50 / p99）。

const (
	mirrorLocalBenchKind  entity.EntityKind = 242 // 本包测试未用（见 remote_delete_ack_promises_test.go 的清单）
	mirrorLocalReadOps                      = 2000
	mirrorLocalPushRounds                   = 50
	mirrorLocalPushKeys                     = 10
)

type mirrorLocalBenchEnv struct {
	ctx      context.Context
	redis    fredis.IRedis
	mongo    fmongo.IMongo
	natsURL  string
	database string
	prefix   string
}

func newMirrorLocalBenchEnv(b *testing.B) *mirrorLocalBenchEnv {
	b.Helper()
	if os.Getenv("ROOST_DATAENGINE_IT") != "1" || os.Getenv("ROOST_MIRROR_LOCAL") != "1" {
		b.Skip("run scripts/mirror-local.sh bench (private dependency processes)")
	}
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: mirrorLocalBenchKind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	redis, err := redisdriver.NewClient(fredis.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_REDIS_ADDR")))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = redis.Close() })
	// 直连地址必须是主：故障场景切过主之后它可能是副本，L2 写会全部失败（B2 按未确认降级），结果不可比。
	if _, err := redis.Eval(context.Background(), "return redis.call('SET', KEYS[1], '1', 'PX', 1000)", []string{"m6bench:write-probe"}); err != nil {
		b.Fatalf("ROOST_DATAENGINE_IT_REDIS_ADDR does not accept writes (%v); run the benchmark on a fresh environment", err)
	}
	mongo, err := mongodriver.NewClient(fmongo.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")), mongodriver.IndexMigrationPolicy{})
	if err != nil {
		b.Fatal(err)
	}
	stamp := fmt.Sprintf("%d_%d", os.Getpid(), time.Now().UnixNano())
	env := &mirrorLocalBenchEnv{ctx: context.Background(), redis: redis, mongo: mongo,
		natsURL: strings.Split(os.Getenv("ROOST_DATAENGINE_IT_NATS_URL"), ",")[0], database: "m6bench_" + stamp, prefix: "m6bench." + stamp}
	b.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = mongo.Database(env.database).Drop(ctx)
		mongo.Close(context.Background())
	})
	return env
}

// countingStorage 数权威回源（只读方的 Backend 存储；其余方法原样是 MongoCommitter 的）。
type countingStorage struct {
	*MongoCommitter
	loads atomic.Int64
}

func (s *countingStorage) LoadRemoteSnapshot(ctx context.Context, key entity.RemoteSnapshotKey, consistency entity.RemoteReadConsistency, minVersion uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
	s.loads.Add(1)
	return s.MongoCommitter.LoadRemoteSnapshot(ctx, key, consistency, minVersion)
}

// assemble 装一个 Manager（owner 与只读方同一装配）；bus 非 nil 时 Start。
func (env *mirrorLocalBenchEnv) assemble(b *testing.B, sid int32, staleness time.Duration, bus fsyncbus.ISyncBus) (*Assembly, *countingStorage) {
	b.Helper()
	storage := &countingStorage{MongoCommitter: NewMongoCommitter(env.mongo, env.database, sid, 0)}
	backend, err := NewBackend(newMockLoader(), storage)
	if err != nil {
		b.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.SnapshotL2KeyPrefix = env.prefix
	cfg.CachedMaxStaleness = staleness
	cfg.SnapshotCacheTTL = 10 * time.Minute
	assembly, err := Assemble(AssemblyDeps{Redis: env.redis, Backend: backend}, cfg, sid, MongoBackendConfig{})
	if err != nil {
		b.Fatal(err)
	}
	if bus != nil {
		if err := assembly.Start(env.ctx, bus); err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = assembly.Stop(ctx)
		})
	}
	return assembly, storage
}

func mirrorLocalKey(b *testing.B, unique int64) entity.RemoteSnapshotKey {
	b.Helper()
	id, err := entity.BuildEntityID(unique, mirrorLocalBenchKind)
	if err != nil {
		b.Fatal(err)
	}
	return entity.RemoteSnapshotKey{EntityID: id, Kind: mirrorLocalBenchKind, Scope: 1}
}

// mirrorLocalPayload 是 256 字节的快照（公会摘要量级）。
var mirrorLocalPayload = []byte(strings.Repeat("guild-summary-256", 16))

func mirrorLocalCommit(key entity.RemoteSnapshotKey, version uint64) (entity.RemoteCommit, entity.RemoteCommitReceipt) {
	var tx entity.RemoteTransactionID
	copy(tx[:], fmt.Sprintf("%08d%08d", key.EntityID%100000000, version))
	commit := entity.RemoteCommit{TransactionID: tx, EntityID: key.EntityID, NextVersion: version, MarkerEpoch: 1, RouteEpoch: 1,
		Snapshots: []entity.RemoteSnapshotRecord{{Key: key, StateVersion: version, BaseVersion: version - 1, MarkerEpoch: 1, RouteEpoch: 1,
			Schema: 1, Codec: 1, Full: true, Data: mirrorLocalPayload, Checksum: entity.RemoteSnapshotChecksum(mirrorLocalPayload)}}}
	receipt := entity.RemoteCommitReceipt{TransactionID: tx, EntityID: key.EntityID, StateVersion: version, MarkerEpoch: 1, RouteEpoch: 1}
	return commit, receipt
}

// reportLatencies 报单次操作的平均延迟（ns/<unit>）与 p50 / p99；ns/op 是整个内部循环（b.N=1，外层 -count 给样本）。
func reportLatencies(b *testing.B, unit string, samples []time.Duration) {
	b.Helper()
	if len(samples) == 0 {
		return
	}
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	var total time.Duration
	for _, s := range sorted {
		total += s
	}
	b.ReportMetric(float64(total.Nanoseconds())/float64(len(sorted)), "ns/"+unit)
	b.ReportMetric(float64(sorted[len(sorted)/2].Nanoseconds()), "p50-ns")
	b.ReportMetric(float64(sorted[len(sorted)*99/100].Nanoseconds()), "p99-ns")
}

// BenchmarkMirrorLocalRead：只读方 Cached 读的三条来源。
//   - L1Hit：已确认、陈旧上限内（不碰 Redis）；
//   - L2Fetch：本机 L1 没有、共享 L2 有（owner 发布过）：一次 L2 读 + 准入；
//   - Authority：L1 / L2 都没有、Mongo 有：Monotonic 读（Cached 未命中不回源），一次权威 FindOne + L2 回填 + 准入。
func BenchmarkMirrorLocalRead(b *testing.B) {
	for _, source := range []string{"L1Hit", "L2Fetch", "Authority"} {
		b.Run(source, func(b *testing.B) {
			env := newMirrorLocalBenchEnv(b)
			owner, _ := env.assemble(b, 2601, time.Minute, nil)
			reader, storage := env.assemble(b, 2602, time.Minute, nil)
			keys := make([]entity.RemoteSnapshotKey, mirrorLocalReadOps)
			for i := range keys {
				keys[i] = mirrorLocalKey(b, int64(960000+i))
			}
			switch source {
			case "L1Hit", "L2Fetch":
				for _, key := range keys {
					commit, receipt := mirrorLocalCommit(key, 1)
					if err := owner.Manager.afterRemoteCommit(env.ctx, commit, receipt); err != nil {
						b.Fatal(err)
					}
				}
			case "Authority":
				var models []fmongo.WriteModel
				for _, key := range keys {
					id := remoteSnapshotStorageKey(key)
					models = append(models, fmongo.NewReplaceOneModel(bson.M{"_id": id}, bson.M{"_id": id, "key": key, "state_version": uint64(1), "base_version": uint64(0),
						"marker_epoch": uint64(1), "route_epoch": uint64(1), "schema": uint32(1), "codec": uint16(1),
						"checksum": entity.RemoteSnapshotChecksum(mirrorLocalPayload), "full": true, "data": mirrorLocalPayload}, true))
				}
				if _, err := env.mongo.Database(env.database).Collection(remoteSnapshotCollection).BulkWrite(env.ctx, models); err != nil {
					b.Fatal(err)
				}
			}
			if source == "L1Hit" {
				for _, key := range keys {
					if _, found, err := reader.Manager.ReadRemoteSnapshot(env.ctx, key, entity.RemoteReadCached, 0); err != nil || !found {
						b.Fatalf("warm read found=%v err=%v", found, err)
					}
				}
			}
			// Cached 未命中不回源（只读缓存），权威回源用 Monotonic（未命中按 key 合并回源一次）。
			consistency := entity.RemoteReadCached
			if source == "Authority" {
				consistency = entity.RemoteReadMonotonic
			}
			loadsBefore := storage.loads.Load()
			samples := make([]time.Duration, 0, len(keys))
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				samples = samples[:0]
				for _, key := range keys {
					started := time.Now()
					got, found, err := reader.Manager.ReadRemoteSnapshot(env.ctx, key, consistency, 0)
					samples = append(samples, time.Since(started))
					if err != nil || !found || got.StateVersion != 1 {
						b.Fatalf("read %v: version=%d found=%v err=%v", key.EntityID, got.StateVersion, found, err)
					}
				}
			}
			b.StopTimer()
			reportLatencies(b, "read", samples)
			b.ReportMetric(float64(storage.loads.Load()-loadsBefore)/float64(len(keys)*b.N), "authority-loads/read")
		})
	}
}

// BenchmarkMirrorLocalPush：owner 发布 → JetStream → N 个只读方各自的 durable → 准入（含 L2 CAS）。每轮给
// mirrorLocalPushKeys 个 key 各发一个新版本，计时到 N 个只读方全部读到这一轮的全部 key（Cached 读，陈旧上限 10 分钟：
// 读到新版本只能来自推送）。ns/round = 一轮扇出完成的平均时间；p50 / p99 同口径。
func BenchmarkMirrorLocalPush(b *testing.B) {
	for _, fanout := range []int{1, 10, 100} {
		b.Run("fanout="+strconv.Itoa(fanout), func(b *testing.B) {
			env := newMirrorLocalBenchEnv(b)
			client, err := natsdriver.NewClient(fnats.DefaultConfig(env.natsURL), natsdriver.ClientOptions{})
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(client.Close)
			js, err := natsdriver.NewJetStreamClient(client)
			if err != nil {
				b.Fatal(err)
			}
			bus := func(sid int32) fsyncbus.ISyncBus {
				bus, err := syncdriver.NewJetStreamSyncBus(env.ctx, js, syncdriver.JetStreamSyncConfig{LocalSid: sid, Prefix: env.prefix, Storage: fnats.JetStreamStorageMemory, StreamMaxAge: 10 * time.Minute, AckWait: 2 * time.Second})
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					_ = bus.StopWithContext(ctx)
				})
				return bus
			}
			b.Cleanup(func() {
				nc, err := gonats.Connect(env.natsURL, gonats.Timeout(2*time.Second))
				if err != nil {
					return
				}
				defer nc.Close()
				if stream, err := gojs.New(nc); err == nil {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					_ = stream.DeleteStream(ctx, syncdriver.JetStreamSyncStream(env.prefix))
				}
			})
			owner, _ := env.assemble(b, 2700, 10*time.Minute, bus(2700))
			readers := make([]*Assembly, fanout)
			for i := range readers {
				readers[i], _ = env.assemble(b, int32(2701+i), 10*time.Minute, bus(int32(2701+i)))
			}
			keys := make([]entity.RemoteSnapshotKey, mirrorLocalPushKeys)
			for i := range keys {
				keys[i] = mirrorLocalKey(b, int64(970000+i))
			}
			version := uint64(0)
			publishRound := func() {
				version++
				for _, key := range keys {
					commit, receipt := mirrorLocalCommit(key, version)
					if err := owner.Manager.afterRemoteCommit(env.ctx, commit, receipt); err != nil {
						b.Fatal(err)
					}
				}
			}
			// allSee 轮询：每个只读方的每个 key 都读到 version（读同时续租兴趣）。
			allSee := func(deadline time.Time) bool {
				for _, reader := range readers {
					for _, key := range keys {
						for {
							got, found, err := reader.Manager.ReadRemoteSnapshot(env.ctx, key, entity.RemoteReadCached, 0)
							if err == nil && found && got.StateVersion >= version {
								break
							}
							if time.Now().After(deadline) {
								return false
							}
							time.Sleep(50 * time.Microsecond)
						}
					}
				}
				return true
			}
			// 预热：第一轮经 L2 读到并续租兴趣；之后直到一轮在 2s 内全部经推送到达（owner 已收到全部兴趣）。
			publishRound()
			allSee(time.Now().Add(10 * time.Second))
			warm := false
			for attempt := 0; attempt < 40 && !warm; attempt++ {
				publishRound()
				warm = allSee(time.Now().Add(2 * time.Second))
			}
			if !warm {
				b.Fatal("pushes never reached every reader")
			}
			samples := make([]time.Duration, 0, mirrorLocalPushRounds)
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				samples = samples[:0]
				for round := 0; round < mirrorLocalPushRounds; round++ {
					started := time.Now()
					publishRound()
					if !allSee(started.Add(30 * time.Second)) {
						b.Fatalf("round %d: not every reader saw version %d within 30s", round, version)
					}
					samples = append(samples, time.Since(started))
				}
			}
			b.StopTimer()
			reportLatencies(b, "round", samples)
		})
	}
}
