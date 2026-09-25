package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
)

type parallelRemoteStore struct {
	project func(context.Context, coredata.CommitRecord) error
}

func (*parallelRemoteStore) SupportsRemoteParallelProjection() bool { return true }
func (s *parallelRemoteStore) Project(ctx context.Context, r coredata.CommitRecord) error {
	return s.project(ctx, r)
}

func remoteProjectionRecord(t *testing.T, sequence byte) coredata.CommitRecord {
	t.Helper()
	const kind entity.EntityKind = 241
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, err := entity.BuildEntityID(int64(sequence), kind)
	if err != nil {
		t.Fatal(err)
	}
	r := projectorRecord(sequence, false)
	c := entity.RemoteCommit{TransactionID: entity.RemoteTransactionID(r.ID), EntityID: id, Kind: kind, BaseVersion: 1, NextVersion: 2, MarkerEpoch: 1, RouteEpoch: 1, LockFence: 1,
		Mutations: []entity.RemoteDataMutation{{Collection: "heroes", ID: id, Version: 2, Mask: 1, Data: []byte("remote")}}}
	r.Mutations = []coredata.Mutation{{Key: coredata.DocumentKey{Resource: "heroes", ID: id}, Kind: coredata.MutationPut, ExpectedVersion: 1, NextVersion: 2, Remote: &c}}
	return r
}

func TestRemoteProjectionIndependentEntityCompletesWhileFirstBlocked(t *testing.T) {
	records := []coredata.CommitRecord{remoteProjectionRecord(t, 1), remoteProjectionRecord(t, 2)}
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	s := &parallelRemoteStore{project: func(ctx context.Context, r coredata.CommitRecord) error {
		if r.ID == records[0].ID {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}}
	p, w := stoppedProjectorWithRecords(t, s, records, 4<<20)
	ticket := &projectionTicket{done: make(chan struct{})}
	p.tickets[records[1].ID] = ticket
	done := make(chan struct{})
	var n int
	var err error
	go func() { defer close(done); n, err = p.ReplayPass(context.Background()) }()
	awaitChan(t, entered, "first remote projection")
	awaitChan(t, ticket.Done(), "independent entity confirmation before first returns")
	select {
	case <-done:
		t.Fatal("replay returned before all workers finished")
	default:
	}
	assertWALReplayCount(t, w, 2)
	once.Do(func() { close(release) })
	awaitChan(t, done, "parallel replay completion")
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	assertWALReplayCount(t, w, 0)
}

func TestRemoteProjectionFailureOnlyAcknowledgesPrefixAndReplaysSuffix(t *testing.T) {
	records := []coredata.CommitRecord{remoteProjectionRecord(t, 1), remoteProjectionRecord(t, 2), remoteProjectionRecord(t, 3), remoteProjectionRecord(t, 4)}
	var mu sync.Mutex
	calls := map[coredata.TransactionID]int{}
	fail := true
	s := &parallelRemoteStore{project: func(_ context.Context, r coredata.CommitRecord) error {
		mu.Lock()
		defer mu.Unlock()
		calls[r.ID]++
		if r.ID == records[1].ID && fail {
			return context.DeadlineExceeded
		}
		return nil
	}}
	p, w := stoppedProjectorWithRecords(t, s, records, 4<<20)
	p.opts.RemoteProjectionWorkers = 3
	n, err := p.ReplayPass(context.Background())
	if n != 1 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	assertWALReplayIDs(t, w, []coredata.TransactionID{records[1].ID, records[2].ID, records[3].ID})
	if calls[records[2].ID] != 1 || calls[records[3].ID] != 0 {
		t.Fatalf("calls=%v", calls)
	}
	fail = false
	if n, err = p.ReplayPass(context.Background()); n != 3 || err != nil {
		t.Fatalf("retry n=%d err=%v", n, err)
	}
	if calls[records[2].ID] != 2 {
		t.Fatal("successful suffix must be replayed")
	}
	assertWALReplayCount(t, w, 0)
}

func TestRemoteProjectionWindowBoundaries(t *testing.T) {
	for _, name := range []string{"independent", "overlap", "transaction", "effect", "receipt", "ordinary", "mixed", "migration", "invalid", "workers", "bytes"} {
		t.Run(name, func(t *testing.T) {
			records := []coredata.CommitRecord{remoteProjectionRecord(t, 1), remoteProjectionRecord(t, 2), remoteProjectionRecord(t, 3)}
			workers, bytes, want := 8, 4<<20, 3
			switch name {
			case "overlap":
				records[1].Mutations = coredata.CloneCommitRecord(records[0]).Mutations
				records[1].Mutations[0].Remote.TransactionID = entity.RemoteTransactionID(records[1].ID)
				want = 1
			case "transaction":
				records[1].ID = records[0].ID
				records[1].Mutations[0].Remote.TransactionID = entity.RemoteTransactionID(records[0].ID)
				want = 1
			case "effect":
				records[1].Effects = []coredata.Effect{{ID: "e", Topic: "t"}}
				want = 1
			case "receipt":
				records[1].Receipts = []coredata.Receipt{{Namespace: "n", ID: "r"}}
				want = 1
			case "ordinary":
				records[1] = projectorRecord(2, false)
				want = 1
			case "mixed":
				records[1].Mutations = append(records[1].Mutations, projectorRecord(2, false).Mutations[0])
				want = 1
			case "migration":
				records[1].Handler = MigrationHandler
				want = 1
			case "invalid":
				records[1].Mutations[0].Remote.Mutations[0].ID = 999
				want = 1
			case "workers":
				workers = 2
				want = 2
			case "bytes":
				bytes = projectionRecordLogicalBytes(records[0])
				want = 1
			}
			segments := make([]projectionSegment, len(records))
			for i := range records {
				segments[i].records = records[i : i+1]
			}
			if n := remoteProjectionWindow(segments, workers, bytes); n != want {
				t.Fatalf("window=%d want=%d", n, want)
			}
		})
	}
}

func TestRemoteProjectionCheckpointFailureKeepsWindow(t *testing.T) {
	records := []coredata.CommitRecord{remoteProjectionRecord(t, 1), remoteProjectionRecord(t, 2), remoteProjectionRecord(t, 3)}
	var mu sync.Mutex
	calls := 0
	s := &parallelRemoteStore{project: func(context.Context, coredata.CommitRecord) error { mu.Lock(); calls++; mu.Unlock(); return nil }}
	p, w := stoppedProjectorWithRecords(t, s, records, 4<<20)
	p.opts.RemoteProjectionWorkers = 2
	failure := errors.New("checkpoint failed")
	p.ack = func(context.Context, corenest.CommitFence) error { return failure }
	if n, err := p.ReplayPass(context.Background()); n != 2 || !errors.Is(err, failure) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
	assertWALReplayCount(t, w, 3)
}

func TestRemoteProjectionCancellationJoinsWorkers(t *testing.T) {
	records := []coredata.CommitRecord{remoteProjectionRecord(t, 1), remoteProjectionRecord(t, 2)}
	entered := make(chan struct{}, 2)
	s := &parallelRemoteStore{project: func(ctx context.Context, _ coredata.CommitRecord) error {
		entered <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}}
	p, w := stoppedProjectorWithRecords(t, s, records, 4<<20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var err error
	go func() { defer close(done); _, err = p.ReplayPass(ctx) }()
	for range 2 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("remote worker did not enter storage")
		}
	}
	cancel()
	awaitChan(t, done, "cancelled workers")
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	assertWALReplayCount(t, w, 2)
}

