package engine

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
)

type heldProjectionStore struct{ entered, release chan struct{} }

func (s *heldProjectionStore) Project(context.Context, coredata.CommitRecord) error {
	close(s.entered)
	<-s.release
	return nil
}
func TestProjectorCloseWaitsForExternalProjection(t *testing.T) {
	store := &heldProjectionStore{make(chan struct{}), make(chan struct{})}
	p, w := stoppedProjectorWithRecords(t, store, []coredata.CommitRecord{localMultiRecord(1)}, 4<<20)
	p.opts.CloseWAL = true
	done := make(chan error, 1)
	go func() { _, err := p.ReplayPass(context.Background()); done <- err }()
	<-store.entered
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Close(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("close=%v", err)
	}
	if _, err := p.ReplayPass(context.Background()); !errors.Is(err, ErrRuntimeStopped) {
		t.Fatalf("new replay=%v", err)
	}
	// WAL cannot be closed while an admitted store call still owns checkpoint responsibility.
	if _, err := w.Append(context.Background(), localMultiRecord(2)); err != nil {
		t.Fatalf("WAL prematurely closed: %v", err)
	}
	close(store.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := p.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append(context.Background(), localMultiRecord(3)); !errors.Is(err, nestwal.ErrClosed) {
		t.Fatalf("WAL remains open: %v", err)
	}
}

// manualProjector 返回运行中（ctx 未取消）但不启动后台循环的 Projector。
// 等待屏障的测试必须用它：已 cancel 的 projector 让 WaitEntityProjection 直接返回
// ErrRuntimeStopped，证明不了“会等待”（RR-20260926-10 复核残留）。
func manualProjector(t *testing.T, store ProjectionStore) (*Projector, *nestwal.WAL) {
	t.Helper()
	options := nestwal.DefaultOptions(t.TempDir())
	options.WriterVersion = nestwal.WriterVersionV2
	w, err := nestwal.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close(context.Background()) })
	p, err := NewProjector(w, store, ProjectorOptions{ReplayBatchRecords: 1, CloseWAL: false, ManualReplay: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	return p, w
}

func waitEntityAsync(p *Projector, ctx context.Context, id int64) <-chan error {
	result := make(chan error, 1)
	go func() { result <- p.WaitEntityProjection(ctx, id) }()
	return result
}

// waitEntityInSelect 在等待方已取走待投影项、进入 select 之后才返回：select 阻塞前
// 会求值 ctx.Done()，这一刻关闭 entered。随后发生的事件一定是在“等待中”被观察到的。
func waitEntityInSelect(t *testing.T, p *Projector, ctx context.Context, id int64) <-chan error {
	t.Helper()
	barrier := &selectEntryContext{Context: ctx, entered: make(chan struct{})}
	result := waitEntityAsync(p, barrier, id)
	awaitChan(t, barrier.entered, "the waiter to enter its select")
	return result
}

type selectEntryContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *selectEntryContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

// RR-20260926-10：冷加载前等待该实体全部 DAO 的在途投影。断言落在“确实在等”：
// 运行中的 projector 上，截止时间内投影未完成就返回截止错误；投影成功后释放；
// Close 唤醒仍在等待的调用方。
func TestEntityProjectionBarrierTracksAllDAOsAndReleasesOnFailure(t *testing.T) {
	p, _ := manualProjector(t, &multiSegmentStore{})
	record := localMultiRecord(1)
	record.Mutations[1].Key.ID = 999
	ids := []int64{record.Mutations[0].Key.ID, 999}
	if err := p.Commit(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, id := range ids {
		if err := p.WaitEntityProjection(canceled, id); !errors.Is(err, context.Canceled) {
			t.Fatalf("entity %d: wait with canceled ctx=%v, want context.Canceled", id, err)
		}
		short, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
		err := p.WaitEntityProjection(short, id)
		stop()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("entity %d: live projector did not wait for pending projection: %v", id, err)
		}
	}
	if err := p.WaitEntityProjection(context.Background(), 12345); err != nil {
		t.Fatalf("unrelated entity waited: %v", err)
	}
	waiters := []<-chan error{waitEntityInSelect(t, p, context.Background(), ids[0]), waitEntityInSelect(t, p, context.Background(), ids[1])}
	p.TransactionReleased(record.ID)
	if _, err := p.ReplayPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, waiter := range waiters {
		if err := awaitChan(t, waiter, "the waiter to observe the projection"); err != nil {
			t.Fatal(err)
		}
	}
	if len(p.pendingEntities) != 0 || len(p.pendingTransactions) != 0 {
		t.Fatal("projection barrier leaked")
	}

	// 关闭释放：投影永远不会发生的等待方由 Close 唤醒，不能一直占着慢 worker。
	next := localMultiRecord(2)
	if err := p.Commit(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	waiter := waitEntityInSelect(t, p, context.Background(), next.Mutations[0].Key.ID)
	if err := p.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := awaitChan(t, waiter, "Close to release the waiter"); !errors.Is(err, ErrRuntimeStopped) && !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter after close=%v", err)
	}
	// 关闭后才开始的等待：记录未投影且不会再被本进程投影，不能返回 nil 放行冷读。
	if err := p.WaitEntityProjection(context.Background(), next.Mutations[0].Key.ID); !errors.Is(err, ErrRuntimeStopped) {
		t.Fatalf("wait after close=%v, want ErrRuntimeStopped", err)
	}
}

type fatalRecordStore struct{ failID coredata.TransactionID }

func (s *fatalRecordStore) Project(_ context.Context, r coredata.CommitRecord) error {
	if r.ID == s.failID {
		return ErrProjectionConflict
	}
	return nil
}

// RR-20260926-17 复核残留：自定义 OnFatal 里同步 Close 不能自等。旧实现在 ReplayPass/Flush
// 仍持有 operation（后台循环还持有自己的 done）时同步回调 OnFatal，Close 等 drained/done
// 直到截止时间。现在 OnFatal 异步投递；fatal 在回调之前已对准入与等待方可见。
func TestProjectorOnFatalMayCloseProjector(t *testing.T) {
	for _, background := range []bool{false, true} {
		name := "external_flush"
		if background {
			name = "background_loop"
		}
		t.Run(name, func(t *testing.T) {
			options := nestwal.DefaultOptions(t.TempDir())
			options.WriterVersion = nestwal.WriterVersionV2
			w, err := nestwal.Open(options)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = w.Close(context.Background()) })
			a := projectorRecord(1, false)
			var p *Projector
			closed := make(chan error, 1)
			visible := make(chan error, 1)
			onFatal := func(error) {
				// 回调开始时 fatal 已可见：准入在 WAL 前拒绝。
				visible <- p.Commit(context.Background(), projectorRecord(9, false))
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				closed <- p.Close(ctx)
			}
			p, err = NewProjector(w, &fatalRecordStore{failID: a.ID}, ProjectorOptions{
				CloseWAL: false, IdlePoll: time.Hour, RetryMin: time.Hour, RetryMax: time.Hour,
				ManualReplay: !background, OnFatal: onFatal,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Commit(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			p.TransactionReleased(a.ID)
			if !background {
				if err := p.Flush(context.Background()); !errors.Is(err, ErrProjectionConflict) {
					t.Fatalf("flush=%v", err)
				}
			}
			if err := awaitChan(t, visible, "OnFatal to run"); !errors.Is(err, ErrProjectionConflict) {
				t.Fatalf("admission inside OnFatal=%v, want the fatal verdict", err)
			}
			if err := awaitChan(t, closed, "Close inside OnFatal"); err != nil {
				t.Fatalf("Close inside OnFatal=%v", err)
			}
			if _, err := p.ReplayPass(context.Background()); !errors.Is(err, ErrRuntimeStopped) {
				t.Fatalf("replay after close=%v", err)
			}
		})
	}
}

// RR-20260926-10 复核残留：投影 fatal 之后，本进程再不会投影 fatal 记录及其后的任何记录。
// 所有待投影实体的等待方（包括 fatal 批次之外的实体）都必须立即拿到可判别的 fatal，
// 不能等到各自截止时间；fatal 之后才开始的等待同样如此。旧实现只唤醒 fatal 批次内的记录。
func TestEntityProjectionFatalWakesEveryPendingWaiter(t *testing.T) {
	a, b := projectorRecord(1, false), projectorRecord(2, false)
	p, _ := manualProjector(t, &fatalRecordStore{failID: a.ID})
	for _, r := range []coredata.CommitRecord{a, b} {
		if err := p.Commit(context.Background(), r); err != nil {
			t.Fatal(err)
		}
		p.TransactionReleased(r.ID)
	}
	system := projectorRecord(3, false)
	ticket, err := p.CommitSystem(context.Background(), system)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	during := waitEntityInSelect(t, p, ctx, 2)
	// ReplayBatchRecords=1：这一轮只读到 a，b 与 system 不在 fatal 批次里。
	if _, err := p.ReplayPass(context.Background()); !errors.Is(err, ErrProjectionConflict) {
		t.Fatalf("replay=%v", err)
	}
	if err := awaitChan(t, during, "the waiter outside the fatal batch"); !errors.Is(err, ErrProjectionConflict) {
		t.Fatalf("waiter for entity outside the fatal batch=%v, want ErrProjectionConflict", err)
	}
	for _, id := range []int64{1, 2, 3, 777} {
		if err := p.WaitEntityProjection(ctx, id); !errors.Is(err, ErrProjectionConflict) {
			t.Fatalf("wait after fatal entity=%d: %v", id, err)
		}
	}
	select {
	case <-ticket.Done():
		if !errors.Is(ticket.Err(), ErrProjectionConflict) {
			t.Fatalf("system ticket=%v", ticket.Err())
		}
	case <-ctx.Done():
		t.Fatal("system ticket after the fatal batch was not released")
	}
	if len(p.pendingEntities) != 0 || len(p.pendingTransactions) != 0 {
		t.Fatalf("pending after fatal entities=%d transactions=%d", len(p.pendingEntities), len(p.pendingTransactions))
	}
	if err := p.Commit(context.Background(), projectorRecord(4, false)); !errors.Is(err, ErrProjectionConflict) {
		t.Fatalf("admission after fatal=%v", err)
	}
}

type failedCommitTicket struct {
	done chan struct{}
	err  error
}

func (ticket failedCommitTicket) LSN() uint64           { return 0 }
func (ticket failedCommitTicket) Done() <-chan struct{} { return ticket.done }
func (ticket failedCommitTicket) Err() error            { return ticket.err }

// RR-20260926-10 复核残留：pipelined 记录的 WAL 票据失败（写入结果未知，WAL terminal）后，
// 本进程不会投影它；该实体的等待方要立即拿到 WAL 错误。真实 WAL 没有可从本包注入 fsync
// 失败的缝，这里降到内部入口：登记在途记录后直接把失败票据交给 signalWhenDurable。
func TestEntityProjectionWaiterSeesFailedPipelinedDurability(t *testing.T) {
	p, _ := manualProjector(t, &multiSegmentStore{})
	record := projectorRecord(5, false)
	if err := p.reserve(record, true); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	waiter := waitEntityInSelect(t, p, ctx, 5)
	walErr := errors.New("injected WAL fsync failure")
	ticket := failedCommitTicket{done: make(chan struct{}), err: walErr}
	close(ticket.done)
	p.signalWhenDurable(record.ID, ticket)
	if err := awaitChan(t, waiter, "the waiter of a record whose WAL write failed"); !errors.Is(err, walErr) {
		t.Fatalf("waiter=%v, want the WAL ticket error", err)
	}
}

// RR-20260926-19：同一事务里的 Remote 写与 lease-fence receipt 必须在写 WAL 前被拒绝，
// 三个准入入口都要拒。复核残留：旧版本用空 RemoteCommit 与空 receipt，修前就在 WAL
// 规范化阶段因 invalid identity / invalid receipt 被拒，构不成负对照。这里用 WAL 会接受的
// 合法 Remote 写与合法 lease-fence receipt（先用 ValidateCommitRecord 证明），修前三个
// 入口都准入并写进 WAL；每个入口用独立事务，逐个报告。
func TestRemoteLeaseFenceRejectedBeforeWALAdmission(t *testing.T) {
	p, w := stoppedProjectorWithRecords(t, &multiSegmentStore{}, nil, 4<<20)
	fence, err := coredata.NewLeaseFenceReceipt(coredata.LeaseFence{
		Database: "saga", Resource: "saga_steps", DocumentID: "step-1", Owner: "sid-1", Token: 7, Digest: make([]byte, sha256.Size),
	})
	if err != nil {
		t.Fatal(err)
	}
	admissions := []struct {
		name  string
		admit func(coredata.CommitRecord) error
	}{
		{"Commit", func(record coredata.CommitRecord) error { return p.Commit(context.Background(), record) }},
		{"Enqueue", func(record coredata.CommitRecord) error {
			_, err := p.Enqueue(context.Background(), record)
			return err
		}},
		{"CommitSystem", func(record coredata.CommitRecord) error {
			_, err := p.CommitSystem(context.Background(), record)
			return err
		}},
	}
	for i, admission := range admissions {
		record := remoteProjectionRecord(t, byte(i+1))
		record.Receipts = []coredata.Receipt{fence}
		if err := coredata.ValidateCommitRecord(record); err != nil {
			t.Fatalf("fixture is not a record the WAL would accept, so it proves nothing: %v", err)
		}
		if err := admission.admit(record); !errors.Is(err, ErrRemoteLeaseFenceUnsupported) {
			t.Errorf("%s admitted a remote write that carries a lease fence: %v", admission.name, err)
		}
	}
	assertWALReplayCount(t, w, 0)
	if p.Stats().WALUnacked != 0 || len(p.tickets) != 0 {
		t.Fatal("rejected admission leaked state")
	}
}

// RR-20260926-29：外部夹具需要“不启动后台循环”的 Projector 来逐步驱动回放。
// ManualReplay 下没有后台循环（done 一开始就关闭），记录只由显式 ReplayPass 投影；
// Close 之后仍按 RR-17 拒绝 ReplayPass / Flush。
func TestManualReplayProjectorRunsOnlyExplicitPasses(t *testing.T) {
	options := nestwal.DefaultOptions(t.TempDir())
	options.WriterVersion = nestwal.WriterVersionV2
	w, err := nestwal.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close(context.Background()) })
	store := &recordingSegmentStore{}
	p, err := NewProjector(w, store, ProjectorOptions{CloseWAL: false, ManualReplay: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
	default:
		t.Fatal("manual projector started a background replay loop")
	}
	record := projectorRecord(1, false)
	if err := p.Commit(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	p.TransactionReleased(record.ID)
	if len(store.events) != 0 {
		t.Fatalf("projected without an explicit pass: %v", store.events)
	}
	if n, err := p.ReplayPass(context.Background()); n != 1 || err != nil {
		t.Fatalf("manual pass n=%d err=%v", n, err)
	}
	assertWALReplayCount(t, w, 0)
	if err := p.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ReplayPass(context.Background()); !errors.Is(err, ErrRuntimeStopped) {
		t.Fatalf("replay after close=%v", err)
	}
	if err := p.Flush(context.Background()); !errors.Is(err, ErrRuntimeStopped) {
		t.Fatalf("flush after close=%v", err)
	}
}

// tombstoneRepositoryStore 把投影落到 repositoryStore：删除写墓碑，其余忽略。
type tombstoneRepositoryStore struct{ docs *repositoryStore }

func (s *tombstoneRepositoryStore) Project(_ context.Context, record coredata.CommitRecord) error {
	s.docs.mu.Lock()
	defer s.docs.mu.Unlock()
	for _, mutation := range record.Mutations {
		if mutation.Kind == coredata.MutationDelete {
			s.docs.docs[mutation.Key.Resource] = []coredata.RawDocument{{Key: mutation.Key, Version: mutation.NextVersion, Deleted: true}}
		}
	}
	return nil
}

type projectorRecoveryGate struct{ *Projector }

func (projectorRecoveryGate) Ready() bool { return true }

// RR-20260926-10：本地删档只把 tombstone 写进 WAL。投影前重新加载必须等待，不能从旧库
// 读出活实体（“复活”）；投影后加载得到 not found，且不发布到 Manager。
func TestRepositoryReloadDoesNotResurrectPendingTombstone(t *testing.T) {
	ensureDataEngineRepositoryEntity()
	id, _ := entity.BuildEntityID(994, dataEngineRepositoryKind)
	docs := &repositoryStore{docs: map[string][]coredata.RawDocument{
		"repository_profile":   {repositoryRaw(t, "repository_profile", id, 1)},
		"repository_inventory": {repositoryRaw(t, "repository_inventory", id, 1)},
	}}
	p, _ := manualProjector(t, &tombstoneRepositoryStore{docs: docs})
	manager := entity.NewEntityManager()
	repository, err := newEntityRepository(manager, docs, nil, projectorRecoveryGate{p})
	if err != nil {
		t.Fatal(err)
	}
	var tombstone coredata.CommitRecord
	tombstone.ID[15] = 94
	tombstone.Durability = corenest.DurabilityStrict
	for _, resource := range []string{"repository_profile", "repository_inventory"} {
		tombstone.Mutations = append(tombstone.Mutations, coredata.Mutation{
			Key:  coredata.DocumentKey{Database: "game", Resource: resource, ID: id},
			Kind: coredata.MutationDelete, ExpectedVersion: 1, NextVersion: 2,
		})
	}
	if err := p.Commit(context.Background(), tombstone); err != nil {
		t.Fatal(err)
	}
	p.TransactionReleased(tombstone.ID)
	short, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	value, err := repository.LoadEntity(short, id, dataEngineRepositoryKind)
	stop()
	if value != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reload before tombstone projection: value=%v err=%v, want wait (deadline exceeded)", value, err)
	}
	if manager.Get(id) != nil {
		t.Fatal("pending tombstone entity was resurrected into the manager")
	}
	loaded := make(chan error, 1)
	go func() {
		_, err := repository.LoadEntity(context.Background(), id, dataEngineRepositoryKind)
		loaded <- err
	}()
	if _, err := p.ReplayPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := awaitChan(t, loaded, "the reload after tombstone projection"); !errors.Is(err, ErrEntityAggregateNotFound) {
		t.Fatalf("reload after tombstone projection=%v, want ErrEntityAggregateNotFound", err)
	}
	if manager.Get(id) != nil {
		t.Fatal("deleted entity published after reload")
	}
}
