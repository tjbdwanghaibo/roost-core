//go:build integration

package saga

// Store Service 的 saga 投影在真实依赖上的用例（权威 Redis：scripts/it-redis-aof.sh 起的实例或私有 redis-server；Mongo：
// 隔离环境副本集，冷副本的 $replaceWith + $cond 单调写要真实服务端执行）。运行：source ~/.roost-it/roost-dataengine-it/env.sh 后
//   GOWORK=off ROOST_DATAENGINE_IT=1 go test -tags integration -count=1 -run 'Projection|RedisEvicts|RedisLateSuccessAfterEviction' ./framework/saga

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/tjbdwanghaibo/roost-core/framework/storeservice"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
	redisdriver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/redistest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// projectionEnv 是一套权威 Redis + 真实 Mongo 冷副本 + 协调器存储与 saga 投影。
type projectionEnv struct {
	*redisTestEnv
	store      *RedisStore
	projection *RedisProjection
}

func newProjectionEnv(t *testing.T, options RedisStoreOptions) projectionEnv {
	t.Helper()
	client, database := realMongoU0280(t)
	addr := redistest.AOFAddr(t)
	env := newRedisTestEnvAt(t, addr, redistest.Prefix(t, addr, "sagaproj"), client)
	env.db = database
	store, err := env.build(nil, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureInfrastructure(context.Background()); err != nil {
		t.Fatal(err)
	}
	projection, err := NewRedisProjection(env.client, store.copy, store.opts)
	if err != nil {
		t.Fatal(err)
	}
	return projectionEnv{redisTestEnv: env, store: store, projection: projection}
}

// startStoreService 起一个进程内 Store Service（只投影 saga）。
func (env projectionEnv) startStoreService(t *testing.T) *storeservice.Runner {
	t.Helper()
	runner, err := storeservice.NewRunner(env.client, storeservice.Options{Owner: "test-" + NewID(), LeaseDuration: time.Second, Block: 20 * time.Millisecond, RetryMin: 5 * time.Millisecond, PausedRecheck: 50 * time.Millisecond}, env.projection)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = runner.Stop(ctx)
	})
	return runner
}

