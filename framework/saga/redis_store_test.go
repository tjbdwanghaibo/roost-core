package saga

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/redistest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// redisTestEnv 是一套权威 Redis（scripts/it-redis-aof.sh 起的实例，或每个用例一个私有 redis-server）加 Mongo 冷副本
// （mongotest）。同一个 env 上可以反复建 RedisStore，模拟进程重启后从持久状态恢复。
type redisTestEnv struct {
	addr   string
	prefix string
	mongo  fmongo.IMongo
	db     string
	client fredis.IRedis
	raw    *goredis.Client
}

func newRedisTestEnv(t testing.TB) *redisTestEnv {
	t.Helper()
	addr := redistest.AOFAddr(t)
	prefix := redistest.Prefix(t, addr, "sagatest")
	return newRedisTestEnvAt(t, addr, prefix, mongotest.NewClient())
}

func newRedisTestEnvAt(t testing.TB, addr, prefix string, mongo fmongo.IMongo) *redisTestEnv {
	t.Helper()
	cfg := fredis.DefaultConfig(addr)
	cfg.PoolSize = 32
	client, err := redisdriver.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	raw := goredis.NewClient(&goredis.Options{Addr: addr})
	t.Cleanup(func() { _ = client.Close(); _ = raw.Close() })
	return &redisTestEnv{addr: addr, prefix: prefix, mongo: mongo, db: "saga", client: client, raw: raw}
}