func TestRemoteProjectionWithoutCapabilityRemainsSerial(t *testing.T) {
	records := []coredata.CommitRecord{remoteProjectionRecord(t, 1), remoteProjectionRecord(t, 2)}
	store := &recordingSegmentStore{}
	p, _ := stoppedProjectorWithRecords(t, store, records, 4<<20)
	if n, err := p.ReplayPass(context.Background()); n != 2 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if len(store.events) != 2 || store.events[0] != "project:"+records[0].ID.String() || store.events[1] != "project:"+records[1].ID.String() {
		t.Fatal(store.events)
	}
}

func TestRemoteProjectionFatalSuffixIsNotHiddenByEarlierTransientFailure(t *testing.T) {
	records := []coredata.CommitRecord{remoteProjectionRecord(t, 1), remoteProjectionRecord(t, 2), remoteProjectionRecord(t, 3), remoteProjectionRecord(t, 4)}
	s := &parallelRemoteStore{project: func(_ context.Context, r coredata.CommitRecord) error {
		switch r.ID {
		case records[1].ID:
			return context.DeadlineExceeded
		case records[2].ID:
			return ErrProjectionConflict
		}
		return nil
	}}
	p, w := stoppedProjectorWithRecords(t, s, records, 4<<20)
	p.opts.RemoteProjectionWorkers = 3
	ticket := &projectionTicket{done: make(chan struct{})}
	p.tickets[records[3].ID] = ticket
	n, err := p.ReplayPass(context.Background())
	if n != 1 || !errors.Is(err, ErrProjectionConflict) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	awaitChan(t, ticket.Done(), "fatal result for unstarted record")
	if !errors.Is(ticket.Err(), ErrProjectionConflict) {
		t.Fatal(ticket.Err())
	}
	if p.Stats().FatalProjectionConflicts != 1 {
		t.Fatal(p.Stats())
	}
	assertWALReplayCount(t, w, 3)
}

func TestMongoStoreParallelCapabilityRequiresBothAdapters(t *testing.T) {
	store, _, _ := newMongoStoreTest(t)
	if store.SupportsRemoteParallelProjection() {
		t.Fatal("missing remote adapters accepted")
	}
	adapter := &mongoRemoteProjectionFake{}
	if err := store.SetRemoteProjection(adapter, adapter); err != nil {
		t.Fatal(err)
	}
	if store.SupportsRemoteParallelProjection() {
		t.Fatal("legacy adapters must remain serial")
	}
}