// drain 等全部分区的日志投影完（积压为 0）。
func (env projectionEnv) drain(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		backlog, err := env.store.ProjectionBacklog(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if backlog == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("projection backlog still %d", backlog)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (env projectionEnv) mongoVersion(t *testing.T, id string) uint64 {
	t.Helper()
	var doc recordDoc
	if err := env.store.copy.sagas().FindOne(context.Background(), bson.M{"_id": id}, &doc); err != nil {
		if errors.Is(err, fmongo.ErrNotFound) {
			return 0
		}
		t.Fatal(err)
	}
	return doc.Version
}

func projectionRecord(id string, now time.Time) Record {
	return Record{ID: id, Type: "rally", DefinitionVersion: 1, BusinessKey: "bk-" + id, Status: StatusPending, Phase: PhaseForward, Version: 1, NextRunAt: now, CreatedAt: now, UpdatedAt: now}
}

// TestProjectionReplayIsMonotonic：同一批条目再投一次、较旧的条目晚到，Mongo 冷副本的版本不倒退；tombstone 的 closure 只升不降；
// 回执与告警只写第一次。
func TestProjectionReplayIsMonotonic(t *testing.T) {
	ctx := context.Background()
	env := newProjectionEnv(t, RedisStoreOptions{Partitions: 1})
	now := time.Now().UTC().Truncate(time.Millisecond)
	r := projectionRecord("mono", now)
	if err := env.store.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	w := r.Clone()
	w.Version, w.Status, w.Attempt = 2, StatusWaiting, 1
	w.OperationKey = operationKey(w.ID, PhaseForward, 0)
	w.CommandID = commandID(w.OperationKey, 0, 1)
	if _, err := env.store.Apply(ctx, ApplyRequest{ExpectedVersion: 1, After: w}); err != nil {
		t.Fatal(err)
	}
	failed := w.Clone()
	failed.Version, failed.Status, failed.OperationKey, failed.CommandID, failed.NextRunAt = 3, StatusFailed, "", "", time.Time{}
	if _, err := env.store.Apply(ctx, ApplyRequest{ExpectedVersion: 2, After: failed, CloseOperation: w.OperationKey}); err != nil {
		t.Fatal(err)
	}
	late := Completion{CommandID: w.CommandID, IdempotencyKey: w.OperationKey, SagaID: w.ID, Success: true, CompletedAt: now}
	if first, err := env.store.MarkLateSuccessAlarm(ctx, late, 0); err != nil || !first {
		t.Fatalf("alarm = %v, %v", first, err)
	}
	reopened := failed.Clone()
	reopened.Version, reopened.Status, reopened.LateStep = 4, StatusManualRequired, 1
	if _, err := env.store.Apply(ctx, ApplyRequest{ExpectedVersion: 3, After: reopened, Receipt: &late, CloseOperation: w.OperationKey}); err != nil {
		t.Fatal(err)
	}
	entries, err := env.client.(*redisdriver.Client).XRangeAfter(ctx, env.store.keys.log(0), "0-0", 100)
	if err != nil || len(entries) != 5 {
		t.Fatalf("log = %d entries, %v; want create, dispatch, abandon, alarm, late success", len(entries), err)
	}
	all := make([]storeservice.Entry, len(entries))
	for i, entry := range entries {
		all[i] = storeservice.Entry{ID: entry.ID, Fields: entry.Fields}
	}
	if err := env.projection.Apply(ctx, 0, all); err != nil {
		t.Fatal(err)
	}
	// 重放：整批再投一次，再把较旧的条目（版本 2、放弃关闭）单独晚投一次。
	if err := env.projection.Apply(ctx, 0, all); err != nil {
		t.Fatal(err)
	}
	if err := env.projection.Apply(ctx, 0, all[1:3]); err != nil {
		t.Fatal(err)
	}
	if v := env.mongoVersion(t, "mono"); v != 4 {
		t.Fatalf("mongo version after replays = %d, want 4", v)
	}
	var tombstone operationDoc
	if err := env.store.copy.operations().FindOne(ctx, bson.M{"_id": w.OperationKey}, &tombstone); err != nil {
		t.Fatal(err)
	}
	if tombstone.Closure != operationClosureResult || tombstone.SagaID != "mono" || len(tombstone.LateAlarms) != 1 {
		t.Fatalf("tombstone after replays = %+v; want closure result, one alarm", tombstone)
	}
	history, err := env.store.copy.CompletionHistory(ctx, late)
	if err != nil || !history.Receipt {
		t.Fatalf("receipt in the copy = %+v, %v", history, err)
	}
	cold, err := env.store.copy.Get(ctx, "mono")
	if err != nil || cold.Status != StatusManualRequired || cold.LateStep != 1 || cold.Lease.Owner != "" {
		t.Fatalf("cold record = %+v, %v", cold, err)
	}
}

// TestProjectionPausesOnPermanentError：冷副本里另一个 saga 已占用同一 (type, business_key)（唯一索引冲突），投影暂停该分区、
// 不跳过、健康失败；运维修好冲突并删除暂停键后继续。
func TestProjectionPausesOnPermanentError(t *testing.T) {
	ctx := context.Background()
	env := newProjectionEnv(t, RedisStoreOptions{Partitions: 1})
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := env.store.Create(ctx, projectionRecord("conflict", now)); err != nil {
		t.Fatal(err)
	}
	if err := env.store.Create(ctx, projectionRecord("after", now)); err != nil {
		t.Fatal(err)
	}
	squatter := toRecordDoc(projectionRecord("squatter", now))
	squatter.BusinessKey = "bk-conflict"
	if _, err := env.store.copy.sagas().InsertOne(ctx, squatter); err != nil {
		t.Fatal(err)
	}
	runner := env.startStoreService(t)
	deadline := time.Now().Add(10 * time.Second)
	for len(runner.Paused()["saga"]) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the partition did not pause on a unique business key conflict")
		}
		time.Sleep(20 * time.Millisecond)
	}
	reason, err := env.raw.Get(ctx, env.store.keys.prefix+":paused:0").Result()
	if err != nil || !strings.Contains(reason, "uniq_type_business") {
		t.Fatalf("pause key = %q, %v", reason, err)
	}
	time.Sleep(200 * time.Millisecond)
	if backlog, _ := env.store.ProjectionBacklog(ctx); backlog != 2 {
		t.Fatalf("a paused partition moved on: backlog %d, want both entries kept", backlog)
	}
	if v := env.mongoVersion(t, "after"); v != 0 {
		t.Fatalf("an entry after the failing one was projected (version %d); nothing may be skipped", v)
	}
	if _, err := env.store.copy.sagas().DeleteOne(ctx, bson.M{"_id": "squatter"}); err != nil {
		t.Fatal(err)
	}
	if err := env.raw.Del(ctx, env.store.keys.prefix+":paused:0").Err(); err != nil {
		t.Fatal(err)
	}
	env.drain(t)
	if env.mongoVersion(t, "conflict") != 1 || env.mongoVersion(t, "after") != 1 || len(runner.Paused()) != 0 {
		t.Fatalf("after repairing: versions %d / %d, paused %v", env.mongoVersion(t, "conflict"), env.mongoVersion(t, "after"), runner.Paused())
	}
}