// open 建一个 RedisStore 并过启动关口。
func (env *redisTestEnv) open(t testing.TB) *RedisStore {
	t.Helper()
	store, err := env.build(nil, RedisStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureInfrastructure(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store
}

func (env *redisTestEnv) build(failure *app.RuntimeFailure, options RedisStoreOptions) (*RedisStore, error) {
	copy, err := NewMongoStore(env.mongo, MongoStoreOptions{Database: env.db})
	if err != nil {
		return nil, err
	}
	options.KeyPrefix = env.prefix
	return NewRedisStore(env.client, copy, failure, options)
}

// newTestRedisStore 是单元用例的协调器存储：权威 Redis + mongotest 冷副本。
func newTestRedisStore(t testing.TB) *RedisStore {
	t.Helper()
	return newRedisTestEnv(t).open(t)
}

// seedTombstone 直接在驻留记录上写一个 tombstone（模拟某个操作此前已被关闭），不经过 Apply。
func seedTombstone(t testing.TB, store *RedisStore, sagaID, operation, closure string, created time.Time) {
	t.Helper()
	client := store.redis.(*redisdriver.Client).Raw()
	if err := client.HSet(context.Background(), store.keys.record(sagaID), "o:"+operation, closure+"|"+fmt.Sprint(created.UnixMilli())).Err(); err != nil {
		t.Fatal(err)
	}
}

// snapshotKeys 读出前缀下全部键的类型与内容，用来证明一次调用没有任何写入。不用 DUMP：hashtable 编码的哈希按内部迭代
// 顺序序列化，读命令触发的渐进 rehash 会改变顺序，内容相同时 DUMP 也可能不同。
func (env *redisTestEnv) snapshotKeys(t testing.TB) map[string]string {
	t.Helper()
	ctx := context.Background()
	out := map[string]string{}
	iter := env.raw.Scan(ctx, 0, env.prefix+":*", 512).Iterator()
	for iter.Next(ctx) {
		key := iter.Val()
		kind, err := env.raw.Type(ctx, key).Result()
		if err != nil {
			t.Fatal(err)
		}
		var content any
		switch kind {
		case "hash":
			content, err = env.raw.HGetAll(ctx, key).Result()
		case "zset":
			content, err = env.raw.ZRangeWithScores(ctx, key, 0, -1).Result()
		case "stream":
			content, err = env.raw.XRange(ctx, key, "-", "+").Result()
		case "string":
			content, err = env.raw.Get(ctx, key).Result()
		case "list":
			content, err = env.raw.LRange(ctx, key, 0, -1).Result()
		}
		if err != nil {
			t.Fatal(err)
		}
		ttl, err := env.raw.PTTL(ctx, key).Result()
		if err != nil {
			t.Fatal(err)
		}
		out[key] = fmt.Sprintf("%s %v ttl=%v", kind, content, ttl > 0)
	}
	if err := iter.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestStoreContract(t *testing.T) {
	runStoreContract(t, func(t *testing.T) Store { return newTestRedisStore(t) })
}

// lostReplyRedis 让第 failOn 次 DurableScriptBatch 在服务端执行之后返回 EOF：脚本已执行，调用方看到“结果未知”。
type lostReplyRedis struct {
	*redisdriver.Client
	calls  atomic.Int64
	failOn int64
}

func (c *lostReplyRedis) DurableScriptBatch(ctx context.Context, calls []fredis.ScriptCall, numLocal, numReplicas int, timeout time.Duration) ([]fredis.ScriptResult, int64, int64, error) {
	results, local, replicas, err := c.Client.DurableScriptBatch(ctx, calls, numLocal, numReplicas, timeout)
	if c.calls.Add(1) == c.failOn && err == nil {
		return nil, 0, 0, io.ErrUnexpectedEOF
	}
	return results, local, replicas, err
}

func TestRedisApplyRetryAfterLostReplyIsApplied(t *testing.T) {
	ctx := context.Background()
	env := newRedisTestEnv(t)
	base := env.open(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	record := Record{ID: "lost", Type: "rally", DefinitionVersion: 1, BusinessKey: "lost", Status: StatusPending, Phase: PhaseForward, Version: 1, NextRunAt: now, CreatedAt: now, UpdatedAt: now}
	if err := base.Create(ctx, record); err != nil {
		t.Fatal(err)
	}
	wrapped := &lostReplyRedis{Client: env.client.(*redisdriver.Client), failOn: 1}
	store, err := NewRedisStore(wrapped, base.copy, nil, base.opts)
	if err != nil {
		t.Fatal(err)
	}
	after := record.Clone()
	after.Version = 2
	after.Status, after.Attempt = StatusWaiting, 1
	after.OperationKey = operationKey(after.ID, after.Phase, after.Step)
	after.CommandID = commandID(after.OperationKey, 0, 1)
	request := ApplyRequest{ExpectedVersion: 1, After: after}
	if _, err := store.Apply(ctx, request); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("first apply = %v, want the injected lost reply", err)
	}
	outcome, err := store.Apply(ctx, request)
	if err != nil || outcome != ApplyApplied {
		t.Fatalf("retry of the same request after a lost reply = %v, %v; want ApplyApplied (it took effect the first time)", outcome, err)
	}
	other := after.Clone()
	other.LastError = "a different request at the same version"
	if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: 1, After: other}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a different request from the old version = %v, want ErrConflict", err)
	}
}

// TestRedisScriptValidatesEverythingBeforeWriting：每个脚本遇到类型不对的键、不存在的记录或不属于本 saga 的操作时返回
// 业务结论，前缀下的全部键与调用前逐字节相同。
func TestRedisScriptValidatesEverythingBeforeWriting(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	pending := func(id string) Record {
		return Record{ID: id, Type: "rally", DefinitionVersion: 1, BusinessKey: "bk-" + id, Status: StatusPending, Phase: PhaseForward, Version: 1, NextRunAt: now, CreatedAt: now, UpdatedAt: now}
	}
	dispatch := func(r Record, attempt uint32) (Record, *OutboxRecord) {
		r = r.Clone()
		r.Version++
		r.Status, r.Attempt = StatusWaiting, attempt
		r.OperationKey = operationKey(r.ID, r.Phase, r.Step)
		r.CommandID = commandID(r.OperationKey, 0, attempt)
		command := Command{ID: r.CommandID, IdempotencyKey: r.OperationKey, SagaID: r.ID, SagaType: r.Type, DefinitionVersion: 1, BusinessKey: r.BusinessKey, Step: 0, StepName: "reserve", Phase: r.Phase, Attempt: attempt, Topic: "reserve", DeadlineAt: now.Add(time.Minute), CreatedAt: now}
		return r, &OutboxRecord{Command: command, NextAttemptAt: now, CreatedAt: now}
	}
	type setup struct {
		name    string
		prepare func(t *testing.T, env *redisTestEnv, store *RedisStore) func() error
		want    error
	}
	corrupt := func(key func(store *RedisStore) string) func(t *testing.T, env *redisTestEnv, store *RedisStore) {
		return func(t *testing.T, env *redisTestEnv, store *RedisStore) {
			if err := env.raw.Del(ctx, key(store)).Err(); err != nil {
				t.Fatal(err)
			}
			if err := env.raw.RPush(ctx, key(store), "not-the-right-type").Err(); err != nil {
				t.Fatal(err)
			}
		}
	}
	waitingWithOutbox := func(t *testing.T, store *RedisStore, id string) Record {
		r := pending(id)
		if err := store.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
		w, outbox := dispatch(r, 1)
		if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: 1, After: w, Outbox: outbox}); err != nil {
			t.Fatal(err)
		}
		return w
	}
	cases := []setup{
		{name: "create: wrong-type due", prepare: func(t *testing.T, env *redisTestEnv, store *RedisStore) func() error {
			corrupt(func(s *RedisStore) string { return s.keys.due() })(t, env, store)
			return func() error { return store.Create(ctx, pending("c1")) }
		}, want: ErrInvalidRecord},
		{name: "claim due: wrong-type due", prepare: func(t *testing.T, env *redisTestEnv, store *RedisStore) func() error {
			corrupt(func(s *RedisStore) string { return s.keys.due() })(t, env, store)
			return func() error {
				_, err := store.ClaimDue(ctx, ClaimRequest{Owner: "a", Now: now, LeaseDuration: time.Second, Limit: 4})
				return err
			}
		}, want: ErrInvalidRecord},
		{name: "claim outbox: wrong-type outbox", prepare: func(t *testing.T, env *redisTestEnv, store *RedisStore) func() error {
			corrupt(func(s *RedisStore) string { return s.keys.outbox() })(t, env, store)
			return func() error {
				_, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "a", Now: now, LeaseDuration: time.Second, Limit: 4})
				return err
			}
		}, want: ErrInvalidRecord},
		{name: "apply: record missing", prepare: func(t *testing.T, env *redisTestEnv, store *RedisStore) func() error {
			w, outbox := dispatch(pending("absent"), 1)
			return func() error {
				_, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: 1, After: w, Outbox: outbox})
				return err
			}
		}, want: ErrNotFound},
		{name: "apply: wrong-type log", prepare: func(t *testing.T, env *redisTestEnv, store *RedisStore) func() error {
			r := pending("a-log")
			if err := store.Create(ctx, r); err != nil {
				t.Fatal(err)
			}
			corrupt(func(s *RedisStore) string { return s.keys.log(s.keys.partition("a-log")) })(t, env, store)
			w, outbox := dispatch(r, 1)
			return func() error {
				_, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: 1, After: w, Outbox: outbox})
				return err
			}
		}, want: ErrInvalidRecord},
		{name: "apply: the replaced command is not a hash", prepare: func(t *testing.T, env *redisTestEnv, store *RedisStore) func() error {
			w := waitingWithOutbox(t, store, "a-old")
			corrupt(func(s *RedisStore) string { return s.keys.command(w.CommandID) })(t, env, store)
			second, outbox := dispatch(w, 2)
			return func() error {
				_, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: w.Version, After: second, Outbox: outbox})
				return err
			}
		}, want: ErrInvalidRecord},
		{name: "apply: the operation index is not a string", prepare: func(t *testing.T, env *redisTestEnv, store *RedisStore) func() error {
			w := waitingWithOutbox(t, store, "a-index")
			corrupt(func(s *RedisStore) string { return s.keys.prefix + ":obk:" + w.OperationKey })(t, env, store)
			second, outbox := dispatch(w, 2)
			return func() error {
				_, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: w.Version, After: second, Outbox: outbox})
				return err
			}
		}, want: ErrInvalidRecord},
		{name: "apply: closing an operation whose command is not a hash", prepare: func(t *testing.T, env *redisTestEnv, store *RedisStore) func() error {
			w := waitingWithOutbox(t, store, "a-close")
			corrupt(func(s *RedisStore) string { return s.keys.command(w.CommandID) })(t, env, store)
			failed := w.Clone()
			failed.Version++
			failed.Status, failed.OperationKey, failed.CommandID, failed.NextRunAt = StatusFailed, "", "", time.Time{}
			return func() error {
				_, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: w.Version, After: failed, CloseOperation: w.OperationKey})
				return err
			}
		}, want: ErrInvalidRecord},
		{name: "apply: closing an operation whose index is not a string", prepare: func(t *testing.T, env *redisTestEnv, store *RedisStore) func() error {
			w := waitingWithOutbox(t, store, "a-close-index")
			corrupt(func(s *RedisStore) string { return s.keys.prefix + ":obk:" + w.OperationKey })(t, env, store)
			failed := w.Clone()
			failed.Version++
			failed.Status, failed.OperationKey, failed.CommandID, failed.NextRunAt = StatusFailed, "", "", time.Time{}
			return func() error {
				_, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: w.Version, After: failed, CloseOperation: w.OperationKey})
				return err
			}
		}, want: ErrInvalidRecord},
		{name: "apply: closing another saga's operation (script check)", prepare: func(t *testing.T, env *redisTestEnv, store *RedisStore) func() error {
			r := pending("a-foreign")
			if err := store.Create(ctx, r); err != nil {
				t.Fatal(err)
			}
			after := r.Clone()
			after.Version++
			return func() error {
				_, err := store.applyScript(ctx, ApplyRequest{ExpectedVersion: 1, After: after, CloseOperation: operationKey("someone-else", PhaseForward, 0)})
				return err
			}
		}, want: ErrIdentityConflict},
		{name: "ack: wrong-type outbox", prepare: func(t *testing.T, env *redisTestEnv, store *RedisStore) func() error {
			w := waitingWithOutbox(t, store, "k-type")
			held, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "p", Now: now, LeaseDuration: time.Minute, Limit: 1})
			if err != nil || len(held) != 1 {
				t.Fatalf("claim = %+v, %v", held, err)
			}
			corrupt(func(s *RedisStore) string { return s.keys.outbox() })(t, env, store)
			return func() error { return store.AckOutbox(ctx, w.CommandID, held[0].Lease) }
		}, want: ErrInvalidRecord},
		{name: "ack: the operation index is not a string", prepare: func(t *testing.T, env *redisTestEnv, store *RedisStore) func() error {
			w := waitingWithOutbox(t, store, "k-index")
			held, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "p", Now: now, LeaseDuration: time.Minute, Limit: 1})
			if err != nil || len(held) != 1 {
				t.Fatalf("claim = %+v, %v", held, err)
			}
			corrupt(func(s *RedisStore) string { return s.keys.prefix + ":obk:" + w.OperationKey })(t, env, store)
			return func() error { return store.AckOutbox(ctx, w.CommandID, held[0].Lease) }
		}, want: ErrInvalidRecord},
		{name: "nack: wrong-type outbox", prepare: func(t *testing.T, env *redisTestEnv, store *RedisStore) func() error {
			w := waitingWithOutbox(t, store, "n-type")
			held, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "p", Now: now, LeaseDuration: time.Minute, Limit: 1})
			if err != nil || len(held) != 1 {
				t.Fatalf("claim = %+v, %v", held, err)
			}
			corrupt(func(s *RedisStore) string { return s.keys.outbox() })(t, env, store)
			return func() error { return store.NackOutbox(ctx, w.CommandID, held[0].Lease, now.Add(time.Minute), "x") }
		}, want: ErrInvalidRecord},
		{name: "alarm: record missing", prepare: func(t *testing.T, env *redisTestEnv, store *RedisStore) func() error {
			completion := Completion{CommandID: "gone:1:0:1", IdempotencyKey: operationKey("gone", PhaseForward, 0), SagaID: "gone", Success: true}
			return func() error {
				if marked, err := store.MarkLateSuccessAlarm(ctx, completion, 0); err != nil || marked {
					return fmt.Errorf("mark = %v, %w", marked, err)
				}
				return nil
			}
		}, want: nil},
		{name: "alarm: wrong-type log", prepare: func(t *testing.T, env *redisTestEnv, store *RedisStore) func() error {
			r := pending("al-log")
			if err := store.Create(ctx, r); err != nil {
				t.Fatal(err)
			}
			op := operationKey(r.ID, PhaseForward, 0)
			seedTombstone(t, store, r.ID, op, operationClosureAbandoned, now)
			corrupt(func(s *RedisStore) string { return s.keys.log(s.keys.partition(r.ID)) })(t, env, store)
			completion := Completion{CommandID: op + ":1", IdempotencyKey: op, SagaID: r.ID, Success: true}
			return func() error {
				_, err := store.MarkLateSuccessAlarm(ctx, completion, 0)
				return err
			}
		}, want: ErrInvalidRecord},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := newRedisTestEnv(t)
			store := env.open(t)
			call := c.prepare(t, env, store)
			before := env.snapshotKeys(t)
			err := call()
			if !errors.Is(err, c.want) {
				t.Fatalf("call = %v, want %v", err, c.want)
			}
			after := env.snapshotKeys(t)
			if !maps.Equal(before, after) {
				var changed []string
				for key := range maps.Keys(after) {
					if before[key] != after[key] {
						changed = append(changed, key)
					}
				}
				for key := range maps.Keys(before) {
					if _, ok := after[key]; !ok {
						changed = append(changed, key+" (deleted)")
					}
				}
				t.Fatalf("a refused call wrote to redis: changed %v", slices.Sorted(slices.Values(changed)))
			}
		})
	}
}

