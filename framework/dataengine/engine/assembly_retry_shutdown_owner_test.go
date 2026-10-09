package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
)

// OPEN-ITEMS B10：RR-20260926-17 的 Runtime / Assembly 级场景——外部 Flush 卡在存储里时重试停机，新拥有者的 checkpoint 不回退。
//
// 正式 Assembly（mongotest + 真实 nestwal + MongoStore）：外部 `Runtime.Flush(Background)` 停在 Mongo 事务入口（存储不理会取消），
// 并发 `Assembly.Shutdown(50ms)` 超时后按契约重试 `Shutdown(Background)`。承诺：
//   - 重试的 Shutdown 在外部投影退出前不返回、不丢 runtime、不交出 WAL 目录（新拥有者此时打不开同一目录）；
//   - 外部投影放行后 Shutdown 返回 nil，外部投影的 ack 在 WAL 交出前完成；
//   - 新拥有者（同配置重新 Start）启动恢复不重放旧拥有者已确认的记录；新拥有者自己确认的记录在下一次启动也不重放。
//
// 修前（RR-17 之前）Projector.Close 不等外部 Flush，重试的 Shutdown 返回 nil 后旧回放才写 checkpoint，覆盖新拥有者的 fence。
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