// TestRedisEvictsOnlyAfterProjection：终态记录过了保留期，但最新一版还没投影进 Mongo（ls 晚于进度）时不驱逐；投影之后驱逐，
// 业务键守卫改为 business_key_guard 后过期，之后的读从冷副本来。
func TestRedisEvictsOnlyAfterProjection(t *testing.T) {
	ctx := context.Background()
	env := newProjectionEnv(t, RedisStoreOptions{Partitions: 1, TerminalRetention: time.Millisecond})
	now := time.Now().UTC().Truncate(time.Millisecond)
	r := projectionRecord("evict", now)
	if err := env.store.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	done := r.Clone()
	done.Version, done.Status, done.CompletedSteps, done.NextRunAt = 2, StatusCompleted, 2, time.Time{}
	if _, err := env.store.Apply(ctx, ApplyRequest{ExpectedVersion: 1, After: done}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := env.projection.AfterAdvance(ctx, 0, "0-0"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := env.store.resident(ctx, "evict"); err != nil || !found {
		t.Fatalf("a terminal record whose last version is not projected was evicted (found=%v err=%v)", found, err)
	}
	env.startStoreService(t)
	env.drain(t)
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, found, err := env.store.resident(ctx, "evict")
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the projected terminal record was never evicted")
		}
		time.Sleep(20 * time.Millisecond)
	}
	ttl, err := env.raw.PTTL(ctx, env.store.keys.businessKey(r.Type, r.BusinessKey)).Result()
	if err != nil || ttl <= 0 || ttl > env.store.opts.BusinessKeyGuard {
		t.Fatalf("business key guard ttl = %v, %v; want it to expire after business_key_guard", ttl, err)
	}
	cold, err := env.store.Get(ctx, "evict")
	if err != nil || cold.Version != 2 || cold.Status != StatusCompleted {
		t.Fatalf("read after eviction = %+v, %v; want the projected version from the copy", cold, err)
	}
	if byKey, err := env.store.GetByBusinessKey(ctx, r.Type, r.BusinessKey); err != nil || byKey.ID != "evict" {
		t.Fatalf("GetByBusinessKey after eviction = %+v, %v", byKey, err)
	}
}