// slowMongo 让冷副本的 sagas 读多等一会（模拟 Create 的 Mongo 读与脚本之间的长停顿）。
type slowMongo struct {
	fmongo.IMongo
	delay func() time.Duration
}

func (m slowMongo) Database(name string) fmongo.IDatabase {
	return slowDatabase{IDatabase: m.IMongo.Database(name), delay: m.delay}
}

type slowDatabase struct {
	fmongo.IDatabase
	delay func() time.Duration
}

func (d slowDatabase) Collection(name string) fmongo.ICollection {
	return slowCollection{ICollection: d.IDatabase.Collection(name), delay: d.delay, sagas: name == defaultSagaCollection}
}

type slowCollection struct {
	fmongo.ICollection
	delay func() time.Duration
	sagas bool
}

func (c slowCollection) FindOne(ctx context.Context, filter any, result any) error {
	err := c.ICollection.FindOne(ctx, filter, result)
	if c.sagas {
		time.Sleep(c.delay())
	}
	return err
}

func TestRedisCreateGuardRejectsStaleMongoRead(t *testing.T) {
	ctx := context.Background()
	addr := redistest.AOFAddr(t)
	var slowReads atomic.Int64
	slowReads.Store(1 << 30)
	mongo := slowMongo{IMongo: mongotest.NewClient(), delay: func() time.Duration {
		if slowReads.Add(-1) >= 0 {
			return 60 * time.Millisecond
		}
		return 0
	}}
	env := newRedisTestEnvAt(t, addr, redistest.Prefix(t, addr, "sagaguard"), mongo)
	store, err := env.build(nil, RedisStoreOptions{BusinessKeyGuard: 80 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	slowReads.Store(0)
	if err := store.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	record := Record{ID: "guard", Type: "rally", DefinitionVersion: 1, BusinessKey: "guard", Status: StatusPending, Phase: PhaseForward, Version: 1, NextRunAt: now, CreatedAt: now, UpdatedAt: now}
	slowReads.Store(3)
	if err := store.Create(ctx, record); !errors.Is(err, ErrConflict) {
		t.Fatalf("create whose Mongo read always exceeds guard/2 = %v, want ErrConflict after redoing the read", err)
	}
	if _, err := store.Get(ctx, record.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the refused create left a record: %v", err)
	}
	slowReads.Store(1)
	if err := store.Create(ctx, record); err != nil {
		t.Fatalf("create whose first Mongo read was slow = %v, want it to redo the read and succeed", err)
	}
}

func TestRedisCreateBackpressureRefusesNewSagas(t *testing.T) {
	ctx := context.Background()
	env := newRedisTestEnv(t)
	store, err := env.build(nil, RedisStoreOptions{Partitions: 1, MaxProjectionBacklog: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	for i := range 3 {
		id := fmt.Sprintf("bp-%d", i)
		err := store.Create(ctx, Record{ID: id, Type: "rally", DefinitionVersion: 1, BusinessKey: id, Status: StatusPending, Phase: PhaseForward, Version: 1, NextRunAt: now, CreatedAt: now, UpdatedAt: now})
		if i < 2 && err != nil {
			t.Fatalf("create %d = %v", i, err)
		}
		if i == 2 && !errors.Is(err, ErrBackpressure) {
			t.Fatalf("create past the projection backlog = %v, want ErrBackpressure", err)
		}
	}
}

// TestRedisCreateKeepsBusinessKeyUniqueAfterEviction：记录驱逐出 Redis（只在 Mongo 冷副本里，业务键守卫也已过期）之后，同一业务键
// 再启动：同意图返回原记录，不同意图 ErrIdentityConflict。业务键在冷副本里永久唯一，靠 Create 先读 Mongo。
func TestRedisCreateKeepsBusinessKeyUniqueAfterEviction(t *testing.T) {
	ctx := context.Background()
	env := newRedisTestEnv(t)
	store := env.open(t)
	engine, err := NewEngine(store, PublishFunc(func(context.Context, Command) error { return nil }), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Register(testDefinition()); err != nil {
		t.Fatal(err)
	}
	request := StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "evicted", Data: []byte("intent")}
	started, err := engine.StartSaga(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	done := started.Clone()
	done.Version++
	done.Status, done.NextRunAt, done.UpdatedAt = StatusFailed, time.Time{}, started.UpdatedAt.Add(time.Millisecond)
	if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: started.Version, After: done}); err != nil {
		t.Fatal(err)
	}
	final, err := store.Get(ctx, started.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 驱逐：冷副本有最新版本，Redis 里记录与业务键守卫都不在了。
	if _, err := store.copy.sagas().InsertOne(ctx, toRecordDoc(final)); err != nil {
		t.Fatal(err)
	}
	if err := env.raw.Del(ctx, store.keys.record(final.ID), store.keys.businessKey(final.Type, final.BusinessKey)).Err(); err != nil {
		t.Fatal(err)
	}
	again, err := engine.StartSaga(ctx, request)
	if err != nil || again.ID != final.ID || again.Status != StatusFailed {
		t.Fatalf("same intent after eviction = %+v, %v; want the original record", again, err)
	}
	other := request
	other.Data = []byte("another intent")
	if _, err := engine.StartSaga(ctx, other); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("different intent after eviction = %v, want ErrIdentityConflict", err)
	}
	if exists, _ := env.raw.Exists(ctx, store.keys.businessKey(final.Type, final.BusinessKey)).Result(); exists != 0 {
		t.Fatal("a refused start re-created the business key guard")
	}
}

func TestRedisWaitAOFUnconfirmedFailsStop(t *testing.T) {
	ctx := context.Background()
	server := redistest.StartAOF(t)
	env := newRedisTestEnvAt(t, server.Addr, "sagaaof", mongotest.NewClient())
	t.Run("startup refuses a redis without appendonly", func(t *testing.T) {
		if err := env.raw.ConfigSet(ctx, "appendonly", "no").Err(); err != nil {
			t.Fatal(err)
		}
		defer env.raw.ConfigSet(ctx, "appendonly", "yes")
		store, err := env.build(nil, RedisStoreOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.EnsureInfrastructure(ctx); err == nil || !strings.Contains(err.Error(), "appendonly") {
			t.Fatalf("EnsureInfrastructure on appendonly no = %v, want a refusal naming appendonly", err)
		}
	})
	t.Run("a running store fail-stops when WAITAOF cannot confirm", func(t *testing.T) {
		waitAOFRewrite(t, env.raw)
		failure := app.NewRuntimeFailure()
		store, err := env.build(failure, RedisStoreOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.EnsureInfrastructure(ctx); err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC().Truncate(time.Millisecond)
		record := Record{ID: "aof", Type: "rally", DefinitionVersion: 1, BusinessKey: "aof", Status: StatusPending, Phase: PhaseForward, Version: 1, NextRunAt: now, CreatedAt: now, UpdatedAt: now}
		if err := store.Create(ctx, record); err != nil {
			t.Fatal(err)
		}
		if err := env.raw.ConfigSet(ctx, "appendonly", "no").Err(); err != nil {
			t.Fatal(err)
		}
		defer env.raw.ConfigSet(ctx, "appendonly", "yes")
		after := record.Clone()
		after.Version = 2
		after.LastError = "written while appendonly is off"
		if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: 1, After: after}); !errors.Is(err, ErrDurabilityUnconfirmed) {
			t.Fatalf("apply with appendonly off = %v, want ErrDurabilityUnconfirmed", err)
		}
		select {
		case err := <-failure.Done():
			if !errors.Is(err, ErrDurabilityUnconfirmed) {
				t.Fatalf("runtime failure = %v", err)
			}
		default:
			t.Fatal("the unconfirmed write did not report a runtime failure")
		}
		got, err := store.Get(ctx, record.ID)
		if err != nil || got.Version != 2 {
			t.Fatalf("record after the unconfirmed write = %+v, %v; the write executed and must not be rolled back", got, err)
		}
	})
}

// waitAOFRewrite 等 appendonly 从 no 切回 yes 时触发的后台重写结束：重写期间 WAITAOF 会等。
func waitAOFRewrite(t *testing.T, raw *goredis.Client) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		info, err := raw.Info(context.Background(), "persistence").Result()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(info, "aof_enabled:1") && strings.Contains(info, "aof_rewrite_in_progress:0") && !strings.Contains(info, "aof_rewrite_scheduled:1") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("AOF did not come back on")
}

func TestRedisAuthorityGuardRefusesEmptyRedis(t *testing.T) {
	ctx := context.Background()
	env := newRedisTestEnv(t)
	env.open(t)
	if _, err := redistest.DeleteKeys(ctx, env.addr, env.prefix); err != nil {
		t.Fatal(err)
	}
	store, err := env.build(nil, RedisStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureInfrastructure(ctx); !errors.Is(err, ErrRedisAuthorityMismatch) {
		t.Fatalf("EnsureInfrastructure on a flushed authority redis = %v, want ErrRedisAuthorityMismatch", err)
	}
}

func TestRedisAuthorityGuardChecksInstanceFormatAndPartitions(t *testing.T) {
	ctx := context.Background()
	env := newRedisTestEnv(t)
	store := env.open(t)
	reopen := func(options RedisStoreOptions) error {
		next, err := env.build(nil, options)
		if err != nil {
			t.Fatal(err)
		}
		return next.EnsureInfrastructure(ctx)
	}
	t.Run("another instance", func(t *testing.T) {
		if err := env.raw.HSet(ctx, store.keys.meta(), "instance", "someone-else").Err(); err != nil {
			t.Fatal(err)
		}
		defer env.raw.HSet(ctx, store.keys.meta(), "instance", func() string {
			var meta sagaMetaDoc
			_ = store.copy.meta().FindOne(ctx, bson.M{"_id": "redis"}, &meta)
			return meta.Instance
		}())
		if err := reopen(RedisStoreOptions{}); !errors.Is(err, ErrRedisAuthorityMismatch) {
			t.Fatalf("instance mismatch = %v, want ErrRedisAuthorityMismatch", err)
		}
	})
	t.Run("the meta script refuses another instance by itself", func(t *testing.T) {
		code, _, err := store.run(ctx, "meta", []string{store.keys.meta()}, "someone-else", store.opts.Partitions, redisFormat, "0")
		if err != nil || code != "MISMATCH" {
			t.Fatalf("meta with another instance = %q, %v; want MISMATCH", code, err)
		}
	})
	t.Run("another format", func(t *testing.T) {
		if err := env.raw.HSet(ctx, store.keys.meta(), "format", "0").Err(); err != nil {
			t.Fatal(err)
		}
		defer env.raw.HSet(ctx, store.keys.meta(), "format", redisFormat)
		if err := reopen(RedisStoreOptions{}); !errors.Is(err, ErrRedisAuthorityMismatch) {
			t.Fatalf("format mismatch = %v, want ErrRedisAuthorityMismatch", err)
		}
	})
	t.Run("partitions change only with an empty log", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Millisecond)
		if err := store.Create(ctx, Record{ID: "p1", Type: "rally", DefinitionVersion: 1, BusinessKey: "p1", Status: StatusPending, Phase: PhaseForward, Version: 1, NextRunAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := reopen(RedisStoreOptions{Partitions: 4}); !errors.Is(err, ErrRedisAuthorityMismatch) {
			t.Fatalf("partitions change with a backlog = %v, want ErrRedisAuthorityMismatch", err)
		}
		code, _, err := store.run(ctx, "meta", []string{store.keys.meta()}, func() string {
			var meta sagaMetaDoc
			_ = store.copy.meta().FindOne(ctx, bson.M{"_id": "redis"}, &meta)
			return meta.Instance
		}(), 4, redisFormat, "0")
		if err != nil || code != "PARTITIONS" {
			t.Fatalf("meta script changing partitions without the drained flag = %q, %v; want PARTITIONS", code, err)
		}
		if err := env.raw.Del(ctx, store.keys.log(store.keys.partition("p1"))).Err(); err != nil {
			t.Fatal(err)
		}
		if err := reopen(RedisStoreOptions{Partitions: 4}); err != nil {
			t.Fatalf("partitions change with every log drained = %v, want nil", err)
		}
	})
	t.Run("meta of the wrong type", func(t *testing.T) {
		if err := env.raw.Del(ctx, store.keys.meta()).Err(); err != nil {
			t.Fatal(err)
		}
		if err := env.raw.RPush(ctx, store.keys.meta(), "x").Err(); err != nil {
			t.Fatal(err)
		}
		if err := reopen(RedisStoreOptions{Partitions: 4}); err == nil {
			t.Fatal("EnsureInfrastructure with a wrong-type meta key = nil, want a refusal")
		}
		if code, _, err := store.run(ctx, "meta", []string{store.keys.meta()}, "x", 4, redisFormat, "1"); code != "CORRUPT" || !errors.Is(err, ErrInvalidRecord) {
			t.Fatalf("meta on a wrong-type key = %q, %v; want CORRUPT", code, err)
		}
	})
}

// BenchmarkRedisClaimDue 测领取在积压 1k / 10k / 100k 时的单次代价（O(log N + limit)）。积压是 next_run 在过去的
// 非终态记录；每次领取 3 条。
func BenchmarkRedisClaimDue(b *testing.B) {
	for _, backlog := range []int{1_000, 10_000, 100_000} {
		b.Run(fmt.Sprint(backlog), func(b *testing.B) {
			ctx := context.Background()
			env := newRedisTestEnv(b)
			store := env.open(b)
			now := time.Now().UTC().Truncate(time.Millisecond)
			pipe := env.raw.Pipeline()
			for i := range backlog {
				id := fmt.Sprintf("bench-%07d", i)
				doc, err := encodeRecordDoc(Record{ID: id, Type: "rally", DefinitionVersion: 1, BusinessKey: id, Status: StatusPending, Phase: PhaseForward, Version: 1, NextRunAt: now.Add(-time.Hour), CreatedAt: now, UpdatedAt: now})
				if err != nil {
					b.Fatal(err)
				}
				pipe.HSet(ctx, store.keys.record(id), "v", 1, "st", int(StatusPending), "nr", now.Add(-time.Hour).UnixMilli(), "lo", "", "lt", 0, "lu", 0, "ta", 0, "la", "", "doc", doc)
				pipe.ZAdd(ctx, store.keys.due(), goredis.Z{Score: float64(now.Add(-time.Hour).UnixMilli()), Member: id})
				if i%1000 == 999 {
					if _, err := pipe.Exec(ctx); err != nil {
						b.Fatal(err)
					}
				}
			}
			if _, err := pipe.Exec(ctx); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			i := 0
			for b.Loop() {
				// 领到的记录租约 1ms，下一轮时间前进 2ms：它们排到积压之后，积压大小保持不变。
				i++
				claimed, err := store.ClaimDue(ctx, ClaimRequest{Owner: "bench", Now: now.Add(time.Duration(i) * 2 * time.Millisecond), LeaseDuration: time.Millisecond, Limit: 3})
				if err != nil || len(claimed) == 0 {
					b.Fatalf("claim = %d, %v", len(claimed), err)
				}
			}
		})
	}
}

func TestResumePersistsGenerationAndAcceptsFreshCompletion(t *testing.T) {
	for _, phase := range []Phase{PhaseForward, PhaseCompensate} {
		t.Run(phase.String(), func(t *testing.T) {
			ctx := context.Background()
			env := newRedisTestEnv(t)
			store := env.open(t)
			newEngine := func() *Engine {
				e, err := NewEngine(store, PublishFunc(func(context.Context, Command) error { return nil }), DefaultOptions())
				if err != nil {
					t.Fatal(err)
				}
				if err := e.Register(testDefinition()); err != nil {
					t.Fatal(err)
				}
				return e
			}
			e := newEngine()
			now := time.Now().UTC().Truncate(time.Millisecond)
			r := Record{ID: "resume", Type: "rally", DefinitionVersion: 1, BusinessKey: "resume", Status: StatusPending, Phase: phase, Version: 1, NextRunAt: now, CreatedAt: now, UpdatedAt: now}
			if phase == PhaseCompensate {
				r.Status = StatusCompensating
				r.CompletedSteps = 1
			}
			if err := store.Create(ctx, r); err != nil {
				t.Fatal(err)
			}
			var oldIDs []string
			for generation := uint32(0); generation < 3; generation++ {
				// 新协调器/新 Store 从持久来源恢复；不使用 Resume 返回的内存对象派发。
				store = env.open(t)
				e = newEngine()
				before, err := store.Get(ctx, r.ID)
				if err != nil {
					t.Fatal(err)
				}
				if before.Incarnation != generation {
					t.Errorf("persisted generation=%d, want %d", before.Incarnation, generation)
				}
				if err := e.processClaimed(ctx, before, now); err != nil {
					t.Fatal(err)
				}
				waiting, err := store.Get(ctx, r.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, old := range oldIDs {
					if waiting.CommandID == old {
						t.Errorf("redispatch reused old command ID %q at generation %d", old, generation)
					}
				}
				oldIDs = append(oldIDs, waiting.CommandID)
				items, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "review", Now: now, LeaseDuration: time.Minute, Limit: 1})
				if err != nil {
					t.Fatal(err)
				}
				if len(items) != 1 || items[0].Command.ID != waiting.CommandID {
					t.Fatalf("outbox did not carry current command: %+v", items)
				}
				result := Completion{CommandID: waiting.CommandID, IdempotencyKey: waiting.OperationKey, SagaID: r.ID}
				if generation == 2 {
					result.Success = true
				} else {
					result.Error = "repair needed"
				}
				after, err := e.Complete(ctx, result)
				if err != nil {
					t.Fatalf("generation %d fresh completion failed: %v", generation, err)
				}
				if generation == 2 {
					want := StatusPending
					if phase == PhaseCompensate {
						want = StatusCompensated
					}
					if after.Status != want {
						t.Fatalf("completion did not advance: %+v", after)
					}
					if _, err := e.Complete(ctx, result); err != nil {
						t.Fatalf("same receipt redelivery: %v", err)
					}
					break
				}
				want := StatusFailed
				if phase == PhaseCompensate {
					want = StatusManualRequired
				}
				if after.Status != want {
					t.Fatalf("failure did not enter repair state: %+v", after)
				}
				resumed, err := e.Resume(ctx, ResumeRequest{ID: r.ID, Now: now})
				if err != nil {
					t.Fatal(err)
				}
				if resumed.Incarnation != generation+1 {
					t.Errorf("Resume generation=%d, want %d", resumed.Incarnation, generation+1)
				}
			}
		})
	}
}

func TestIncarnationSurvivesEveryRecordReadAndReplace(t *testing.T) {
	for _, generation := range []uint32{0, 1, 17, ^uint32(0)} {
		t.Run(fmt.Sprint(generation), func(t *testing.T) {
			ctx := context.Background()
			store := newTestRedisStore(t)
			now := time.Now().UTC().Truncate(time.Millisecond)
			r := Record{ID: "compat", Type: "rally", DefinitionVersion: 1, BusinessKey: "compat", Status: StatusPending, Phase: PhaseForward, Incarnation: generation, Version: 1, NextRunAt: now, CreatedAt: now, UpdatedAt: now}
			if err := store.Create(ctx, r); err != nil {
				t.Fatal(err)
			}
			for _, read := range []func() (Record, error){func() (Record, error) { return store.Get(ctx, r.ID) }, func() (Record, error) { return store.GetByBusinessKey(ctx, r.Type, r.BusinessKey) }} {
				got, err := read()
				if err != nil {
					t.Fatal(err)
				}
				if got.Incarnation != generation {
					t.Errorf("record read lost generation: got %d want %d", got.Incarnation, generation)
				}
			}
			rows, err := store.List(ctx, Query{Limit: 1})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].Incarnation != generation {
				t.Fatalf("List lost generation: %+v", rows)
			}
			r.Version++
			if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: 1, After: r}); err != nil {
				t.Fatal(err)
			}
			got, err := store.Get(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Incarnation != generation || got.Version != 2 {
				t.Fatalf("replacement lost generation: %+v", got)
			}
		})
	}
}

