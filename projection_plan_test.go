package dataengine

import (
	"math"
	"testing"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
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
	if !isBatchProjectionRecord(ordinary) {
		t.Fatal("ordinary record not batchable")
	}
	for name, edit := range map[string]func(*coredata.CommitRecord){
		"migration": func(r *coredata.CommitRecord) { r.Handler = MigrationHandler },
		"effect":    func(r *coredata.CommitRecord) { r.Effects = []coredata.Effect{{ID: "e", Topic: "t"}} },
		"receipt":   func(r *coredata.CommitRecord) { r.Receipts = []coredata.Receipt{{Namespace: "n", ID: "r"}} },
		"multiple":  func(r *coredata.CommitRecord) { r.Mutations = append(r.Mutations, r.Mutations[0]) },
		"remote":    func(r *coredata.CommitRecord) { r.Mutations[0].Remote = &remoteProjectionTestCommit },
	} {
		t.Run(name, func(t *testing.T) {
			record := projectorRecord(1, false)
			edit(&record)
			if isBatchProjectionRecord(record) {
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