// TestRedisLateSuccessAfterEvictionReloadsAndCompensates：第 0 步超时用尽、saga 补偿完被驱逐出 Redis 之后，那一步迟到的成功
// 送达：协调器从冷副本读到记录与放弃关闭的 tombstone，写时先装回，再把 saga 带回补偿、只补偿那一步；同一份成功再送达
// 按回执去重，告警只记一次。
func TestRedisLateSuccessAfterEvictionReloadsAndCompensates(t *testing.T) {
	ctx := context.Background()
	env := newProjectionEnv(t, RedisStoreOptions{Partitions: 2, TerminalRetention: time.Millisecond})
	engine, err := NewEngine(env.store, PublishFunc(func(context.Context, Command) error { return nil }), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Register(testDefinition()); err != nil {
		t.Fatal(err)
	}
	started, err := engine.StartSaga(ctx, StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "late-evicted"})
	if err != nil {
		t.Fatal(err)
	}
	now := started.NextRunAt
	if err := engine.processClaimed(ctx, started, now); err != nil {
		t.Fatal(err)
	}
	waiting, err := env.store.Get(ctx, started.ID)
	if err != nil {
		t.Fatal(err)
	}
	late := Completion{CommandID: waiting.CommandID, IdempotencyKey: waiting.OperationKey, SagaID: waiting.ID, Success: true, Data: []byte("reserved")}
	// 两次尝试都超时：第 0 步放弃关闭，saga 没有完成的步骤可补，直接 Compensated。
	for range 2 {
		current, err := env.store.Get(ctx, started.ID)
		if err != nil {
			t.Fatal(err)
		}
		at := current.NextRunAt
		if current.Status == StatusWaiting {
			at = current.NextRunAt
		}
		if err := engine.processClaimed(ctx, current, at); err != nil {
			t.Fatal(err)
		}
		if next, err := env.store.Get(ctx, started.ID); err == nil && next.Status == StatusPending {
			if err := engine.processClaimed(ctx, next, next.NextRunAt); err != nil {
				t.Fatal(err)
			}
		}
	}
	ended, err := env.store.Get(ctx, started.ID)
	if err != nil || !ended.Status.Terminal() {
		t.Fatalf("saga after the step's budget = %+v, %v; want terminal", ended, err)
	}
	env.startStoreService(t)
	env.drain(t)
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, found, _ := env.store.resident(ctx, started.ID)
		if !found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the terminal saga was not evicted")
		}
		time.Sleep(20 * time.Millisecond)
	}
	before := engine.Stats()
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Go(func() { _, errs[i] = engine.Complete(ctx, late) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("delivery %d of the late success after eviction = %v", i, err)
		}
	}
	after, found, err := env.store.resident(ctx, started.ID)
	if err != nil || !found {
		t.Fatalf("the evicted saga was not reloaded: found=%v err=%v", found, err)
	}
	if after.Status != StatusCompensating || after.Step != 0 || after.LateStep != 1 || string(after.LateData) != "reserved" {
		t.Fatalf("after the late success: %+v; want back in compensation for step 0", after)
	}
	if got := engine.Stats().LateAfterAbandon - before.LateAfterAbandon; got != 1 {
		t.Fatalf("late success counted %d times across 4 deliveries, want 1", got)
	}
	if reloads := counterValue("saga.redis.reload_total"); reloads == 0 {
		t.Fatal("no reload was counted")
	}
}

