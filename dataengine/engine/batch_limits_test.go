package engine

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
)

type multiSegmentStore struct{ recordingSegmentStore }

func (*multiSegmentStore) SupportsMultiMutationBatch() bool { return true }

func localMultiRecord(id byte) coredata.CommitRecord {
	r := projectorRecord(id, false)
	second := r.Mutations[0]
	second.Key.Resource = "inventory"
	r.Mutations = append(r.Mutations, second)
	return r
}

func TestMultiBatchCapabilityAndSpecialBoundaries(t *testing.T) {
	records := []coredata.CommitRecord{localMultiRecord(1), localMultiRecord(2), projectorRecord(3, true), localMultiRecord(4)}
	for _, capable := range []bool{false, true} {
		var store ProjectionStore = &recordingSegmentStore{}
		var events *[]string
		if capable {
			s := &multiSegmentStore{}
			store = s
			events = &s.events
		} else {
			events = &store.(*recordingSegmentStore).events
		}
		p, w := stoppedProjectorWithRecords(t, store, records, 4<<20)
		p.opts.CheckpointInterval = time.Hour
		p.ack = func(ctx context.Context, fence corenest.CommitFence) error {
			*events = append(*events, "ack")
			return w.Ack(ctx, fence)
		}
		if n, err := p.ReplayPass(context.Background()); n != 4 || err != nil {
			t.Fatalf("n=%d err=%v", n, err)
		}
		want := []string{"project:" + records[0].ID.String(), "ack", "project:" + records[1].ID.String(), "ack", "project:" + records[2].ID.String(), "ack", "project:" + records[3].ID.String(), "ack"}
		if capable {
			want = []string{"batch:2", "ack", "project:" + records[2].ID.String(), "ack", "project:" + records[3].ID.String(), "ack"}
		}
		if !slices.Equal(*events, want) {
			t.Fatalf("capable=%v events=%v want=%v", capable, *events, want)
		}
	}
}

func TestMultiCheckpointThresholdsAndFailurePrefix(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		count                   int
		interval                time.Duration
		fail                    bool
		wantAcks, wantProcessed int
	}{
		{"end", 256, time.Hour, false, 1, 3}, {"count", 2, time.Hour, false, 2, 3},
		{"time", 256, 20 * time.Millisecond, false, 3, 3}, {"legacy", 1, time.Hour, false, 3, 3},
		{"failure", 256, time.Hour, true, 1, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records := []coredata.CommitRecord{localMultiRecord(1), localMultiRecord(2), localMultiRecord(3)}
			store := &multiSegmentStore{}
			if tc.fail {
				store.failID = records[2].ID
			}
			// 每条超过字节窗口，Mongo 按笔处理；安全的 marker 允许跨段合并 ack。
			p, w := stoppedProjectorWithRecords(t, store, records, 1)
			p.opts.CheckpointRecords = tc.count
			p.opts.CheckpointInterval = tc.interval
			clock := time.Unix(0, 0)
			p.now = func() time.Time { clock = clock.Add(20 * time.Millisecond); return clock }
			acks := 0
			p.ack = func(ctx context.Context, f corenest.CommitFence) error { acks++; return w.Ack(ctx, f) }
			n, err := p.ReplayPass(context.Background())
			if n != tc.wantProcessed || acks != tc.wantAcks || (err != nil) != tc.fail {
				t.Fatalf("n=%d acks=%d err=%v", n, acks, err)
			}
			assertWALReplayCount(t, w, 3-n)
		})
	}
}

func TestMultiCheckpointLossKeepsEntireSuccessfulPrefix(t *testing.T) {
	records := []coredata.CommitRecord{localMultiRecord(1), localMultiRecord(2), localMultiRecord(3)}
	p, w := stoppedProjectorWithRecords(t, &multiSegmentStore{}, records, 1)
	p.opts.CheckpointInterval = time.Hour
	ackErr := errors.New("lost checkpoint")
	p.ack = func(context.Context, corenest.CommitFence) error { return ackErr }
	if n, err := p.ReplayPass(context.Background()); n != 3 || !errors.Is(err, ackErr) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	assertWALReplayCount(t, w, 3)
	p.ack = w.Ack
	if n, err := p.ReplayPass(context.Background()); n != 3 || err != nil {
		t.Fatalf("retry n=%d err=%v", n, err)
	}
	assertWALReplayCount(t, w, 0)
}

func TestMultiCheckpointFlushesBeforeHeldAndOnCancel(t *testing.T) {
	records := []coredata.CommitRecord{localMultiRecord(1), localMultiRecord(2), localMultiRecord(3)}
	p, w := stoppedProjectorWithRecords(t, &multiSegmentStore{}, records, 1)
	p.opts.CheckpointInterval = time.Hour
	p.admit(records[2].ID)
	n, err := p.ReplayPass(context.Background())
	if n != 2 || !errors.Is(err, errProjectorTransactionHeld) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	assertWALReplayIDs(t, w, []coredata.TransactionID{records[2].ID})
	p.release(records[2].ID)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.ReplayPass(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled replay=%v", err)
	}
	assertWALReplayCount(t, w, 1)
}

