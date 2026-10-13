// 装配与运行时生命周期：缺依赖拒绝、并发启动共享运行时、停机等启动中的拥有者、取消恢复保留清理所有权、重试停机交接 WAL。

package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
)

// Assemble 是 kit Mod 的唯一入口：缺依赖在 Provide 时点名拒绝，而不是等到 Start 时 nil 解引用；远端投影的两个依赖要么都给要么都不给，
// 给了的 manager 必须能应用远端提交。outbox 发布器对不完整的效果拒绝（否则 JetStream 收到一条没有去重键的消息）。

type recordingJetStream struct{ published []string }

func (*recordingJetStream) EnsureStream(context.Context, fnats.JetStreamConfig) error { return nil }
func (s *recordingJetStream) Publish(_ context.Context, subject string, _ []byte, opts fnats.JetStreamPublishOptions) (fnats.JetStreamPublishAck, error) {
	s.published = append(s.published, subject+"|"+opts.MsgID)
	return fnats.JetStreamPublishAck{}, nil
}
func (*recordingJetStream) Subscribe(context.Context, fnats.JetStreamConsumerConfig, fnats.JetStreamHandler) (fnats.IJetStreamSubscription, error) {
	return nil, errors.New("unused")
}

// remoteManagerWithoutApplier satisfies entity.IRemoteEntityManager (every method
// panics if called) but is not an entity.RemoteCommitApplier.
type remoteManagerWithoutApplier struct{ entity.IRemoteEntityManager }

type remoteManagerWithApplier struct{ entity.IRemoteEntityManager }

func (remoteManagerWithApplier) ApplyRemoteCommits(context.Context, entity.RemoteTransactionID, []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error) {
	return nil, nil
}

type remoteProjectionStoreStub struct{}

func (remoteProjectionStoreStub) ApplyRemoteCommitsInTransaction(context.Context, []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error) {
	return nil, nil
}

func TestAssembleRefusesEachMissingDependency(t *testing.T) {
	access := entity.NewManagerAccess(entity.NewEntityManager())
	full := AssemblyDeps{Mongo: mongotest.NewClient(), JetStream: &recordingJetStream{}, Access: access}
	cfg := AssemblyConfig{Mongo: MongoStoreConfig{DefaultDatabase: "game", ServerID: 1}}
	if asm, err := Assemble(full, cfg); err != nil || asm == nil || asm.Store == nil {
		t.Fatalf("baseline Assemble = (%v, %v)", asm, err)
	}
	cases := []struct {
		name string
		deps AssemblyDeps
		text string
	}{
		{"no entity access", AssemblyDeps{Mongo: full.Mongo, JetStream: full.JetStream}, "entity access is required"},
		{"access without manager", AssemblyDeps{Mongo: full.Mongo, JetStream: full.JetStream, Access: &entity.ManagerAccess{}}, "entity access is required"},
		{"no mongo", AssemblyDeps{JetStream: full.JetStream, Access: access}, "mongo client is required"},
		{"no jetstream", AssemblyDeps{Mongo: full.Mongo, Access: access}, "jetstream client is required"},
		{"remote store without manager", AssemblyDeps{Mongo: full.Mongo, JetStream: full.JetStream, Access: access, RemoteStore: remoteProjectionStoreStub{}}, "remote projection needs both"},
		{"remote manager without store", AssemblyDeps{Mongo: full.Mongo, JetStream: full.JetStream, Access: access, RemoteManager: remoteManagerWithApplier{}}, "remote projection needs both"},
		{"remote manager without applier", AssemblyDeps{Mongo: full.Mongo, JetStream: full.JetStream, Access: access, RemoteManager: remoteManagerWithoutApplier{}, RemoteStore: remoteProjectionStoreStub{}}, "no commit applier"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			asm, err := Assemble(tc.deps, cfg)
			if err == nil || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("Assemble = (%v, %v), want error containing %q", asm, err, tc.text)
			}
		})
	}
	// 成对给出且 manager 能应用远端提交：装配成功。
	remote := full
	remote.RemoteManager, remote.RemoteStore = remoteManagerWithApplier{}, remoteProjectionStoreStub{}
	if _, err := Assemble(remote, cfg); err != nil {
		t.Fatalf("Assemble with remote projection = %v", err)
	}

	var none *Assembly
	if err := none.Start(context.Background()); err == nil {
		t.Fatal("nil assembly Start returned no error")
	}
	if err := (&Assembly{}).Start(context.Background()); err == nil || !strings.Contains(err.Error(), "not assembled") {
		t.Fatalf("empty assembly Start = %v", err)
	}
	if none.Runtime() != nil || none.Shutdown(context.Background()) != nil {
		t.Fatal("nil assembly must report no runtime and shut down silently")
	}
}

