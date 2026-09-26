package engine

import (
	"context"
	"errors"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
)

func TestReplayReadBudgetRetainsOnlyBoundedPrefix(t *testing.T) {
	records := []coredata.CommitRecord{projectorRecord(1, false), projectorRecord(2, false), projectorRecord(3, false)}
	for i := range records {
		records[i].Mutations[0].Data = make([]byte, 128<<10)
	}
	for _, extra := range []int{-1, 0} {
		t.Run(map[int]string{-1: "before_boundary", 0: "exact_boundary"}[extra], func(t *testing.T) {
			p, w := stoppedProjectorWithRecords(t, &recordingSegmentStore{}, records, 4<<20)
			p.opts.ReplayReadBytes = 2*projectionRecordLogicalBytes(records[0]) + extra
			want := 2
			if extra < 0 {
				want = 1
			}
			if n, err := p.ReplayPass(context.Background()); n != want || err != nil {
				t.Fatalf("read window n=%d err=%v", n, err)
			}
			ids := make([]coredata.TransactionID, 0, len(records)-want)
			for _, r := range records[want:] {
				ids = append(ids, r.ID)
			}
			assertWALReplayIDs(t, w, ids)
			for len(ids) > 0 {
				n, err := p.ReplayPass(context.Background())
				if err != nil || n == 0 {
					t.Fatalf("no progress: %d %v", n, err)
				}
				ids = ids[n:]
			}
			assertWALReplayCount(t, w, 0)
		})
	}
}

func TestReplayOversizedRecordGetsExclusiveReadWindow(t *testing.T) {
	records := []coredata.CommitRecord{projectorRecord(1, false), projectorRecord(2, false)}
	records[0].Mutations[0].Data = make([]byte, 128<<10)
	p, w := stoppedProjectorWithRecords(t, &recordingSegmentStore{}, records, 4<<20)
	p.opts.ReplayReadBytes = 1024
	if n, err := p.ReplayPass(context.Background()); n != 1 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	assertWALReplayIDs(t, w, []coredata.TransactionID{records[1].ID})
	if n, err := p.ReplayPass(context.Background()); n != 1 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	assertWALReplayCount(t, w, 0)
}

func TestReplayBudgetCountsConsumedRecordsAcrossSegments(t *testing.T) {
	for _, budget := range []string{"records", "bytes"} {
		t.Run(budget, func(t *testing.T) {
			opts := nestwal.DefaultOptions(t.TempDir())
			opts.WriterVersion = nestwal.WriterVersionV2
			opts.SegmentBytes, opts.MaxRecordBytes = 4096, 2048
			w, err := nestwal.Open(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close(context.Background())
			p, err := NewProjector(w, &recordingSegmentStore{}, ProjectorOptions{IdlePoll: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			p.cancel()
			awaitChan(t, p.done, "stopped projector")
			defer p.Close(context.Background())
			p.opts.ReplayBatchRecords = 20
			for i := byte(1); i <= 20; i++ {
				record := projectorRecord(i, false)
				record.Mutations[0].Data = make([]byte, 1024)
				if budget == "bytes" {
					p.opts.ReplayReadBytes = 2 * projectionRecordLogicalBytes(record)
				} else {
					p.opts.ReplayBatchRecords = 2
				}
				if _, err := w.Append(context.Background(), record); err != nil {
					t.Fatal(err)
				}
			}
			if w.Stats().SegmentFiles < 2 {
				t.Fatal("test did not cross a physical segment")
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if n, err := p.ReplayPass(ctx); n != 0 || !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel: %d %v", n, err)
			}
			if w.Stats().Replayed != 0 {
				t.Fatal("canceled pass consumed records")
			}
			for total := 0; total < 20; {
				n, err := p.ReplayPass(t.Context())
				if n != 2 || err != nil {
					t.Fatalf("n=%d err=%v", n, err)
				}
				total += n
				if got := w.Stats().Replayed; got != uint64(total) {
					t.Fatalf("consumed=%d replayed=%d", total, got)
				}
			}
			if p.Stats().Projected != 20 {
				t.Fatal(p.Stats())
			}
			assertWALReplayCount(t, w, 0)
		})
	}
}