func TestMarkerlessSingleRecordStillCheckpointsImmediately(t *testing.T) {
	records := []coredata.CommitRecord{projectorRecord(1, false), projectorRecord(2, false)}
	s := &multiSegmentStore{}
	p, w := stoppedProjectorWithRecords(t, s, records, 1)
	p.opts.CheckpointInterval = time.Hour
	p.ack = func(ctx context.Context, f corenest.CommitFence) error {
		s.events = append(s.events, "ack")
		return w.Ack(ctx, f)
	}
	if _, err := p.ReplayPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"project:" + records[0].ID.String(), "ack", "project:" + records[1].ID.String(), "ack"}
	if !slices.Equal(s.events, want) {
		t.Fatalf("events=%v", s.events)
	}
}

func TestProjectionAdmissionConcurrentLimitAndRecovery(t *testing.T) {
	p, w := stoppedProjectorWithRecords(t, &multiSegmentStore{}, nil, 4<<20)
	p.opts.MaxUnackedRecords = 8
	p.opts.WarnUnackedRecords = 4
	var accepted atomic.Int64
	var group sync.WaitGroup
	for i := range 64 {
		group.Go(func() {
			r := localMultiRecord(byte(i + 1))
			if err := p.Commit(context.Background(), r); err == nil {
				accepted.Add(1)
				p.TransactionReleased(r.ID)
			} else if !errors.Is(err, ErrProjectionBackpressure) {
				t.Errorf("commit=%v", err)
			}
		})
	}
	group.Wait()
	if s := p.Stats(); accepted.Load() != 8 || s.WALUnacked != 8 || s.AdmissionRejected != 56 || !s.BacklogWarning || s.FatalProjectionConflicts != 0 {
		t.Fatalf("accepted=%d stats=%+v", accepted.Load(), s)
	}
	assertWALReplayCount(t, w, 8)
	if ticket, err := p.CommitSystem(context.Background(), localMultiRecord(90)); ticket != nil || !errors.Is(err, ErrProjectionBackpressure) {
		t.Fatalf("system ticket=%v err=%v", ticket, err)
	}
	if len(p.tickets) != 0 {
		t.Fatal("rejected system left ticket")
	}
	if ticket, err := p.Enqueue(context.Background(), localMultiRecord(91)); ticket != nil || !errors.Is(err, ErrProjectionBackpressure) {
		t.Fatalf("enqueue=%v %v", ticket, err)
	}
	if err := p.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := p.Stats(); s.WALUnacked != 0 || s.BacklogWarning {
		t.Fatalf("after recovery=%+v", s)
	}
	if err := p.Commit(context.Background(), localMultiRecord(92)); err != nil {
		t.Fatal(err)
	}
	p.TransactionReleased(localMultiRecord(92).ID)
	if err := p.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestProjectionAdmissionReturnsCapacityOnAppendFailure(t *testing.T) {
	p, _ := stoppedProjectorWithRecords(t, &multiSegmentStore{}, nil, 4<<20)
	p.opts.MaxUnackedRecords = 1
	invalid := localMultiRecord(1)
	invalid.Mutations[0].Key.Resource = ""
	for _, call := range []func() error{
		func() error { return p.Commit(context.Background(), invalid) },
		func() error { _, err := p.Enqueue(context.Background(), invalid); return err },
		func() error { _, err := p.CommitSystem(context.Background(), invalid); return err },
	} {
		if err := call(); err == nil || errors.Is(err, ErrProjectionBackpressure) {
			t.Fatalf("err=%v", err)
		}
		if p.Stats().WALUnacked != 0 || len(p.held) != 0 || len(p.tickets) != 0 {
			t.Fatalf("leaked state=%+v", p.Stats())
		}
	}
}

type cancelAfterMultiStore struct {
	multiSegmentStore
	cancel context.CancelFunc
}

func (s *cancelAfterMultiStore) Project(ctx context.Context, r coredata.CommitRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.multiSegmentStore.Project(ctx, r); err != nil {
		return err
	}
	s.cancel()
	return nil
}
func TestMultiCheckpointCancellationConfirmsOnlySuccessfulPrefix(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	records := []coredata.CommitRecord{localMultiRecord(1), localMultiRecord(2)}
	p, w := stoppedProjectorWithRecords(t, &cancelAfterMultiStore{cancel: cancel}, records, 1)
	p.opts.CheckpointInterval = time.Hour
	if n, err := p.ReplayPass(ctx); n != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	assertWALReplayIDs(t, w, []coredata.TransactionID{records[1].ID})
}

func TestLocalBatchEligibilityChecksEveryMutation(t *testing.T) {
	for name, edit := range map[string]func(*coredata.CommitRecord){
		"remote_second": func(r *coredata.CommitRecord) { r.Mutations[1].Remote = &remoteProjectionTestCommit },
		"receipt":       func(r *coredata.CommitRecord) { r.Receipts = []coredata.Receipt{{Namespace: "n", ID: "1"}} },
		"effect":        func(r *coredata.CommitRecord) { r.Effects = []coredata.Effect{{ID: "e", Topic: "t"}} },
		"migration":     func(r *coredata.CommitRecord) { r.Handler = MigrationHandler },
	} {
		t.Run(name, func(t *testing.T) {
			records := []coredata.CommitRecord{localMultiRecord(1), localMultiRecord(2), localMultiRecord(3)}
			edit(&records[1])
			segments, err := planProjectionSegments(records, projectionTestFences(records), 16, 4<<20, true)
			if err != nil || len(segments) != 3 || segments[1].batch {
				t.Fatalf("segments=%+v err=%v", segments, err)
			}
		})
	}
}
