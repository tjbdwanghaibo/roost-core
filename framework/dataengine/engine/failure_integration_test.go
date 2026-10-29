package engine

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
	"go.mongodb.org/mongo-driver/v2/bson"
	rawmongo "go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type recoveringPublisher struct {
	mu   sync.Mutex
	down bool
	ids  map[string]int
}

func (publisher *recoveringPublisher) Publish(_ context.Context, item OutboxItem) error {
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if publisher.down {
		return context.DeadlineExceeded
	}
	publisher.ids[item.Effect.ID]++
	return nil
}

func TestNATSOutageDoesNotBlockProjectionAndBacklogRecoversByEffectID(t *testing.T) {
	options := nestwal.DefaultOptions(t.TempDir())
	wal, err := nestwal.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	store := newProjectorOutboxFake()
	projector, err := NewProjector(wal, store, ProjectorOptions{CloseWAL: true, IdlePoll: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer projector.Close(context.Background())
	const records = 100
	for sequence := 1; sequence <= records; sequence++ {
		record := projectorRecord(byte(sequence), true)
		record.Effects[0].ID = "effect-" + record.ID.String()
		if err := projector.Commit(context.Background(), record); err != nil {
			t.Fatal(err)
		}
		projector.TransactionReleased(record.ID)
	}
	if err := projector.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	projected, pending := len(store.records), len(store.pending)
	store.mu.Unlock()
	if projected != records || pending != records || projector.Stats().WALUnacked != 0 {
		t.Fatalf("projected=%d pending=%d stats=%+v", projected, pending, projector.Stats())
	}
	publisher := &recoveringPublisher{down: true, ids: make(map[string]int)}
	worker, _ := NewOutboxWorker(store, publisher, OutboxWorkerOptions{Owner: "worker", BatchSize: 32, RetryMin: time.Nanosecond})
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	publisher.mu.Lock()
	publisher.down = false
	publisher.mu.Unlock()
	for {
		store.mu.Lock()
		remaining := len(store.pending)
		store.mu.Unlock()
		if remaining == 0 {
			break
		}
		if _, err := worker.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if len(publisher.ids) != records {
		t.Fatalf("published unique effects=%d", len(publisher.ids))
	}
	for id, count := range publisher.ids {
		if count != 1 {
			t.Fatalf("effect %s published %d times", id, count)
		}
	}
}

// 设计 T-A（saga JetStream 权威，DESIGN-2026-10-28 §10）：被 kill -9 的进程留下的投影事务在服务端持锁到
// transactionLifetimeLimitSeconds 才回收。重启的启动排空对同一文档的写一直 WriteConflict，在启动期限内重试（驱动在一次
// WithTransaction 里重跑，超过 mongo.transaction_timeout 后由 Runtime 退避再 Flush），遗留事务回收后完成、记录恰好投影一次。
//
// 回收时刻：服务端每 transactionLifetimeLimitSeconds / 2 扫一次过期事务，所以遗留事务最晚在 1.5 倍参数时回收（2026-10-29
// 私有 mongod 实测：参数 20s，排空在遗留事务开始后 28.7s 完成；参数 60s、期限 30s 时启动失败）。本用例的期限取
// max(30s, 1.5 × 参数 + 5s)，钉的是“重试到回收为止”；部署参数与 dataengine.startup_timeout 的关系见 SAGA.md。
//
// 资源：ROOST_DATAENGINE_IT_MONGO_URI 指向的副本集，只用 deleftover_<pid>_<纳秒> 一个库，用后删除；遗留事务由第二个客户端
// 开启后不提交、不结束会话（与被杀进程一样只能等服务端回收）。参数是服务端的，测试只读（GetParameter），不改。
func TestRealMongoStartupDrainOutlastsALeftoverTransaction(t *testing.T) {
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Skip("ROOST_DATAENGINE_IT_MONGO_URI is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	client, err := mongodriver.NewClient(fmongo.DefaultConfig(uri), mongodriver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	database := fmt.Sprintf("deleftover_%d_%d", os.Getpid(), time.Now().UnixNano())
	defer func() {
		_ = client.Database(database).Drop(context.Background())
		_ = client.Close(context.Background())
	}()
	value, err := client.GetParameter(ctx, "transactionLifetimeLimitSeconds")
	if err != nil {
		t.Skipf("transactionLifetimeLimitSeconds is not readable here: %v", err)
	}
	var lifetime time.Duration
	switch v := value.(type) {
	case int32:
		lifetime = time.Duration(v) * time.Second
	case int64:
		lifetime = time.Duration(v) * time.Second
	case float64:
		lifetime = time.Duration(v * float64(time.Second))
	default:
		t.Fatalf("transactionLifetimeLimitSeconds has type %T", value)
	}
	if lifetime > time.Minute {
		t.Skipf("the server's transactionLifetimeLimitSeconds is %v; this test waits up to 1.5x that for the leftover transaction", lifetime)
	}
	startupTimeout := max(30*time.Second, lifetime*3/2+5*time.Second)

	// 集合先存在（线上实体集合早已建好；事务里隐式建集合不与另一事务的写冲突，测不到这里要的场景）。
	if _, err := client.Database(database).Collection("heroes").InsertOne(ctx, bson.M{"_id": int64(1), "_version": uint64(1)}); err != nil {
		t.Fatal(err)
	}
	// 遗留事务：另一个客户端在事务里插入 hero 7，不提交、不结束会话。
	leftover, err := rawmongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = leftover.Disconnect(context.Background()) }()
	session, err := leftover.StartSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.StartTransaction(); err != nil {
		t.Fatal(err)
	}
	if _, err := leftover.Database(database).Collection("heroes").InsertOne(rawmongo.NewSessionContext(ctx, session), bson.M{"_id": int64(7), "_version": uint64(1), "value": int32(-1)}); err != nil {
		t.Fatal(err)
	}
	leftoverAt := time.Now()

	// 重启的进程：WAL 里一条要写 hero 7 的记录（带 effect，走事务路径），Start 在启动期限内排空。
	cfg := AssemblyConfig{
		Mongo: MongoStoreConfig{Databases: identityDatabases(t), DefaultDatabase: database, ServerID: 1},
		WAL:   nestwal.DefaultOptions(t.TempDir()), Projector: DefaultProjectorOptions(),
		Outbox: OutboxWorkerOptions{Owner: "leftover"},
	}
	a, err := Assemble(AssemblyDeps{Mongo: client, JetStream: &recordingJetStream{}, Access: entity.NewManagerAccess(entity.NewEntityManager())}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Shutdown(context.Background()) }()
	wal, err := nestwal.Open(cfg.WAL)
	if err != nil {
		t.Fatal(err)
	}
	var id coredata.TransactionID
	copy(id[:], "leftover-drain-1")
	payload, _ := bson.Marshal(bson.M{"_id": int64(7), "value": int32(7)})
	record := coredata.CommitRecord{
		ID: id, Durability: corenest.DurabilityStrict.Record(),
		Mutations: []coredata.Mutation{{Key: coredata.DocumentKey{Database: database, Resource: "heroes", ID: int64(7)}, Kind: coredata.MutationPut, NextVersion: 1, Data: payload}},
		Effects:   []coredata.Effect{{ID: "leftover-effect", Topic: "hero.changed", Payload: []byte{7}}},
	}
	if _, err := wal.Append(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := wal.Close(ctx); err != nil {
		t.Fatal(err)
	}
	startCtx, cancelStart := context.WithTimeout(ctx, startupTimeout)
	defer cancelStart()
	startErr := a.Start(startCtx)
	elapsed := time.Since(leftoverAt)
	t.Logf("transactionLifetimeLimitSeconds=%v; Start returned after %v (startup timeout %v): %v", lifetime, elapsed.Round(time.Millisecond), startupTimeout, startErr)
	if startErr != nil {
		t.Fatalf("startup drain did not outlast the leftover transaction (server lifetime %v, startup timeout %v): %v", lifetime, startupTimeout, startErr)
	}
	var hero struct {
		Version uint64 `bson:"_version"`
		Value   int32  `bson:"value"`
	}
	if err := client.Database(database).Collection("heroes").FindOne(ctx, bson.M{"_id": int64(7)}, &hero); err != nil || hero.Version != 1 || hero.Value != 7 {
		t.Fatalf("hero 7 after the drain = %+v err=%v; want the WAL record projected once", hero, err)
	}
	if got := a.Runtime().Projector.Stats().Projected; got != 1 {
		t.Fatalf("projected %d records, want 1", got)
	}
}
