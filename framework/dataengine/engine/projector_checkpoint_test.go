package engine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
)

type checkpointProbeStore struct {
	project func(coredata.CommitRecord) error
}

func (s checkpointProbeStore) Project(_ context.Context, r coredata.CommitRecord) error {
	return s.project(r)
}
func (s checkpointProbeStore) ProjectBatch(ctx context.Context, records []coredata.CommitRecord) error {
	for _, r := range records {
		if err := s.Project(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

func TestBackgroundCheckpointAllowsSafeProjectionButKeepsAdmission(t *testing.T) {
	records := []coredata.CommitRecord{localMultiRecord(1), localMultiRecord(2)}
	entered, release, projected := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	store := checkpointProbeStore{project: func(r coredata.CommitRecord) error {
		if r.ID == records[1].ID {
			awaitChan(t, entered, "first checkpoint to start")
			close(projected)
		}
		return nil
	}}
	p, w := stoppedProjectorWithRecords(t, store, records, 1)
	defer unblock()
	p.opts.BackgroundCheckpoint = true
	p.opts.CheckpointRecords = 1
	for _, r := range records {
		p.admit(r.ID)
		p.release(r.ID)
	}
	var calls atomic.Int32
	p.ack = func(ctx context.Context, fence corenest.CommitFence) error {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return w.Ack(ctx, fence)
	}
	done := make(chan error, 1)
	go func() { _, err := p.ReplayPass(context.Background()); done <- err }()
	awaitChan(t, projected, "second safe projection while checkpoint is blocked")
	if got := p.Stats().WALUnacked; got != 2 {
		t.Fatalf("unconfirmed admission=%d, want 2", got)
	}
	select {
	case err := <-done:
		t.Fatalf("pass returned before checkpoint: %v", err)
	default:
	}
	unblock()
	if err := awaitChan(t, done, "pass to drain its writer"); err != nil {
		t.Fatal(err)
	}
	if got := p.Stats().WALUnacked; got != 0 {
		t.Fatalf("confirmed admission=%d", got)
	}
	assertWALReplayCount(t, w, 0)
}

func TestBackgroundCheckpointCoalescesOnlyLatestSuccessfulPrefix(t *testing.T) {
	records := []coredata.CommitRecord{localMultiRecord(1), localMultiRecord(2), localMultiRecord(3), localMultiRecord(4)}
	p, w := stoppedProjectorWithRecords(t, &recordingSegmentStore{}, records, 1)
	var fences []corenest.CommitFence
	if err := w.Replay(context.Background(), func(f corenest.CommitFence, _ coredata.CommitRecord) error { fences = append(fences, f); return nil }); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var calls []corenest.CommitFence
	p.ack = func(ctx context.Context, fence corenest.CommitFence) error {
		calls = append(calls, fence)
		if len(calls) == 1 {
			close(entered)
			<-release
		}
		return w.Ack(ctx, fence)
	}
	writer := newReplayCheckpointWriter(context.Background(), p, records, fences)
	defer writer.close()
	defer unblock()
	if err := writer.submit(1); err != nil {
		t.Fatal(err)
	}
	awaitChan(t, entered, "writer to hold first prefix")
	for _, n := range []int{2, 3} {
		if err := writer.submit(n); err != nil {
			t.Fatal(err)
		}
	}
	unblock()
	if err := writer.wait(); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0] != fences[0] || calls[1] != fences[2] {
		t.Fatalf("checkpoint calls=%v", calls)
	}
	assertWALReplayCount(t, w, 1) // 第四条从未投影，不能被最新目标越过。
}

func TestBackgroundCheckpointFailureStopsBeforeNextFastPath(t *testing.T) {
	records := []coredata.CommitRecord{projectorRecord(1, false), projectorRecord(2, false)}
	store := &recordingSegmentStore{}
	p, w := stoppedProjectorWithRecords(t, store, records, 1)
	p.opts.BackgroundCheckpoint = true
	lost := errors.New("checkpoint lost")
	p.ack = func(context.Context, corenest.CommitFence) error { return lost }
	n, err := p.ReplayPass(context.Background())
	if n != 1 || !errors.Is(err, lost) || len(store.events) != 1 {
		t.Fatalf("processed=%d events=%v err=%v", n, store.events, err)
	}
	assertWALReplayCount(t, w, 2)
}

func TestBackgroundCheckpointProjectionFailureConfirmsOnlyPrefix(t *testing.T) {
	records := []coredata.CommitRecord{localMultiRecord(1), localMultiRecord(2), localMultiRecord(3)}
	store := &recordingSegmentStore{failID: records[1].ID}
	p, w := stoppedProjectorWithRecords(t, store, records, 1)
	p.opts.BackgroundCheckpoint = true
	p.opts.CheckpointRecords = 1
	n, err := p.ReplayPass(context.Background())
	if n != 1 || err == nil {
		t.Fatalf("processed=%d err=%v", n, err)
	}
	assertWALReplayCount(t, w, 2)
}
