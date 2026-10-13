// 投影：分段计划、Projector 生命期与实体投影屏障、fatal 唤醒等待方、Commit 与 Enqueue 结果一致、停机截止。

package engine

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
)

func projectionTestFences(records []coredata.CommitRecord) []corenest.CommitFence {
	fences := make([]corenest.CommitFence, len(records))
	for i, record := range records {
		fences[i] = corenest.CommitFence{TransactionID: record.ID, Segment: uint64(i), Offset: int64(i)}
	}
	return fences
}

func TestPlanProjectionSegmentsPreservesMixedOrder(t *testing.T) {
	records := []coredata.CommitRecord{
		projectorRecord(1, false), projectorRecord(2, false),
		projectorRecord(3, true), projectorRecord(4, false),
	}
	fences := projectionTestFences(records)
	segments, err := planProjectionSegments(records, fences, 16, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 3 || !segments[0].batch || segments[1].batch || !segments[2].batch {
		t.Fatalf("segments=%+v", segments)
	}
	if len(segments[0].records) != 2 || segments[1].records[0].ID != records[2].ID {
		t.Fatalf("segment records=%+v", segments)
	}
}

func TestBatchProjectionEligibilityMatchesMongoContract(t *testing.T) {
	ordinary := projectorRecord(1, false)
	if !isLocalBatchRecord(ordinary) {
		t.Fatal("ordinary record not batchable")
	}
	for name, edit := range map[string]func(*coredata.CommitRecord){
		"effect":  func(r *coredata.CommitRecord) { r.Effects = []coredata.Effect{{ID: "e", Topic: "t"}} },
		"receipt": func(r *coredata.CommitRecord) { r.Receipts = []coredata.Receipt{{Namespace: "n", ID: "r"}} },
		"remote":  func(r *coredata.CommitRecord) { r.Mutations[0].Remote = &remoteProjectionTestCommit },
	} {
		t.Run(name, func(t *testing.T) {
			record := projectorRecord(1, false)
			edit(&record)
			if isLocalBatchRecord(record) {
				t.Fatal("special record is batchable")
			}
		})
	}
}

var remoteProjectionTestCommit = func() entity.RemoteCommit { return entity.RemoteCommit{} }()

func TestPlanProjectionSegmentsBoundsOrdinaryBatches(t *testing.T) {
	records := []coredata.CommitRecord{projectorRecord(1, false), projectorRecord(2, false), projectorRecord(3, false)}
	fences := projectionTestFences(records)
	one := projectionRecordLogicalBytes(records[0])
	segments, err := planProjectionSegments(records, fences, 2, one*2-1)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 3 {
		t.Fatalf("segments=%d", len(segments))
	}
}

func TestPlanProjectionSegmentsHonorsMaxRecords(t *testing.T) {
	records := []coredata.CommitRecord{projectorRecord(1, false), projectorRecord(2, false), projectorRecord(3, false)}
	segments, err := planProjectionSegments(records, projectionTestFences(records), 2, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2 || len(segments[0].records) != 2 || len(segments[1].records) != 1 {
		t.Fatalf("segments=%+v", segments)
	}
}

func TestPlanProjectionSegmentsPreservesEveryRecordAndFence(t *testing.T) {
	records := []coredata.CommitRecord{projectorRecord(1, false), projectorRecord(2, true), projectorRecord(3, false), projectorRecord(4, false)}
	fences := projectionTestFences(records)
	segments, err := planProjectionSegments(records, fences, 1, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	flat := 0
	for _, segment := range segments {
		if len(segment.records) != len(segment.fences) {
			t.Fatalf("segment records=%d fences=%d", len(segment.records), len(segment.fences))
		}
		for i := range segment.records {
			if segment.records[i].ID != records[flat].ID || segment.fences[i] != fences[flat] {
				t.Fatalf("at flattened index %d: got record=%v fence=%v", flat, segment.records[i].ID, segment.fences[i])
			}
			flat++
		}
	}
	if flat != len(records) {
		t.Fatalf("flattened %d records, want %d", flat, len(records))
	}
}

func TestProjectionPlanSaturatingMultiply(t *testing.T) {
	if got := saturatingMul(3, 4); got != 12 {
		t.Fatalf("saturatingMul(3,4)=%d", got)
	}
	if got := saturatingMul(math.MaxInt, 2); got != math.MaxInt {
		t.Fatalf("positive overflow=%d", got)
	}
}

func TestPlanProjectionSegmentsRejectsMismatchedFences(t *testing.T) {
	if _, err := planProjectionSegments([]coredata.CommitRecord{projectorRecord(1, false)}, nil, 16, 4<<20); err == nil {
		t.Fatal("expected record/fence mismatch")
	}
}

func TestPlanProjectionSegmentsHandlesOversizedFirstRecord(t *testing.T) {
	records := []coredata.CommitRecord{projectorRecord(1, false), projectorRecord(2, false)}
	segments, err := planProjectionSegments(records, projectionTestFences(records), 16, 1)
	if err != nil || len(segments) != 2 || len(segments[0].records) != 1 {
		t.Fatalf("segments=%+v err=%v", segments, err)
	}
}

func TestPlanProjectionSegmentsRejectsInvalidLimits(t *testing.T) {
	records := []coredata.CommitRecord{projectorRecord(1, false)}
	for _, limits := range [][2]int{{0, 1}, {1, 0}} {
		if _, err := planProjectionSegments(records, projectionTestFences(records), limits[0], limits[1]); err == nil {
			t.Fatalf("limits=%v accepted", limits)
		}
	}
}

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

// manualProjector 返回运行中（ctx 未取消）但不启动后台循环的 Projector。等待屏障的测试必须用它：已 cancel 的 projector 让
// WaitEntityProjection 直接返回 ErrRuntimeStopped，证明不了“会等待”。
func manualProjector(t *testing.T, store ProjectionStore) (*Projector, *nestwal.WAL) {
	t.Helper()
	options := nestwal.DefaultOptions(t.TempDir())
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

// 冷加载前等待该实体全部 DAO 的在途投影：运行中的 projector 上，截止时间内投影未完成就返回截止错误；投影成功后释放；Close 唤醒仍在
// 等待的调用方。
// 历史：RR-20260926-10。
func TestEntityProjectionBarrierTracksAllDAOsAndReleasesOnFailure(t *testing.T) {
	p, _ := manualProjector(t, &recordingSegmentStore{})
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

// 自定义 OnFatal 里同步 Close 不能自等：OnFatal 异步投递；fatal 在回调之前已对准入与等待方可见。
func TestProjectorOnFatalMayCloseProjector(t *testing.T) {
	for _, background := range []bool{false, true} {
		name := "external_flush"
		if background {
			name = "background_loop"
		}
		t.Run(name, func(t *testing.T) {
			options := nestwal.DefaultOptions(t.TempDir())
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

// 投影 fatal 之后本进程不再投影 fatal 记录及其后的任何记录：所有待投影实体的等待方（包括 fatal 批次之外的实体）立即拿到可判别的 fatal，
// fatal 之后才开始的等待同样如此。
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

// pipelined 记录的 WAL 票据失败（写入结果未知，WAL terminal）后本进程不会投影它，该实体的等待方立即拿到 WAL 错误。真实 WAL 没有可从本包
// 注入 fsync 失败的缝，这里降到内部入口：登记在途记录后直接把失败票据交给 signalWhenDurable。
func TestEntityProjectionWaiterSeesFailedPipelinedDurability(t *testing.T) {
	p, _ := manualProjector(t, &recordingSegmentStore{})
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

// 同一事务里的 Remote 写与 lease-fence receipt 在写 WAL 前被拒绝，三个准入入口都要拒；用 WAL 会接受的合法 Remote 写与合法 receipt
// （先用 ValidateCommitRecord 证明），每个入口用独立事务，逐个报告。
// 历史：RR-20260926-19。
func TestRemoteLeaseFenceRejectedBeforeWALAdmission(t *testing.T) {
	p, w := stoppedProjectorWithRecords(t, &recordingSegmentStore{}, nil, 4<<20)
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

// 外部夹具需要“不启动后台循环”的 Projector 来逐步驱动回放：ManualReplay 下没有后台循环（done 一开始就关闭），记录只由显式 ReplayPass
// 投影；Close 之后仍拒绝 ReplayPass / Flush。
func TestManualReplayProjectorRunsOnlyExplicitPasses(t *testing.T) {
	options := nestwal.DefaultOptions(t.TempDir())
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

// 本地删档只把 tombstone 写进 WAL：投影前重新加载必须等待，不能从旧库读出活实体；投影后加载得到 not found，且不发布到 Manager。
func TestRepositoryReloadDoesNotResurrectPendingTombstone(t *testing.T) {
	ensureDataEngineRepositoryEntity()
	id, _ := entity.BuildEntityID(994, dataEngineRepositoryKind)
	docs := &repositoryStore{docs: map[string][]coredata.RawDocument{
		"repository_profile":   {repositoryRaw(t, "repository_profile", id, 1)},
		"repository_inventory": {repositoryRaw(t, "repository_inventory", id, 1)},
	}}
	p, _ := manualProjector(t, &tombstoneRepositoryStore{docs: docs})
	manager := entity.NewEntityManager()
	repository, err := newEntityRepository(manager, docs, projectorRecoveryGate{p})
	if err != nil {
		t.Fatal(err)
	}
	var tombstone coredata.CommitRecord
	tombstone.ID[15] = 94
	tombstone.Durability = corenest.DurabilityStrict.Record()
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

// Projector 的 Commit（严格）与 Enqueue（流水线）对同一条记录给出同样的结果：拒绝时都不留下准入、不计提交；接受时都在记录落盘后尽快投影
// （流水线提交不等到 IdlePoll）。

func newPipelinedProjector(t *testing.T, mutate func(*nestwal.Options)) (*Projector, *projectorOutboxFake) {
	t.Helper()
	opts := nestwal.DefaultOptions(t.TempDir())
	if mutate != nil {
		mutate(&opts)
	}
	wal, err := nestwal.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	store := newProjectorOutboxFake()
	// IdlePoll 拉到一小时：投影只能靠唤醒发生，不能靠轮询兜底。
	projector, err := NewProjector(wal, store, ProjectorOptions{CloseWAL: true, IdlePoll: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = projector.Close(context.Background()) })
	return projector, store
}

func pipelinedRecord(sequence byte) corenest.CommitRecord {
	record := projectorRecord(sequence, false)
	record.Durability = corenest.DurabilityPipelined.Record()
	return record
}

func TestPipelinedCommitIsProjectedOnceDurableWithoutWaitingForIdlePoll(t *testing.T) {
	// 批延迟拉到 200ms：事务释放时的那次唤醒必然早于落盘，和生产里一样。
	projector, store := newPipelinedProjector(t, func(opts *nestwal.Options) { opts.BatchDelay = 200 * time.Millisecond })
	// 让启动时的那次唤醒先被消费掉，投影循环进入 IdlePoll 等待。
	time.Sleep(50 * time.Millisecond)
	record := pipelinedRecord(11)
	ticket, err := projector.Enqueue(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	// nest 在 Enqueue 返回后立刻释放实体锁并通知投影器。
	projector.TransactionReleased(record.ID)
	select {
	case <-ticket.Done():
		if err := ticket.Err(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("pipelined commit never became durable")
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		store.mu.Lock()
		_, projected := store.records[record.ID]
		store.mu.Unlock()
		if projected {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a durable pipelined commit was not projected within 500ms (stats %+v); it would wait for IdlePoll", projector.Stats())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCommitAndEnqueueRejectAnOversizedRecordAlikeAndLeaveNoAdmission(t *testing.T) {
	projector, store := newPipelinedProjector(t, func(opts *nestwal.Options) { opts.MaxRecordBytes = 512 })
	ctx := context.Background()
	strict := projectorRecord(12, false)
	strict.Mutations[0].Data = make([]byte, 4096)
	pipelined := pipelinedRecord(13)
	pipelined.Mutations[0].Data = make([]byte, 4096)

	if err := projector.Commit(ctx, strict); !errors.Is(err, nestwal.ErrRecordTooLarge) {
		t.Fatalf("Commit of an oversized record = %v, want ErrRecordTooLarge", err)
	}
	if _, err := projector.Enqueue(ctx, pipelined); !errors.Is(err, nestwal.ErrRecordTooLarge) {
		t.Fatalf("Enqueue of an oversized record = %v, want ErrRecordTooLarge", err)
	}
	for _, id := range []corenest.TransactionID{strict.ID, pipelined.ID} {
		if projector.isHeld(id) {
			t.Fatalf("a rejected record is still held: %s", id.String())
		}
	}
	if stats := projector.Stats(); stats.Committed != 0 || stats.WALUnacked != 0 {
		t.Fatalf("rejected records were counted: %+v", stats)
	}
	// 对照：两条路径接受的记录都被投影。
	if err := projector.Commit(ctx, projectorRecord(14, false)); err != nil {
		t.Fatal(err)
	}
	projector.TransactionReleased(projectorRecord(14, false).ID)
	ticket, err := projector.Enqueue(ctx, pipelinedRecord(15))
	if err != nil {
		t.Fatal(err)
	}
	projector.TransactionReleased(pipelinedRecord(15).ID)
	<-ticket.Done()
	deadline := time.Now().Add(time.Second)
	for {
		store.mu.Lock()
		_, strictProjected := store.records[projectorRecord(14, false).ID]
		_, pipelinedProjected := store.records[pipelinedRecord(15).ID]
		store.mu.Unlock()
		if strictProjected && pipelinedProjected {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("accepted records not projected: strict=%v pipelined=%v", strictProjected, pipelinedProjected)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type blockedProjectionStore struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockedProjectionStore) Project(ctx context.Context, _ coredata.CommitRecord) error {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func requireDeadline(t *testing.T, call func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- call(ctx) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("got %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Error("operation remained blocked after its deadline")
	}
}

// 先用通道确认存储正在执行，再验证另一个调用能取消等待；不靠 sleep 猜测锁时序。
func TestProjectorWaitsRespectDeadline(t *testing.T) {
	for _, method := range []string{"replay", "flush", "second_flush"} {
		t.Run(method, func(t *testing.T) {
			store := &blockedProjectionStore{entered: make(chan struct{}), release: make(chan struct{})}
			p, wal := stoppedProjectorWithRecords(t, store, []coredata.CommitRecord{projectorRecord(91, false)}, 0)
			first := make(chan error, 1)
			go func() {
				if method == "second_flush" {
					first <- p.Flush(context.Background())
				} else {
					_, err := p.ReplayPass(context.Background())
					first <- err
				}
			}()
			<-store.entered
			requireDeadline(t, func(ctx context.Context) error {
				if method == "replay" {
					_, err := p.ReplayPass(ctx)
					return err
				}
				return p.Flush(ctx)
			})
			close(store.release)
			if err := <-first; err != nil {
				t.Fatal(err)
			}
			if err := p.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertWALReplayCount(t, wal, 0)
		})
	}
}

func TestProjectorShutdownDeadlineBehindBackgroundProjection(t *testing.T) {
	opts := nestwal.DefaultOptions(t.TempDir())
	w, err := nestwal.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	store := &blockedProjectionStore{entered: make(chan struct{}), release: make(chan struct{})}
	p, err := NewProjector(w, store, ProjectorOptions{CloseWAL: true})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(context.Background())
	defer close(store.release)
	record := projectorRecord(92, false)
	if err = p.Commit(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	p.TransactionReleased(record.ID)
	<-store.entered
	requireDeadline(t, p.Shutdown)
	if t.Failed() {
		return // 修前复现时让 defer 释放存储，不在清理阶段再次无限等待。
	}
	if err := p.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 取消等待不能把未投影记录确认为完成；真实 WAL 重开后仍可恢复。
	reopened, err := nestwal.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(context.Background())
	assertWALReplayIDs(t, reopened, []coredata.TransactionID{record.ID})
}

type shutdownBlockedOutbox struct {
	*projectorOutboxFake
	entered, canceled, release chan struct{}
	enterOnce, cancelOnce      sync.Once
}

func (s *shutdownBlockedOutbox) Claim(ctx context.Context, _ string, _ time.Time, _ int, _ time.Duration) ([]OutboxItem, error) {
	s.enterOnce.Do(func() { close(s.entered) })
	<-ctx.Done()
	s.cancelOnce.Do(func() { close(s.canceled) })
	<-s.release
	return nil, ctx.Err()
}

func TestRuntimeConcurrentShutdownHonorsDeadline(t *testing.T) {
	s := &shutdownBlockedOutbox{projectorOutboxFake: newProjectorOutboxFake(), entered: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{})}
	w, err := NewOutboxWorker(s, failingOutboxPublisher{}, OutboxWorkerOptions{Owner: "deadline", Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	w.Start(context.Background())
	<-s.entered
	a := &Assembly{runtime: &Runtime{Outbox: w}}
	first := make(chan error, 1)
	go func() { first <- a.Shutdown(context.Background()) }()
	<-s.canceled // 第一次 Shutdown 已持有运行时关闭所有权并等待 worker。
	requireDeadline(t, a.Shutdown)
	if a.Runtime() == nil {
		t.Error("incomplete shutdown lost its runtime")
	}
	close(s.release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.Runtime() != nil {
		t.Fatal("completed shutdown retained runtime")
	}
}
