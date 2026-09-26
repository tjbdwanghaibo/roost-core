package engine

import (
	"context"
	"testing"

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