func TestRedisStoreSkipsACorruptRecordWithoutFailingTheBatch(t *testing.T) {
	ctx := context.Background()
	env := newRedisTestEnv(t)
	store := env.open(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	record := func(id string, offset time.Duration) Record {
		return Record{ID: id, Type: "rally", DefinitionVersion: 1, BusinessKey: "key-" + id, Status: StatusPending, Phase: PhaseForward, Version: 1,
			NextRunAt: now.Add(offset), CreatedAt: now, UpdatedAt: now.Add(offset)}
	}
	for _, r := range []Record{record("good-a", -2*time.Second), record("good-c", -time.Second)} {
		if err := store.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	// 坏记录排在最前（next_run_at 最早）：业务键被不兼容的写者丢掉。
	corrupt := record("corrupt-b", -3*time.Second)
	corrupt.BusinessKey = ""
	corruptDoc, err := json.Marshal(redisRecordDoc{ID: corrupt.ID, Type: corrupt.Type, DefinitionVersion: 1, Status: corrupt.Status, Phase: corrupt.Phase, Version: 1,
		NextRunAt: corrupt.NextRunAt.UnixMilli(), CreatedAt: now.UnixMilli(), UpdatedAt: corrupt.UpdatedAt.UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.raw.HSet(ctx, store.keys.record(corrupt.ID), "v", 1, "st", int(StatusPending), "nr", corrupt.NextRunAt.UnixMilli(), "lo", "", "lt", 0, "lu", 0, "doc", string(corruptDoc)).Err(); err != nil {
		t.Fatal(err)
	}
	if err := env.raw.ZAdd(ctx, store.keys.due(), goredis.Z{Score: float64(corrupt.NextRunAt.UnixMilli()), Member: corrupt.ID}).Err(); err != nil {
		t.Fatal(err)
	}
	ids := func(records []Record) []string {
		out := make([]string, 0, len(records))
		for _, r := range records {
			out = append(out, r.ID)
		}
		sort.Strings(out)
		return out
	}

	before := counterValue("saga.store.corrupt_record_total")
	claimed, err := store.ClaimDue(ctx, ClaimRequest{Owner: "coordinator", Now: now, LeaseDuration: 15 * time.Second, Limit: 10})
	if err != nil {
		t.Errorf("ClaimDue with one corrupt record among three = (%v, %v); want the two good records and no error — the coordinator discards a batch that comes back with an error, so the good records wait out their lease", ids(claimed), err)
	}
	if got := ids(claimed); len(got) != 2 || got[0] != "good-a" || got[1] != "good-c" {
		t.Errorf("ClaimDue returned %v, want [good-a good-c]", got)
	}
	if grown := counterValue("saga.store.corrupt_record_total") - before; grown != 1 {
		t.Errorf("ClaimDue: saga.store.corrupt_record_total grew by %d, want 1", grown)
	}

	before = counterValue("saga.store.corrupt_record_total")
	listed, err := store.List(ctx, Query{Limit: 100})
	if err != nil {
		t.Errorf("List with one corrupt record = (%v, %v); want the good records and no error", ids(listed), err)
	}
	if got := ids(listed); len(got) != 2 || got[0] != "good-a" || got[1] != "good-c" {
		t.Errorf("List returned %v, want [good-a good-c]", got)
	}
	if grown := counterValue("saga.store.corrupt_record_total") - before; grown != 1 {
		t.Errorf("List: saga.store.corrupt_record_total grew by %d, want 1", grown)
	}
	// 坏记录原样保留，不被改写（领取只改租约字段）。
	if doc, err := env.raw.HGet(ctx, store.keys.record(corrupt.ID), "doc").Result(); err != nil || doc != string(corruptDoc) {
		t.Fatalf("the corrupt record was changed: %q err=%v", doc, err)
	}
}

// 协调器 A 领取记录后租约过期、B 接管，A 之后才执行 Apply：A 的晚到 Apply 一律 ErrConflict，不覆盖 B 的结论、
// 不重复推进（attempt 不多加、outbox 不多出一条命令）。fence 由 apply.lua 的版本与租约校验给出：version 相等，
// 且 ExpectedLease 的 owner + token 相等（claim_due.lua 每次领取 token 加一）。覆盖三种到达顺序：B 已领取未 Apply、
// B 已 Apply、A 在超时分支上与 completion 交错。
func TestCoordinatorLeaseTakeoverFencesTheLateApply(t *testing.T) {
	runCoordinatorTakeoverCases(t, func(t *testing.T) *RedisStore { return newTestRedisStore(t) })
}

func runCoordinatorTakeoverCases(t *testing.T, newStore func(*testing.T) *RedisStore) {
	ctx := context.Background()
	setup := func(t *testing.T, status Status) (*RedisStore, *Engine, *Engine, Record, time.Time) {
		t.Helper()
		store := newStore(t)
		newEngine := func(owner string) *Engine {
			options := DefaultOptions()
			options.Owner = owner
			engine, err := NewEngine(store, PublishFunc(func(context.Context, Command) error { return nil }), options)
			if err != nil {
				t.Fatal(err)
			}
			if err := engine.Register(testDefinition()); err != nil {
				t.Fatal(err)
			}
			return engine
		}
		now := time.Now().UTC().Truncate(time.Millisecond)
		id := "takeover-" + NewID()
		record := Record{ID: id, Type: "rally", DefinitionVersion: 1, BusinessKey: id, Status: StatusPending, Phase: PhaseForward, Version: 1, NextRunAt: now, CreatedAt: now, UpdatedAt: now}
		if err := store.Create(ctx, record); err != nil {
			t.Fatal(err)
		}
		a, b := newEngine("coordinator-a"), newEngine("coordinator-b")
		if status == StatusWaiting {
			if err := a.processClaimed(ctx, record, now); err != nil {
				t.Fatal(err)
			}
			current, err := store.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			record = current
			now = current.NextRunAt // 第一次尝试的截止：协调器下一次领取它是为了判超时
		}
		return store, a, b, record, now
	}
	claim := func(t *testing.T, store *RedisStore, owner string, now time.Time) Record {
		t.Helper()
		records, err := store.ClaimDue(ctx, ClaimRequest{Owner: owner, Now: now, LeaseDuration: 2 * time.Second, Limit: 8})
		if err != nil {
			t.Fatal(err)
		}
		if len(records) != 1 {
			t.Fatalf("%s claimed %d records, want 1", owner, len(records))
		}
		return records[0]
	}
	queued := func(t *testing.T, store *RedisStore, now time.Time) []OutboxRecord {
		t.Helper()
		items, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "audit", Now: now.Add(time.Hour), LeaseDuration: time.Minute, Limit: 16})
		if err != nil {
			t.Fatal(err)
		}
		return items
	}

	t.Run("B claimed, A applies before B", func(t *testing.T) {
		store, a, b, _, now := setup(t, StatusPending)
		byA := claim(t, store, "coordinator-a", now)
		byB := claim(t, store, "coordinator-b", now.Add(3*time.Second)) // A 的租约已过期
		if err := a.processClaimed(ctx, byA, now.Add(3*time.Second)); !errors.Is(err, ErrConflict) {
			t.Fatalf("A's late dispatch after B took the lease = %v, want ErrConflict", err)
		}
		if err := b.processClaimed(ctx, byB, now.Add(3*time.Second)); err != nil {
			t.Fatalf("B's dispatch after A's rejected one = %v", err)
		}
		final, err := store.Get(ctx, byB.ID)
		if err != nil {
			t.Fatal(err)
		}
		if final.Status != StatusWaiting || final.Attempt != 1 || final.Version != byB.Version+1 {
			t.Fatalf("after takeover: %+v, want one dispatch (attempt 1)", final)
		}
		if items := queued(t, store, now); len(items) != 1 || items[0].Command.ID != final.CommandID {
			t.Fatalf("outbox after takeover: %+v, want exactly B's command %s", items, final.CommandID)
		}
	})

	t.Run("B already applied, A applies late", func(t *testing.T) {
		store, a, b, _, now := setup(t, StatusPending)
		byA := claim(t, store, "coordinator-a", now)
		byB := claim(t, store, "coordinator-b", now.Add(3*time.Second))
		if err := b.processClaimed(ctx, byB, now.Add(3*time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := a.processClaimed(ctx, byA, now.Add(4*time.Second)); !errors.Is(err, ErrConflict) {
			t.Fatalf("A's late dispatch after B applied = %v, want ErrConflict", err)
		}
		final, err := store.Get(ctx, byB.ID)
		if err != nil {
			t.Fatal(err)
		}
		if final.Attempt != 1 || final.Version != byB.Version+1 {
			t.Fatalf("A's late apply moved the record: %+v", final)
		}
		if items := queued(t, store, now); len(items) != 1 {
			t.Fatalf("outbox has %d commands, want 1", len(items))
		}
	})

	t.Run("A's timeout decision loses to B's and to a completion", func(t *testing.T) {
		store, a, b, waiting, now := setup(t, StatusWaiting)
		byA := claim(t, store, "coordinator-a", now) // A 领取它是为了判超时
		byB := claim(t, store, "coordinator-b", now.Add(3*time.Second))
		// 结果在 B 判超时之前到达：Complete 只按版本 fence，接收它。
		if _, err := b.Complete(ctx, Completion{CommandID: waiting.CommandID, IdempotencyKey: waiting.OperationKey, SagaID: waiting.ID, Success: true}); err != nil {
			t.Fatal(err)
		}
		if err := b.processClaimed(ctx, byB, now.Add(3*time.Second)); !errors.Is(err, ErrConflict) {
			t.Fatalf("B's timeout after the completion = %v, want ErrConflict", err)
		}
		if err := a.processClaimed(ctx, byA, now.Add(4*time.Second)); !errors.Is(err, ErrConflict) {
			t.Fatalf("A's late timeout = %v, want ErrConflict", err)
		}
		final, err := store.Get(ctx, waiting.ID)
		if err != nil {
			t.Fatal(err)
		}
		if final.Status != StatusPending || final.Step != 1 || final.CompletedSteps != 1 || final.Attempt != 0 {
			t.Fatalf("the accepted completion was overwritten by a stale timeout: %+v", final)
		}
	})
}

// 发布器与协调器在同一操作的 outbox 上交错：
//   - 发布器领取了第 1 次尝试的命令、正在发布，协调器判超时后派发第 2 次尝试（Apply 带 Outbox 时删掉同一操作的旧命令再写
//     新命令）：发布器之后的 Ack / Nack 都返回 nil（命令已不在，等价于已确认），不能把被替换的命令写回去；outbox 只剩第 2 次。
//   - 发布成功但 Ack 结果未知（Ack 没有执行）：租约过期后另一个发布器重新领取到同一条命令，CommandID 不变——它就是
//     JetStream 的 MsgID，去重窗口内的重发被 broker 去重，窗口外的重发由步骤收件箱按 CommandID 去重。
func TestOutboxSupersedeAndUnknownAck(t *testing.T) {
	ctx := context.Background()
	engine, store, record := waitingOnRedis(t)
	now := record.CreatedAt
	// waitingOnRedis 直接写入 Waiting 记录，没有 outbox；先按正常路径派发出第 1 次尝试的命令。
	pending := record.Clone()
	pending.Version++
	pending.Status, pending.Attempt, pending.OperationKey, pending.CommandID = StatusPending, 0, "", ""
	if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: record.Version, After: pending}); err != nil {
		t.Fatal(err)
	}
	if err := engine.processClaimed(ctx, pending, now); err != nil {
		t.Fatal(err)
	}
	first, err := store.Get(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "publisher-a", Now: now, LeaseDuration: 2 * time.Second, Limit: 4})
	if err != nil || len(claimed) != 1 || claimed[0].Command.ID != first.CommandID {
		t.Fatalf("publisher-a claim = %+v, %v; want the first attempt %s", claimed, err, first.CommandID)
	}

	t.Run("unknown ack: the same CommandID is published again after the lease", func(t *testing.T) {
		again, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "publisher-b", Now: now.Add(3 * time.Second), LeaseDuration: 2 * time.Second, Limit: 4})
		if err != nil || len(again) != 1 || again[0].Command.ID != first.CommandID {
			t.Fatalf("re-claim after publisher-a's lease = %+v, %v; want the same command %s", again, err, first.CommandID)
		}
		if err := store.AckOutbox(ctx, first.CommandID, claimed[0].Lease); !errors.Is(err, ErrConflict) {
			t.Fatalf("publisher-a's late ack after publisher-b took the lease = %v, want ErrConflict", err)
		}
		claimed = again
	})

	t.Run("supersede while a publisher holds the old command", func(t *testing.T) {
		// 第 1 次尝试超时：退避后派发第 2 次。
		if err := engine.processClaimed(ctx, first, first.NextRunAt); err != nil {
			t.Fatal(err)
		}
		backoff, err := store.Get(ctx, record.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := engine.processClaimed(ctx, backoff, backoff.NextRunAt); err != nil {
			t.Fatal(err)
		}
		second, err := store.Get(ctx, record.ID)
		if err != nil {
			t.Fatal(err)
		}
		if second.Attempt != 2 {
			t.Fatalf("want the second attempt dispatched: %+v", second)
		}
		// 被替换的命令已不在：Ack / Nack 都按“已确认”返回 nil，不把它写回去，也不计冲突。
		holder := claimed[0]
		if err := store.NackOutbox(ctx, holder.Command.ID, holder.Lease, now.Add(time.Minute), "publish failed"); err != nil {
			t.Fatalf("nack of a superseded command = %v, want nil", err)
		}
		if err := store.AckOutbox(ctx, holder.Command.ID, holder.Lease); err != nil {
			t.Fatalf("ack of a superseded command = %v, want nil", err)
		}
		items, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "audit", Now: now.Add(time.Hour), LeaseDuration: time.Minute, Limit: 8})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 || items[0].Command.ID != second.CommandID {
			t.Fatalf("outbox after supersede = %+v, want only %s", items, second.CommandID)
		}
	})
}

