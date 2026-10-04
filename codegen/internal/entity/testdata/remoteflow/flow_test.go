package remoteflow

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/mongo/driver"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	natsdriver "github.com/tjbdwanghaibo/roost-core/nats/driver"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/remoteentity"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/sync/syncbus"
	syncdriver "github.com/tjbdwanghaibo/roost-core/sync/syncbus/driver"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// 业务 loader 是 Remote 唯一需要应用提供的能力；测试实体由正式生成器创建。
type vaultLoader struct{ values map[int64]*Vault }

func (l vaultLoader) LoadRemoteEntity(_ context.Context, id int64, kind entity.EntityKind) (entity.IThreadSafeRemoteEntity, error) {
	if kind != EntityKindVault || l.values[id] == nil {
		return nil, errors.New("missing vault")
	}
	return l.values[id], nil
}
func (l vaultLoader) LookupLocalRemoteEntity(id int64, kind entity.EntityKind) entity.IThreadSafeRemoteEntity {
	if kind != EntityKindVault {
		return nil
	}
	return l.values[id]
}

type noEffects struct{}

func (noEffects) Publish(context.Context, engine.OutboxItem) error {
	return errors.New("unexpected effect")
}

type scopedRedis struct {
	fredis.IRedis
	prefix string
	keys   sync.Map
	// evalFault 非空时 Eval 不发往 Redis、直接返回该错误（B27 第 2 批 RR-20260926-46：提交后释放锁失败）。
	evalFault atomic.Pointer[error]
}

func (r *scopedRedis) key(key string) string {
	key = r.prefix + key
	r.keys.Store(key, struct{}{})
	return key
}
func (r *scopedRedis) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	if fault := r.evalFault.Load(); fault != nil {
		return nil, *fault
	}
	scoped := make([]string, len(keys))
	for i, key := range keys {
		scoped[i] = r.key(key)
	}
	return r.IRedis.Eval(ctx, script, scoped, args...)
}
func (r *scopedRedis) HGet(ctx context.Context, key, field string) ([]byte, error) {
	return r.IRedis.HGet(ctx, r.key(key), field)
}
func (r *scopedRedis) Del(ctx context.Context, keys ...string) (int64, error) {
	scoped := make([]string, len(keys))
	for i, key := range keys {
		scoped[i] = r.key(key)
	}
	return r.IRedis.Del(ctx, scoped...)
}

type observedBus struct {
	fsyncbus.ISyncBus
	interests chan struct{}
}

func (b observedBus) Subscribe(topic string, h fsyncbus.Handler) (func(), error) {
	return b.ISyncBus.Subscribe(topic, func(msg *fsyncbus.SyncMsg) error {
		err := h(msg)
		if err == nil && topic == remoteentity.SyncTopicInterest {
			select {
			case b.interests <- struct{}{}:
			default:
			}
		}
		return err
	})
}