func TestJetStreamOutboxPublisherRefusesIncompleteEffects(t *testing.T) {
	ctx := context.Background()
	js := &recordingJetStream{}
	publisher := &jetStreamOutboxPublisher{client: js, prefix: "roost.effect"}
	good := OutboxItem{TransactionID: "tx-1"}
	good.Effect.ID, good.Effect.Topic, good.Effect.Payload = "effect-1", "hero.changed", []byte("p")
	if err := publisher.Publish(ctx, good); err != nil {
		t.Fatalf("baseline Publish = %v", err)
	}
	if len(js.published) != 1 || js.published[0] != "roost.effect.hero.changed|effect-1" {
		t.Fatalf("published = %v, want subject under the prefix and the effect id as MsgID", js.published)
	}

	noID := good
	noID.Effect.ID = ""
	noTopic := good
	noTopic.Effect.Topic = ""
	cases := []struct {
		name      string
		publisher *jetStreamOutboxPublisher
		item      OutboxItem
	}{
		{"nil publisher", nil, good},
		{"publisher without client", &jetStreamOutboxPublisher{prefix: "roost.effect"}, good},
		{"effect without id", publisher, noID},
		{"effect without topic", publisher, noTopic},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(js.published)
			if err := tc.publisher.Publish(ctx, tc.item); err == nil || !strings.Contains(err.Error(), "invalid JetStream publish") {
				t.Fatalf("Publish = %v, want invalid-publish error", err)
			}
			if len(js.published) != before {
				t.Fatal("an invalid effect reached JetStream")
			}
		})
	}
}