// 两个协调器 / 发布器同时领取：领取是一个脚本，候选扫描与写租约在服务端串行，两条到期项各给一个，不重复。
func TestConcurrentClaimsFillAvailableSlots(t *testing.T) {
	for _, outbox := range []bool{false, true} {
		t.Run(fmt.Sprint("outbox=", outbox), func(t *testing.T) {
			ctx := context.Background()
			store := newTestRedisStore(t)
			engine, err := NewEngine(store, PublishFunc(func(context.Context, Command) error { return nil }), DefaultOptions())
			if err != nil {
				t.Fatal(err)
			}
			if err := engine.Register(testDefinition()); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Millisecond)
			for i := range 2 {
				id := fmt.Sprint("claim-", i)
				r, err := engine.StartSaga(ctx, StartRequest{ID: id, Type: "rally", DefinitionVersion: 1, BusinessKey: id, Now: now})
				if err != nil {
					t.Fatal(err)
				}
				if outbox {
					if err := engine.processClaimed(ctx, r, now); err != nil {
						t.Fatal(err)
					}
				}
			}
			results := make(chan []string, 2)
			for i := range 2 {
				go func() {
					q := ClaimRequest{Owner: fmt.Sprint("worker-", i), Now: now, LeaseDuration: time.Minute, Limit: 1}
					var ids []string
					if outbox {
						rows, err := store.ClaimOutbox(ctx, q)
						if err != nil {
							t.Error(err)
						}
						for _, r := range rows {
							ids = append(ids, r.Command.ID)
						}
					} else {
						rows, err := store.ClaimDue(ctx, q)
						if err != nil {
							t.Error(err)
						}
						for _, r := range rows {
							ids = append(ids, r.ID)
						}
					}
					results <- ids
				}()
			}
			first, second := <-results, <-results
			if len(first) != 1 || len(second) != 1 {
				t.Fatalf("available=2 claimed=%v / %v; each worker should receive one", first, second)
			}
			if first[0] == second[0] {
				t.Fatalf("duplicate claim %s", first[0])
			}
		})
	}
}