// TestRedisLateAlarmReloadsAnEvictedRecord：记录已驱逐、只在冷副本里（带放弃关闭的 tombstone）时，迟到告警先把记录装回
// （连同 tombstone），再原子地记一次；同一（操作，代际）再记返回 false。脚本对“记录不在”返回需要装回，不当成“没有 tombstone”。
func TestRedisLateAlarmReloadsAnEvictedRecord(t *testing.T) {
	ctx := context.Background()
	env := newProjectionEnv(t, RedisStoreOptions{Partitions: 1})
	now := time.Now().UTC().Truncate(time.Millisecond)
	record := projectionRecord("cold", now)
	record.Version, record.Status, record.NextRunAt = 4, StatusFailed, time.Time{}
	if _, err := env.store.copy.sagas().InsertOne(ctx, toRecordDoc(record)); err != nil {
		t.Fatal(err)
	}
	operation := operationKey("cold", PhaseForward, 0)
	if _, err := env.store.copy.operations().InsertOne(ctx, operationDoc{ID: operation, SagaID: "cold", Closure: operationClosureAbandoned, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	late := Completion{CommandID: operation + ":1", IdempotencyKey: operation, SagaID: "cold", Success: true}
	if marked, err := env.store.MarkLateSuccessAlarm(ctx, late, 0); err != nil || !marked {
		t.Fatalf("alarm on an evicted record = %v, %v; want true after reloading it", marked, err)
	}
	if marked, err := env.store.MarkLateSuccessAlarm(ctx, late, 0); err != nil || marked {
		t.Fatalf("the same alarm again = %v, %v; want false", marked, err)
	}
	resident, found, err := env.store.resident(ctx, "cold")
	if err != nil || !found || resident.Version != 4 {
		t.Fatalf("reloaded record = %+v found=%v err=%v", resident, found, err)
	}
	other := Completion{CommandID: operationKey("ghost", PhaseForward, 0) + ":1", IdempotencyKey: operationKey("ghost", PhaseForward, 0), SagaID: "ghost", Success: true}
	if marked, err := env.store.MarkLateSuccessAlarm(ctx, other, 0); err != nil || marked {
		t.Fatalf("alarm for a saga in neither store = %v, %v; want false", marked, err)
	}
}

// TestRedisReloadAndEvictValidateBeforeWriting：装回遇到已驻留的记录返回 EXISTS、不覆盖（驻留的版本比冷副本新）；装回与驱逐
// 遇到类型不对的键返回 CORRUPT，前缀下的键一个不变。
func TestRedisReloadAndEvictValidateBeforeWriting(t *testing.T) {
	ctx := context.Background()
	env := newProjectionEnv(t, RedisStoreOptions{Partitions: 1})
	now := time.Now().UTC().Truncate(time.Millisecond)
	stale := projectionRecord("resident", now)
	stale.Version, stale.Status, stale.NextRunAt = 2, StatusFailed, time.Time{}
	if _, err := env.store.copy.sagas().InsertOne(ctx, toRecordDoc(stale)); err != nil {
		t.Fatal(err)
	}
	live := projectionRecord("resident", now)
	live.Version = 5
	if err := env.store.Create(ctx, live); err != nil && !errors.Is(err, ErrAlreadyExists) {
		t.Fatal(err)
	}
	// Create 先查冷副本会拒绝；直接写一条更新的驻留记录。
	doc, err := encodeRecordDoc(live)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.raw.HSet(ctx, env.store.keys.record("resident"), "v", 5, "st", int(StatusPending), "nr", now.UnixMilli(), "lo", "", "lt", 0, "lu", 0, "doc", doc).Err(); err != nil {
		t.Fatal(err)
	}
	before := env.snapshotKeys(t)
	if found, err := env.store.reload(ctx, "resident"); err != nil || !found {
		t.Fatalf("reload of a resident record = %v, %v; want found (EXISTS)", found, err)
	}
	if after := env.snapshotKeys(t); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatal("reload overwrote a resident record with the older copy")
	}
	if err := env.raw.Del(ctx, env.store.keys.due()).Err(); err != nil {
		t.Fatal(err)
	}
	if err := env.raw.RPush(ctx, env.store.keys.due(), "x").Err(); err != nil {
		t.Fatal(err)
	}
	if err := env.raw.Del(ctx, env.store.keys.record("resident")).Err(); err != nil {
		t.Fatal(err)
	}
	before = env.snapshotKeys(t)
	if _, err := env.store.reload(ctx, "resident"); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("reload with a wrong-type due key = %v, want CORRUPT (ErrInvalidRecord)", err)
	}
	if after := env.snapshotKeys(t); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatal("a refused reload wrote to redis")
	}
	if err := env.raw.RPush(ctx, env.store.keys.evict(0), "x").Err(); err != nil {
		t.Fatal(err)
	}
	before = env.snapshotKeys(t)
	if err := env.projection.AfterAdvance(ctx, 0, "0-0"); err == nil || !strings.Contains(err.Error(), "CORRUPT") {
		t.Fatalf("evict with a wrong-type evict key = %v, want CORRUPT", err)
	}
	if after := env.snapshotKeys(t); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatal("a refused evict wrote to redis")
	}
}