func TestGeneratedRemoteNestFlow(t *testing.T) {
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Skip("run scripts/test-remote-generated.sh")
	}
	database := NewBalanceDao().DbName()
	if !strings.HasPrefix(database, "roost_remote_generated_") || database == "roost_remote_generated_placeholder" {
		t.Fatal("isolated database required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), remoteTestTimeout())
	defer cancel()
	mongo, err := mongodriver.NewClient(fmongo.DefaultConfig(uri), mongodriver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer mongo.Close(context.Background())
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := mongo.Database(database).Drop(cleanup); err != nil {
			t.Errorf("drop fixture: %v", err)
		}
	}()
	redis, err := redisdriver.NewClient(fredis.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_REDIS_ADDR")))
	if err != nil {
		t.Fatal(err)
	}
	defer redis.Close()
	RegisterEntity()
	for policyIndex, policy := range []nest.DurabilityPolicy{nest.DurabilityAsync, nest.DurabilityStrict, nest.DurabilityPipelined} {
		if selected := os.Getenv("ROOST_REMOTE_POLICY"); selected != "" && selected != policy.String() {
			continue
		}
		t.Run(policy.String(), func(t *testing.T) {
			scoped := &scopedRedis{IRedis: redis, prefix: database + ":" + policy.String() + ":"}
			t.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				scoped.keys.Range(func(key, value any) bool {
					if _, err := redis.Del(cleanup, key.(string)); err != nil {
						t.Errorf("Redis cleanup: %v", err)
					}
					return true
				})
			})
			access := entity.NewManagerAccess(entity.NewEntityManager())
			loader := vaultLoader{values: make(map[int64]*Vault)}
			ids := make([]int64, remoteEntityCount())
			for i := range ids {
				id, err := entity.BuildEntityID(int64(7500+policyIndex*len(ids)+i), EntityKindVault)
				if err != nil {
					t.Fatal(err)
				}
				ids[i] = id
				created, err := access.Create(&entity.EntityCreateParam{IsCreate: true, Kind: EntityKindVault, Id: id})
				if err != nil {
					t.Fatal(err)
				}
				loader.values[id] = created.(*Vault)
			}
			remoteStore := remoteentity.NewMongoCommitter(mongo, database, 1000, 0)
			backend, err := remoteentity.NewBackend(loader, remoteStore)
			if err != nil {
				t.Fatal(err)
			}
			cfg := remoteentity.DefaultConfig()
			if os.Getenv("ROOST_REMOTE_WRITE_LIMIT") != "" {
				cfg.MaxConcurrentWrites = remoteInt("ROOST_REMOTE_WRITE_LIMIT", 128)
			}
			if remoteLoadEnabled() {
				cfg.SnapshotCacheTTL = 5 * time.Minute
				if value := os.Getenv("ROOST_REMOTE_LOCK_TTL"); value != "" {
					ttl, err := time.ParseDuration(value)
					if err != nil || ttl <= 0 {
						t.Fatal("positive ROOST_REMOTE_LOCK_TTL required")
					}
					cfg.LockTTL = ttl
				}
			} else {
				// 短业务/故障用例缩短租期；容量与长稳默认沿用正式配置的租期。
				cfg.LockTTL = 3 * time.Second
			}
			assembly, err := remoteentity.Assemble(remoteentity.AssemblyDeps{Redis: scoped, Backend: backend}, cfg, 1000, remoteentity.MongoBackendConfig{})
			if err != nil {
				t.Fatal(err)
			}
			senderNats, err := natsdriver.NewClient(fnats.DefaultConfig(remoteNatsURL()), natsdriver.ClientOptions{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(senderNats.Close)
			receiverNats, err := natsdriver.NewClient(fnats.DefaultConfig(remoteNatsURL()), natsdriver.ClientOptions{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(receiverNats.Close)
			prefix := database + "." + policy.String()
			observed := observedBus{ISyncBus: syncdriver.NewNatsSyncBus(senderNats, 1000, prefix), interests: make(chan struct{}, len(ids)*2)}
			if err := assembly.Start(ctx, observed); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := assembly.Stop(context.Background()); err != nil {
					t.Errorf("Remote stop: %v", err)
				}
			})
			// 健康/容量场景不配置回源，证明 NATS 交付；断网场景验证正式单调读取与权威回填。
			receiver := remoteentity.NewManager(remoteentity.NewVersionedLockFactory(scoped), cfg, 2000)
			refill := &observedSnapshotBackend{IRemoteEntityBackend: backend}
			readConsistency := entity.RemoteReadCached
			if remoteFaultRefill() {
				receiver.SetBackend(refill)
				readConsistency = entity.RemoteReadMonotonic
			}
			snapshots, interests := receiver.BindSync(syncdriver.NewNatsSyncBus(receiverNats, 2000, prefix))
			if err := snapshots.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(snapshots.Stop)
			if err := interests.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(interests.Stop)
			keys := make([]entity.RemoteSnapshotKey, 0, len(ids)*2)
			prepareRemoteOwnership(t, ctx, assembly.Manager, ids)
			for _, id := range ids {
				for _, collection := range []string{"remote_balances", "remote_items"} {
					key := entity.RemoteSnapshotKey{EntityID: id, Kind: EntityKindVault, Scope: entity.RemoteSnapshotScope(collection)}
					keys = append(keys, key)
					if err := receiver.RenewRemoteSnapshotInterest(ctx, key); err != nil {
						t.Fatal(err)
					}
				}
			}
			for range keys {
				select {
				case <-observed.interests:
				case <-ctx.Done():
					t.Fatal("NATS interest not delivered", ctx.Err())
				}
			}
			store, err := engine.NewMongoStore(mongo, engine.MongoStoreConfig{DefaultDatabase: database})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.EnsureInfrastructure(ctx); err != nil {
				t.Fatal(err)
			}
			if err := store.SetRemoteProjection(backend, assembly.Manager); err != nil {
				t.Fatal(err)
			}
			if !store.SupportsRemoteParallelProjection() {
				t.Fatal("production Backend wiring lost parallel projection capability")
			}
			opts := nestwal.DefaultOptions(t.TempDir())
			opts.WriterVersion = nestwal.WriterVersionV2
			wal, err := nestwal.Open(opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = wal.Close(context.Background()) })
			projectorOptions := engine.ProjectorOptions{CloseWAL: true}
			if os.Getenv("ROOST_REMOTE_WAL_LIMIT") != "" {
				projectorOptions.MaxUnackedRecords = uint64(remoteInt("ROOST_REMOTE_WAL_LIMIT", 512))
			}
			if os.Getenv("ROOST_REMOTE_PROJECTION_WORKERS") != "" {
				projectorOptions.RemoteProjectionWorkers = remoteInt("ROOST_REMOTE_PROJECTION_WORKERS", 8)
			}
			projector, err := engine.NewProjector(wal, store, projectorOptions)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = projector.Close(context.Background()) })
			outboxStore, err := engine.NewMongoOutboxStore(store)
			if err != nil {
				t.Fatal(err)
			}
			outbox, err := engine.NewOutboxWorker(outboxStore, noEffects{}, engine.OutboxWorkerOptions{Owner: "remote-generated"})
			if err != nil {
				t.Fatal(err)
			}
			update, reject, panicName := nest.NewHandlerName("remote_update"), nest.NewHandlerName("remote_reject"), nest.NewHandlerName("remote_panic")
			warmup := nest.NewHandlerName("remote_warmup")
			runtime, err := engine.NewRuntime(store, wal, projector, outbox, access, assembly.Manager, nil, engine.PipelinedRuntimeConfig{Allowlist: []string{update.String(), reject.String(), panicName.String()}, Async: true, AsyncWorkers: 2, AsyncQueueCap: 32})
			if err != nil {
				t.Fatal(err)
			}
			if err := runtime.Start(ctx); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := runtime.Shutdown(context.Background()); err != nil {
					t.Errorf("runtime shutdown: %v", err)
				}
			})
			options := append(runtime.NestOptions(), nest.NestOptionWithGetter(access), nest.NestOptionWithRemoteEntityManager(assembly.Manager), nest.NestOptionWithWorkerNumAndMsgCap(remoteNestWorkers(), 1, 4096))
			options = append(options, nest.NestOptionWithWorkerPools(
				nest.WorkerPoolConfig{Workers: remoteNestWorkers(), QueueCap: remoteInt("ROOST_REMOTE_FAST_QUEUE", 4096)},
				nest.WorkerPoolConfig{Workers: remoteInt("ROOST_REMOTE_IO_WORKERS", remoteNestWorkers()), QueueCap: remoteInt("ROOST_REMOTE_SLOW_QUEUE", 64)},
			))
			if os.Getenv("ROOST_REMOTE_STAGE_METRICS") == "1" {
				options = append(options, nest.NestOptionWithStageMetrics(true))
			}
			if os.Getenv("ROOST_REMOTE_FAULT") != "" {
				options = append(options, nest.NestOptionWithSyncTimeout(60*time.Second))
			}
			scheduler := nest.NewEngine(options...)
			businessErr := errors.New("business rejection")
			for _, name := range []nest.HandlerName{update, reject, panicName, warmup} {
				durability := policy
				if name == warmup {
					// 预热统一等待提交完成，避免 async 提前回复把冷初始化变成无界投递。
					durability = nest.DurabilityStrict
				}
				scheduler.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
					for _, e := range es {
						vault := e.(*Vault)
						vault.balance.SetValue(vault.balance.GetValue() + 1)
						vault.items.SetValue(vault.items.GetValue() + 1)
					}
					if name == reject {
						return nil, businessErr
					}
					if name == panicName {
						panic("remote fixture panic")
					}
					return nil, nil
				}, nest.HandlerMeta{Rollback: nest.RollbackState, Durability: durability})
			}
			if err := scheduler.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = scheduler.Shutdown(context.Background()) })
			// 每个实体的期望是区间 [Min, Max]；短业务流与无错误的负载里 Min == Max，与点期望逐字相同。
			expected := make(map[int64]remoteExpect, len(ids))
			var load remoteLoadResult
			if remoteLoadEnabled() {
				load = runRemoteLoad(t, ctx, scheduler, update, warmup, ids, keys, receiver, assembly.Manager, projector, wal)
				expected = load.Expected
			} else {
				for step := 1; step <= 3; step++ {
					if step == 2 {
						finishFault := startRemoteBusinessFault(t)
						defer finishFault()
					}
					if _, err := scheduler.RequestMulti(ctx, update, ids, nil); err != nil {
						t.Fatal(err)
					}
				}
				for _, id := range ids {
					expected[id] = remoteExpect{Min: 3, Max: 3}
				}
			}
			if _, err := scheduler.RequestMulti(ctx, reject, ids[:2], nil); !errors.Is(err, businessErr) {
				t.Fatalf("reject=%v", err)
			}
			if _, err := scheduler.RequestMulti(ctx, panicName, ids[:2], nil); err == nil {
				t.Fatal("panic accepted")
			}
			if err := runtime.Flush(ctx); err != nil {
				t.Fatal(err)
			}
			// settled 是排空后实体的实际计数：必须落在期望区间内，且 Mongo 投影 / 版本、NATS 快照都与它逐一相等。
			// 区间是点时 settled 就是原来的精确期望值。
			settled := make(map[int64]int64, len(ids))
			var settledAboveSuccess int64
			for _, id := range ids {
				vault := loader.values[id]
				vault.GetMutex().Lock()
				balance, items := vault.balance.GetValue(), vault.items.GetValue()
				vault.GetMutex().Unlock()
				if balance != items || !expected[id].contains(balance) {
					t.Fatalf("rollback lost id=%d %d/%d%s", id, balance, items, expected[id].suffix())
				}
				settled[id] = balance
				settledAboveSuccess += balance - expected[id].Min
				for _, collection := range []string{"remote_balances", "remote_items"} {
					var doc struct {
						Version uint64 `bson:"_ver"`
						Data    []byte `bson:"data"`
					}
					if err := mongo.Database(database).Collection(collection).FindOne(ctx, bson.M{"_id": id}, &doc); err != nil {
						t.Fatal(err)
					}
					var data struct {
						Value int64 `bson:"value"`
					}
					if err := bson.Unmarshal(doc.Data, &data); err != nil {
						t.Fatal(err)
					}
					if doc.Version != uint64(settled[id]) || data.Value != settled[id] {
						t.Fatalf("Mongo %s/%d version=%d value=%d%s", collection, id, doc.Version, data.Value, expected[id].suffix())
					}
				}
			}
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for _, key := range keys {
				for {
					snapshot, found, err := receiver.ReadRemoteSnapshot(ctx, key, readConsistency, uint64(settled[key.EntityID]))
					if err != nil {
						t.Fatal(err)
					}
					if found && snapshot.StateVersion == uint64(settled[key.EntityID]) {
						var payload struct {
							Value int64 `bson:"value"`
						}
						if err := bson.Unmarshal(snapshot.Payload.BytesCopy(), &payload); err != nil {
							t.Fatal(err)
						}
						if payload.Value != settled[key.EntityID] {
							t.Fatalf("snapshot value=%d", payload.Value)
						}
						break
					}
					select {
					case <-ticker.C:
					case <-ctx.Done():
						t.Fatal("NATS snapshot not delivered", ctx.Err())
					}
				}
			}
			if pending, err := remoteStore.PendingRemoteCommits(ctx, 10); err != nil || len(pending) != 0 {
				t.Fatalf("pending=%v err=%v", pending, err)
			}
			t.Logf("%s: %d entities x 2 DAOs, rejection/panic rollback, all Mongo versions/values and %d snapshots verified; authoritative_refill=%t loads=%d", policy, len(ids), len(keys), remoteFaultRefill(), refill.loads.Load())
			if remoteLoadEnabled() {
				// 先写 .verified 再报告负载错误：数据一致与负载有错误分开判定，错误不再遮住核验结果。
				markRemoteVerified(t, load, settledAboveSuccess)
				if load.Errors != 0 {
					t.Errorf("load errors=%d not_applied=%d uncertain=%d first=%s; final consistency verified within [success, success+uncertain] for %d entities (%d widened, settled_above_success=%d)",
						load.Errors, load.NotApplied, load.Uncertain, load.FirstError, load.Entities, load.Widened, settledAboveSuccess)
				}
			}
		})
	}
}

// 所有权初始化不计入业务压测；限制并发，避免 10000 个实体串行等待 majority journal。
func prepareRemoteOwnership(t *testing.T, ctx context.Context, manager *remoteentity.Manager, ids []int64) {
	t.Helper()
	var workers sync.WaitGroup
	slots := make(chan struct{}, 16)
	failures := make(chan error, len(ids))
	for _, id := range ids {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			workers.Wait()
			t.Fatal(ctx.Err())
		}
		workers.Go(func() {
			defer func() { <-slots }()
			if _, err := manager.ClaimRemoteOwnership(ctx, id); err != nil {
				failures <- err
				return
			}
			if _, err := manager.EnterRemoteSharedMode(ctx, id); err != nil {
				failures <- err
			}
		})
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Errorf("prepare ownership: %v", err)
	}
	if t.Failed() {
		t.FailNow()
	}
}