// 外部 Flush 卡在存储里时重试停机，新拥有者的 checkpoint 不回退（正式 Assembly：mongotest + 真实 nestwal + MongoStore）：
//   - 重试的 Shutdown 在外部投影退出前不返回、不丢 runtime、不交出 WAL 目录（新拥有者此时打不开同一目录）；
//   - 外部投影放行后 Shutdown 返回 nil，外部投影的 ack 在 WAL 交出前完成；
//   - 新拥有者（同配置重新 Start）启动恢复不重放旧拥有者已确认的记录；新拥有者自己确认的记录在下一次启动也不重放。
//
// 历史：RR-20260926-17。
func TestAssemblyRetriedShutdownHandsOverWALWithoutCheckpointRegression(t *testing.T) {
	mongo := &gatedTransactionMongo{Client: mongotest.NewClient(), entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(mongo.open)
	cfg := AssemblyConfig{Mongo: MongoStoreConfig{DefaultDatabase: "game", ServerID: 1}}
	cfg.WAL = nestwal.DefaultOptions(t.TempDir())
	cfg.WAL.GroupCommitInterval = time.Millisecond
	cfg.Projector = ProjectorOptions{ManualReplay: true, CloseWAL: true}
	cfg.Outbox = OutboxWorkerOptions{Owner: "b10"}
	start := func() *Assembly {
		t.Helper()
		asm, err := Assemble(AssemblyDeps{Mongo: mongo, JetStream: &recordingJetStream{}, Access: entity.NewManagerAccess(entity.NewEntityManager())}, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := asm.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		return asm
	}
	// 两个 mutation 的本地记录走 Mongo 事务（单 mutation 快路不开事务，停不到闸门上）。
	asyncRecord := func(sequence byte) coredata.CommitRecord {
		record := localMultiRecord(sequence)
		record.Durability = corenest.DurabilityAsync.Record()
		return record
	}

	old := start()
	rt := old.Runtime()
	commit := func(p *Projector, record coredata.CommitRecord) {
		t.Helper()
		if err := p.Commit(context.Background(), record); err != nil {
			t.Fatal(err)
		}
		// Commit 在实体锁内准入（held）；Nest 释放锁后通知，投影才会越过它。
		p.TransactionReleased(record.ID)
	}
	commit(rt.Projector, asyncRecord(1))
	mongo.arm()
	external := make(chan error, 1)
	go func() { external <- rt.Flush(context.Background()) }()
	awaitChan(t, mongo.entered, "the external flush to enter the store")

	bounded, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := old.Shutdown(bounded); err == nil {
		t.Fatal("bounded Shutdown returned nil while an external projection is inside the store")
	}
	if old.Runtime() != rt {
		t.Fatal("Assembly dropped the runtime after an incomplete shutdown")
	}
	retried := make(chan error, 1)
	go func() { retried <- old.Shutdown(context.Background()) }()

	// 外部投影仍在存储里：WAL 目录不能交出。
	if w, err := nestwal.Open(cfg.WAL); err == nil {
		_ = w.Close(context.Background())
		t.Fatal("a new owner opened the WAL directory while the old runtime's external projection was still running")
	}
	select {
	case err := <-retried:
		t.Fatalf("retried Shutdown returned %v before the external projection left the store", err)
	case err := <-external:
		t.Fatalf("external flush returned %v while held in the store", err)
	default:
	}

	mongo.open()
	// 停机已开始：外部 Flush 投影完已进入的这一轮后可以报 ErrRuntimeStopped，但已投影前缀必须在交出 WAL 前确认（下文新拥有者核对）。
	if err := <-external; err != nil && !errors.Is(err, ErrRuntimeStopped) {
		t.Fatalf("external flush: %v", err)
	}
	if err := <-retried; err != nil {
		t.Fatalf("retried Shutdown = %v, want nil once the external projection returned", err)
	}
	if old.Runtime() != nil {
		t.Fatal("Assembly kept the runtime after a completed shutdown")
	}

	// 新拥有者：启动恢复不重放旧拥有者已确认的记录 1；自己提交并确认记录 2。
	owner := start()
	if got := owner.Runtime().Projector.Stats().Projected; got != 0 {
		t.Fatalf("new owner replayed %d record(s) the old owner had acknowledged: checkpoint regressed", got)
	}
	commit(owner.Runtime().Projector, asyncRecord(2))
	if err := owner.Runtime().Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := owner.Runtime().Projector.Stats(); got.Projected != 1 || got.WALUnacked != 0 {
		t.Fatalf("new owner stats=%+v, want record 2 projected and acknowledged", got)
	}
	if err := owner.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	next := start()
	defer func() { _ = next.Shutdown(context.Background()) }()
	if got := next.Runtime().Projector.Stats().Projected; got != 0 {
		t.Fatalf("restart after the new owner replayed %d record(s): the new owner's checkpoint regressed", got)
	}
}

// gatedTransactionMongo 在 arm 之后让第一次 Mongo 事务停在入口，直到 open；停住期间不理会 ctx 取消，
// 模拟不遵守取消的存储调用（外部 Flush 的截止由调用方决定，停机不能靠取消它来交出 WAL）。
type gatedTransactionMongo struct {
	*mongotest.Client
	mu      sync.Mutex
	armed   bool
	entered chan struct{}
	release chan struct{}
	opened  sync.Once
}

func (m *gatedTransactionMongo) arm() {
	m.mu.Lock()
	m.armed = true
	m.mu.Unlock()
}

func (m *gatedTransactionMongo) open() { m.opened.Do(func() { close(m.release) }) }

func (m *gatedTransactionMongo) StartSession(ctx context.Context) (fmongo.ISession, error) {
	session, err := m.Client.StartSession(ctx)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	gate := m.armed
	m.armed = false
	m.mu.Unlock()
	if !gate {
		return session, nil
	}
	return &gatedSession{ISession: session, mongo: m}, nil
}

type gatedSession struct {
	fmongo.ISession
	mongo *gatedTransactionMongo
}

func (s *gatedSession) WithTransaction(ctx context.Context, fn func(context.Context) error) error {
	close(s.mongo.entered)
	<-s.mongo.release
	return s.ISession.WithTransaction(ctx, fn)
}

// slowClaimOutboxStore parks the outbox worker inside its first Claim until
// the test releases it, so a bounded shutdown context runs out while the
// worker is still active.
type slowClaimOutboxStore struct {
	*projectorOutboxFake
	entered chan struct{}
	release chan struct{}
}

func (s *slowClaimOutboxStore) Claim(ctx context.Context, _ string, _ time.Time, _ int, _ time.Duration) ([]OutboxItem, error) {
	close(s.entered)
	<-s.release
	return nil, ctx.Err()
}

// Shutdown 的 context 先到期、worker 还没退出时，Assembly 不忘掉 runtime：重试继续等同一批组件，等到了才算停下、才释放引用。
// 历史：RR-20260909-03。
func TestAssemblyKeepsTheRuntimeUntilShutdownCompletes(t *testing.T) {
	store := &slowClaimOutboxStore{projectorOutboxFake: newProjectorOutboxFake(), entered: make(chan struct{}), release: make(chan struct{})}
	worker, err := NewOutboxWorker(store, failingOutboxPublisher{}, OutboxWorkerOptions{Owner: "review"})
	if err != nil {
		t.Fatal(err)
	}
	worker.Start(context.Background())
	<-store.entered
	released := false
	defer func() {
		if !released {
			close(store.release)
		}
		if err := worker.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()

	runtime := &Runtime{Outbox: worker}
	assembly := &Assembly{runtime: runtime}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := assembly.Shutdown(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("first Shutdown with a canceled context = %v, want context.Canceled", err)
	}
	select {
	case <-worker.done:
		t.Fatal("the worker exited before the test released it; the scenario did not hold")
	default:
	}
	if assembly.Runtime() != runtime {
		t.Fatalf("Runtime() = %v after an incomplete shutdown, want the still-running runtime", assembly.Runtime())
	}

	retryCtx, retryCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer retryCancel()
	if err := assembly.Shutdown(retryCtx); err == nil {
		t.Fatalf("second Shutdown answered success while the outbox worker is still active; Runtime()=%v", assembly.Runtime())
	}
	if assembly.Runtime() != runtime {
		t.Fatal("lost runtime ownership after a second incomplete shutdown")
	}

	// Once the worker can exit, a retry completes and only then forgets the runtime.
	close(store.release)
	released = true
	if err := assembly.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown after the worker was released = %v, want nil", err)
	}
	select {
	case <-worker.done:
	default:
		t.Fatal("Shutdown returned nil while the worker had not exited")
	}
	if assembly.Runtime() != nil {
		t.Fatalf("Runtime() = %v after a completed shutdown, want nil", assembly.Runtime())
	}
	if err := assembly.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown after completion = %v, want nil (idempotent)", err)
	}
}

func lifecycleAssembly(t *testing.T, client fmongo.IMongo, js fnats.IJetStream) *Assembly {
	t.Helper()
	cfg := AssemblyConfig{
		Mongo: MongoStoreConfig{DefaultDatabase: "game", ServerID: 1},
		WAL:   nestwal.DefaultOptions(t.TempDir()), Projector: DefaultProjectorOptions(),
		Outbox: OutboxWorkerOptions{Owner: "lifecycle"},
	}
	a, err := Assemble(AssemblyDeps{Mongo: client, JetStream: js, Access: entity.NewManagerAccess(entity.NewEntityManager())}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return a
}

func TestRuntimeRepeatedStartPreservesReadyAndOutbox(t *testing.T) {
	a := lifecycleAssembly(t, mongotest.NewClient(), &recordingJetStream{})
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := a.Runtime()
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !r.Ready() {
		t.Fatal("repeated Start dismantled ready runtime")
	}
	select {
	case <-r.Outbox.done:
		t.Fatal("repeated Start closed outbox")
	default:
	}
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background()); err == nil {
		t.Fatal("stopped runtime became ready again")
	}
}

func TestAssemblyConcurrentStartsShareRuntime(t *testing.T) {
	a := lifecycleAssembly(t, mongotest.NewClient(), &recordingJetStream{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := a.Start(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if a.Runtime() == nil || !a.Runtime().Ready() {
		t.Fatal("runtime is not ready")
	}
}

type parkedStream struct {
	recordingJetStream
	entered, release chan struct{}
}

func (s *parkedStream) EnsureStream(ctx context.Context, _ fnats.JetStreamConfig) error {
	close(s.entered)
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func TestAssemblyShutdownWaitsForStartingOwner(t *testing.T) {
	js := &parkedStream{entered: make(chan struct{}), release: make(chan struct{})}
	a := lifecycleAssembly(t, mongotest.NewClient(), js)
	started := make(chan error, 1)
	go func() { started <- a.Start(context.Background()) }()
	<-js.entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := a.Shutdown(ctx)
	cancel()
	close(js.release)
	if startErr := <-started; startErr != nil {
		t.Fatal(startErr)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown while starting=%v, want deadline", err)
	}
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.Runtime() != nil {
		t.Fatal("shutdown did not release runtime")
	}
}

type cancelProjectionMongo struct {
	fmongo.IMongo
	cancel context.CancelFunc
}

func (c cancelProjectionMongo) Database(name string) fmongo.IDatabase {
	return cancelProjectionDB{c.IMongo.Database(name), c.cancel}
}

type cancelProjectionDB struct {
	fmongo.IDatabase
	cancel context.CancelFunc
}

func (d cancelProjectionDB) Collection(name string) fmongo.ICollection {
	coll := d.IDatabase.Collection(name)
	if name == "heroes" {
		return cancelProjectionCollection{coll, d.cancel}
	}
	return coll
}

type cancelProjectionCollection struct {
	fmongo.ICollection
	cancel context.CancelFunc
}

func (c cancelProjectionCollection) FindOneAndUpdate(context.Context, any, any, any, ...fmongo.FindOneAndUpdateOption) error {
	c.cancel()
	return context.Canceled
}
func TestAssemblyCanceledRecoveryRetainsCleanupOwnership(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := lifecycleAssembly(t, cancelProjectionMongo{mongotest.NewClient(), cancel}, &recordingJetStream{})
	wal, err := nestwal.Open(a.cfg.WAL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wal.Append(context.Background(), projectorRecord(17, false)); err != nil {
		t.Fatal(err)
	}
	if err := wal.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Start=%v", err)
	}
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, err := nestwal.Open(a.cfg.WAL)
	if err != nil {
		t.Fatalf("failed startup leaked WAL: %v", err)
	}
	defer reopened.Close(context.Background())
	assertWALReplayCount(t, reopened, 1)
}
func TestAssemblyOwnsWALWhenProjectorDoesNot(t *testing.T) {
	a := lifecycleAssembly(t, mongotest.NewClient(), &recordingJetStream{})
	a.cfg.Projector.CloseWAL = false
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, err := nestwal.Open(a.cfg.WAL)
	if err != nil {
		t.Fatalf("assembly leaked its WAL: %v", err)
	}
	reopened.Close(context.Background())
}

func TestAssemblyFailedConstructionCanRetry(t *testing.T) {
	for _, stage := range []string{"projector", "outbox"} {
		t.Run(stage, func(t *testing.T) {
			a := lifecycleAssembly(t, mongotest.NewClient(), &recordingJetStream{})
			good := a.cfg
			if stage == "projector" {
				a.cfg.Projector.RetryMin = time.Second
				a.cfg.Projector.RetryMax = time.Millisecond
			} else {
				a.cfg.Outbox.Owner = ""
			}
			if err := a.Start(context.Background()); err == nil {
				t.Fatal("invalid construction succeeded")
			}
			if a.Runtime() != nil {
				t.Fatal("completed failed-start cleanup retained runtime")
			}
			a.cfg = good
			if err := a.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAssembleValidatesReceiptRetentionBeforeInfrastructure(t *testing.T) {
	deps := AssemblyDeps{Mongo: mongotest.NewClient(), JetStream: &recordingJetStream{}, Access: entity.NewManagerAccess(entity.NewEntityManager())}
	cfg := AssemblyConfig{Mongo: MongoStoreConfig{DefaultDatabase: "test", TransactionReceiptTTL: 48 * time.Hour}, WAL: nestwal.Options{MaxUnackedAge: 48 * time.Hour}}
	if _, err := Assemble(deps, cfg); err == nil || !strings.Contains(err.Error(), "transaction_receipt_ttl") {
		t.Fatalf("unsafe direct assembly = %v", err)
	}
	cfg.Mongo.TransactionReceiptTTL += time.Second
	if _, err := Assemble(deps, cfg); err != nil {
		t.Fatalf("valid direct assembly = %v", err)
	}
	cfg.Mongo.TransactionReceiptTTL = 0
	cfg.WAL.MaxUnackedAge = 0
	if _, err := Assemble(deps, cfg); err != nil {
		t.Fatalf("default retention = %v", err)
	}
}