// BenchmarkSagaProjection 测 Store Service 的 saga 投影吞吐（v2 门槛：本机 P=16、批 256 时 >= 5000 条/s）：预先在 16 个
// 分区写好 b.N 条真实形状的日志条目（每条一份记录主体；每三条带一个回执、一个 tombstone），起一个运行器，计时到积压归零。
func BenchmarkSagaProjection(b *testing.B) {
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		b.Skip("ROOST_DATAENGINE_IT_MONGO_URI is not set")
	}
	b.StopTimer()
	ctx := context.Background()
	client, database := benchmarkMongo(b, uri)
	addr := redistest.AOFAddr(b)
	env := newRedisTestEnvAt(b, addr, redistest.Prefix(b, addr, "sagaprojbench"), client)
	env.db = database
	store, err := env.build(nil, RedisStoreOptions{Partitions: 16, MaxProjectionBacklog: 1 << 30})
	if err != nil {
		b.Fatal(err)
	}
	if err := store.EnsureInfrastructure(ctx); err != nil {
		b.Fatal(err)
	}
	projection, err := NewRedisProjection(env.client, store.copy, store.opts)
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	pipe := env.raw.Pipeline()
	for i := range b.N {
		id := fmt.Sprintf("bench-%09d", i)
		r := projectionRecord(id, now)
		r.Version = 1 + uint64(i%3)
		doc, err := encodeRecordDoc(r)
		if err != nil {
			b.Fatal(err)
		}
		values := map[string]any{"id": id, "v": strconv.FormatUint(r.Version, 10), "doc": doc}
		if i%3 == 2 {
			op := operationKey(id, PhaseForward, 0)
			values["c"], values["cd"], values["ct"] = op+":1", "00", strconv.FormatInt(now.UnixMilli(), 10)
			values["o"], values["oc"], values["ot"] = op, operationClosureResult, strconv.FormatInt(now.UnixMilli(), 10)
		}
		pipe.XAdd(ctx, &goredis.XAddArgs{Stream: store.keys.log(store.keys.partition(id)), Values: values})
		if i%1000 == 999 {
			if _, err := pipe.Exec(ctx); err != nil {
				b.Fatal(err)
			}
		}
	}
	if _, err := pipe.Exec(ctx); err != nil {
		b.Fatal(err)
	}
	runner, err := storeservice.NewRunner(env.client, storeservice.Options{BatchSize: 256, LeaseDuration: 10 * time.Second, Block: 20 * time.Millisecond}, projection)
	if err != nil {
		b.Fatal(err)
	}
	b.StartTimer()
	start := time.Now()
	if err := runner.Start(ctx); err != nil {
		b.Fatal(err)
	}
	for {
		backlog, err := store.ProjectionBacklog(ctx)
		if err != nil {
			b.Fatal(err)
		}
		if backlog == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	elapsed := time.Since(start)
	b.StopTimer()
	stop, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = runner.Stop(stop)
	b.ReportMetric(float64(b.N)/elapsed.Seconds(), "entries/s")
}

func benchmarkMongo(b *testing.B, uri string) (fmongo.IMongo, string) {
	b.Helper()
	client, err := driver.NewClient(fmongo.DefaultConfig(uri), driver.IndexMigrationPolicy{})
	if err != nil {
		b.Fatal(err)
	}
	database := fmt.Sprintf("roost_sagaprojbench_%d_%d", os.Getpid(), time.Now().UnixNano())
	b.Cleanup(func() {
		_ = client.Database(database).Drop(context.Background())
		_ = client.Close(context.Background())
	})
	return client, database
}
