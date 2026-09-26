package engine

import (
	"context"
	"errors"
	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
	"testing"
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
func TestEntityProjectionBarrierTracksAllDAOsAndReleasesOnFailure(t *testing.T) {
	p, _ := stoppedProjectorWithRecords(t, &multiSegmentStore{}, nil, 4<<20)
	record := localMultiRecord(1)
	record.Mutations[1].Key.ID = 999
	if err := p.Commit(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, id := range []int64{record.Mutations[0].Key.ID, 999} {
		if err := p.WaitEntityProjection(canceled, id); err == nil {
			t.Fatal("pending entity did not wait")
		}
	}
	p.TransactionReleased(record.ID)
	if _, err := p.ReplayPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{record.Mutations[0].Key.ID, 999} {
		if err := p.WaitEntityProjection(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	if len(p.pendingEntities) != 0 || len(p.pendingTransactions) != 0 {
		t.Fatal("projection barrier leaked")
	}
}

func TestRemoteLeaseFenceRejectedBeforeWALAdmission(t *testing.T) {
	p, w := stoppedProjectorWithRecords(t, &multiSegmentStore{}, nil, 4<<20)
	record := localMultiRecord(1)
	record.Mutations[0].Remote = &remoteProjectionTestCommit
	record.Receipts = []coredata.Receipt{{Namespace: coredata.LeaseFenceReceiptNamespace}}
	for _, call := range []func() error{
		func() error { return p.Commit(context.Background(), record) },
		func() error { _, err := p.Enqueue(context.Background(), record); return err },
		func() error { _, err := p.CommitSystem(context.Background(), record); return err },
	} {
		if err := call(); !errors.Is(err, ErrRemoteLeaseFenceUnsupported) {
			t.Fatalf("admission=%v", err)
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