// countingScripts 数每个脚本被调用的次数（按 SHA 对回脚本名），空领取（claim_* 没领到任何东西）单独计。
type countingScripts struct {
	*redisdriver.Client
	names map[string]string

	mu     sync.Mutex
	calls  map[string]int
	empty  map[string]int
	hmgets int
}

func (c *countingScripts) DurableScriptBatch(ctx context.Context, calls []fredis.ScriptCall, numLocal, numReplicas int, timeout time.Duration) ([]fredis.ScriptResult, int64, int64, error) {
	results, local, replicas, err := c.Client.DurableScriptBatch(ctx, calls, numLocal, numReplicas, timeout)
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, call := range calls {
		name := c.names[call.SHA]
		if err == nil && (name == "claim_due" || name == "claim_outbox") {
			if items, ok := results[i].Value.([]any); ok && len(items) == 1 {
				c.empty[name]++
				continue
			}
		}
		c.calls[name]++
	}
	return results, local, replicas, err
}

func (c *countingScripts) HMGet(ctx context.Context, key string, fields ...string) ([][]byte, error) {
	c.mu.Lock()
	c.hmgets++
	c.mu.Unlock()
	return c.Client.HMGet(ctx, key, fields...)
}

// TestRedisRoundTripsPerTwoStepSaga 核对 v2 §3.2 第 2 步 2a / 2b 的协调器往返数：一笔两步正向 saga = 启动 1 个脚本 + 每步
// （领取 1、派发 1、outbox 领取 1、确认 1、接收结果 1）5 个脚本，共 11 个脚本；接收结果前的 Get 是只读 HMGET，每步 1 次。
// 空领取（协调 / 发布循环被唤醒却没领到东西）不算在内，单独报告。
func TestRedisRoundTripsPerTwoStepSaga(t *testing.T) {
	ctx := context.Background()
	env := newRedisTestEnv(t)
	base := env.open(t)
	counter := &countingScripts{Client: env.client.(*redisdriver.Client), names: map[string]string{}, calls: map[string]int{}, empty: map[string]int{}}
	for name, script := range base.lua {
		counter.names[script.SHA] = name
	}
	store, err := NewRedisStore(counter, base.copy, nil, base.opts)
	if err != nil {
		t.Fatal(err)
	}
	commands := make(chan Command, 64)
	options := DefaultOptions()
	options.CoordinatorWorkers, options.PublisherWorkers = 1, 1
	options.PollInterval = time.Hour // 只靠唤醒，不做定时空领取
	engine, err := NewEngine(store, PublishFunc(func(_ context.Context, c Command) error { commands <- c; return nil }), options)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Register(testDefinition()); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = engine.Run(runCtx) }()
	const sagas = 20
	for i := range sagas {
		started, err := engine.StartSaga(ctx, StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "trip-" + string(rune('a'+i))})
		if err != nil {
			t.Fatal(err)
		}
		for step := 0; step < 2; step++ {
			select {
			case command := <-commands:
				if _, err := engine.Complete(ctx, Completion{CommandID: command.ID, IdempotencyKey: command.IdempotencyKey, SagaID: started.ID, Success: true}); err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("saga %d step %d was not dispatched", i, step)
			}
		}
	}
	// 等最后一条命令的确认写完。
	deadline := time.Now().Add(5 * time.Second)
	for {
		counter.mu.Lock()
		acks := counter.calls["ack_outbox"]
		counter.mu.Unlock()
		if acks >= 2*sagas || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	counter.mu.Lock()
	defer counter.mu.Unlock()
	scripts := 0
	for name, n := range counter.calls {
		if name != "meta" {
			scripts += n
		}
	}
	t.Logf("per saga: scripts %.2f %v; HMGET %.2f; empty claims %v", float64(scripts)/sagas, counter.calls, float64(counter.hmgets)/sagas, counter.empty)
	want := map[string]int{"create": sagas, "claim_due": 2 * sagas, "apply": 4 * sagas, "claim_outbox": 2 * sagas, "ack_outbox": 2 * sagas}
	for name, n := range want {
		if counter.calls[name] != n {
			t.Errorf("%s = %d calls for %d two-step sagas, want %d", name, counter.calls[name], sagas, n)
		}
	}
	if scripts != 11*sagas {
		t.Errorf("scripts = %d, want 11 per saga (%d)", scripts, 11*sagas)
	}
}
