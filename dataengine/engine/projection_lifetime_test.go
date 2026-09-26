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
