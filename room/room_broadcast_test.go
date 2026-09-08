package room

import (
	"context"
	"errors"
	"fmt"
	stdsync "sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	coreentitysync "github.com/tjbdwanghaibo/roost-core/entitysync"
)

type recordingRoomFrameSink struct {
	mu      stdsync.Mutex
	batches [][]RoomFrame
	fail    bool
	panic   bool
}

func (s *recordingRoomFrameSink) AdmitRoomFrames(_ context.Context, frames []RoomFrame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.panic {
		panic("room frame sink panic")
	}
	if s.fail {
		return errors.New("rejected")
	}
	batch := make([]RoomFrame, len(frames))
	for i, frame := range frames {
		batch[i] = frame
		batch[i].Entries = append([]RoomFrameEntry(nil), frame.Entries...)
	}
	s.batches = append(s.batches, batch)
	return nil
}

func (s *recordingRoomFrameSink) setFail(fail bool) {
	s.mu.Lock()
	s.fail = fail
	s.mu.Unlock()
}

func (s *recordingRoomFrameSink) reset() {
	s.mu.Lock()
	s.batches = nil
	s.mu.Unlock()
}

func (s *recordingRoomFrameSink) lastBatch() []RoomFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.batches) == 0 {
		return nil
	}
	return append([]RoomFrame(nil), s.batches[len(s.batches)-1]...)
}

func testRoomState(subjectID int64, deltaPacks *atomic.Int64) *entity.SubjectSyncState {
	return entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{
		Enabled: true, SubjectID: subjectID,
		Packer: entity.SubjectSyncPackFunc{
			Snapshot: func(profile entity.SyncProfile) (entity.FrozenSyncPayload, error) {
				return entity.CopyFrozenSyncPayload(1, []byte("snapshot:"+profile.Key)), nil
			},
			Delta: func(profile entity.SyncProfile, mask uint64) (entity.FrozenSyncPayload, error) {
				if deltaPacks != nil {
					deltaPacks.Add(1)
				}
				return entity.CopyFrozenSyncPayload(1, []byte(fmt.Sprintf("delta:%s:%d", profile.Key, mask))), nil
			},
		},
	})
}

func TestRoomReplicationSharesProfilePayloadAndFrames(t *testing.T) {
	recorder := &recordingRoomFrameSink{}
	room, err := NewRoomBroadcaster(77, recorder)
	if err != nil {
		t.Fatal(err)
	}
	var deltaPacks atomic.Int64
	state := testRoomState(101, &deltaPacks)
	if err := room.RegisterSubject(state); err != nil {
		t.Fatal(err)
	}
	profile := entity.SyncProfile{Key: "near", LOD: 1, SchemaVersion: 3}
	for i := int64(1); i <= 20; i++ {
		subscriber := coreentitysync.SubscriberRef{Kind: coreentitysync.SubscriberKindPlayer, ID: i}
		if _, err := room.Subscribe(context.Background(), subscriber, 101, profile); err != nil {
			t.Fatalf("subscribe %d: %v", i, err)
		}
	}
	recorder.reset()
	state.MarkDirty(0x04)
	if err := room.FlushSubject(context.Background(), 101); err != nil {
		t.Fatal(err)
	}
	if got := deltaPacks.Load(); got != 1 {
		t.Fatalf("delta packed %d times, want once per profile", got)
	}
	frames := recorder.lastBatch()
	if len(frames) != 20 {
		t.Fatalf("got %d frames, want 20", len(frames))
	}
	for i, frame := range frames {
		if frame.RoomID != 77 || frame.Frame != 21 || frame.SessionSequence != 2 {
			t.Fatalf("frame %d counters: %+v", i, frame)
		}
		if len(frame.Entries) != 1 || frame.Entries[0].Kind != coreentitysync.EnvelopeDelta {
			t.Fatalf("frame %d entries: %+v", i, frame.Entries)
		}
		if got := string(frame.Entries[0].Update.Payload.BytesCopy()); got != "delta:near:4" {
			t.Fatalf("frame %d payload %q", i, got)
		}
	}
	if state.Version() != 1 || state.PendingDirty() {
		t.Fatalf("state version=%d dirty=%v", state.Version(), state.PendingDirty())
	}
}

func TestRoomReplicationFlushDirtyBuildsOneGlobalFrame(t *testing.T) {
	recorder := &recordingRoomFrameSink{}
	room, err := NewRoomBroadcaster(78, recorder)
	if err != nil {
		t.Fatal(err)
	}
	first := testRoomState(111, nil)
	second := testRoomState(222, nil)
	if err := room.RegisterSubject(first); err != nil {
		t.Fatal(err)
	}
	if err := room.RegisterSubject(second); err != nil {
		t.Fatal(err)
	}
	subscriber := coreentitysync.SubscriberRef{Kind: coreentitysync.SubscriberKindPlayer, ID: 1}
	if _, err := room.Subscribe(context.Background(), subscriber, 111, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	if _, err := room.Subscribe(context.Background(), subscriber, 222, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	recorder.reset()
	first.MarkDirty(1)
	second.MarkDirty(2)
	if err := room.FlushDirty(context.Background()); err != nil {
		t.Fatal(err)
	}
	frames := recorder.lastBatch()
	if len(frames) != 1 || len(frames[0].Entries) != 2 {
		t.Fatalf("global room frame = %+v", frames)
	}
	if frames[0].Entries[0].Update.SubjectID != 111 || frames[0].Entries[1].Update.SubjectID != 222 {
		t.Fatalf("global room frame order = %+v", frames[0].Entries)
	}
	if first.Version() != 1 || second.Version() != 1 || first.PendingDirty() || second.PendingDirty() {
		t.Fatalf("global frame did not commit every subject")
	}
}

func TestRoomReplicationAdmissionRollbackHasNoSequenceGap(t *testing.T) {
	recorder := &recordingRoomFrameSink{}
	room, err := NewRoomBroadcaster(8, recorder)
	if err != nil {
		t.Fatal(err)
	}
	state := testRoomState(201, nil)
	if err := room.RegisterSubject(state); err != nil {
		t.Fatal(err)
	}
	subscriber := coreentitysync.SubscriberRef{Kind: coreentitysync.SubscriberKindPlayer, ID: 9}
	if _, err := room.Subscribe(context.Background(), subscriber, 201, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	recorder.reset()
	recorder.setFail(true)
	state.MarkDirty(1)
	if err := room.FlushSubject(context.Background(), 201); !errors.Is(err, ErrRoomFrameAdmission) {
		t.Fatalf("flush error = %v", err)
	}
	if state.Version() != 0 || !state.PendingDirty() {
		t.Fatalf("failed admission changed state: version=%d dirty=%v", state.Version(), state.PendingDirty())
	}
	recorder.setFail(false)
	if err := room.FlushSubject(context.Background(), 201); err != nil {
		t.Fatal(err)
	}
	frames := recorder.lastBatch()
	if len(frames) != 1 || frames[0].Frame != 2 || frames[0].SessionSequence != 2 {
		t.Fatalf("retry counters have a gap: %+v", frames)
	}
	stats := room.Stats()
	if stats.FailedBatches != 1 || stats.AdmittedFrames != 2 || stats.AdmittedEntries != 2 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestRoomReplicationUnregisterRequiresNoSubscribers(t *testing.T) {
	recorder := &recordingRoomFrameSink{}
	room, err := NewRoomBroadcaster(1, recorder)
	if err != nil {
		t.Fatal(err)
	}
	state := testRoomState(301, nil)
	if err := room.RegisterSubject(state); err != nil {
		t.Fatal(err)
	}
	subscriber := coreentitysync.SubscriberRef{Kind: coreentitysync.SubscriberKindPlayer, ID: 2}
	if _, err := room.Subscribe(context.Background(), subscriber, 301, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	if err := room.UnregisterSubject(301); !errors.Is(err, ErrRoomSubjectHasSubscribers) {
		t.Fatalf("unregister error = %v", err)
	}
	if err := room.Unsubscribe(context.Background(), subscriber, 301); err != nil {
		t.Fatal(err)
	}
	if err := room.UnregisterSubject(301); err != nil {
		t.Fatal(err)
	}
	if err := room.FlushSubject(context.Background(), 301); !errors.Is(err, ErrRoomSubjectNotRegistered) {
		t.Fatalf("flush error = %v", err)
	}
}

func TestRoomReplicationEnforcesRoomLimitsAndReleasesSequenceState(t *testing.T) {
	recorder := &recordingRoomFrameSink{}
	room, err := NewRoomBroadcaster(14, recorder, RoomBroadcasterConfig{MaxSubjects: 1, MaxSubscribers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := room.RegisterSubject(testRoomState(501, nil)); err != nil {
		t.Fatal(err)
	}
	if err := room.RegisterSubject(testRoomState(502, nil)); !errors.Is(err, ErrRoomSubjectLimit) {
		t.Fatalf("subject limit error = %v", err)
	}
	first := coreentitysync.SubscriberRef{Kind: coreentitysync.SubscriberKindPlayer, ID: 1}
	second := coreentitysync.SubscriberRef{Kind: coreentitysync.SubscriberKindPlayer, ID: 2}
	if _, err := room.Subscribe(context.Background(), first, 501, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	if _, err := room.Subscribe(context.Background(), second, 501, entity.SyncProfile{}); !errors.Is(err, ErrRoomSubscriberLimit) {
		t.Fatalf("subscriber limit error = %v", err)
	}
	if stats := room.Stats(); stats.ActiveSubjects != 1 || stats.ActiveSubscribers != 1 || stats.SessionSequences != 1 {
		t.Fatalf("active stats = %+v", stats)
	}
	if err := room.Unsubscribe(context.Background(), first, 501); err != nil {
		t.Fatal(err)
	}
	if stats := room.Stats(); stats.ActiveSubscribers != 0 || stats.SessionSequences != 0 {
		t.Fatalf("subscriber lifecycle leaked state: %+v", stats)
	}
}

func TestRoomEnvelopeSinkContainsDownstreamPanic(t *testing.T) {
	recorder := &recordingRoomFrameSink{panic: true}
	sink := NewRoomEnvelopeSink(recorder)
	if err := sink.RegisterSubject(1, 99); err != nil {
		t.Fatal(err)
	}
	err := sink.AdmitEnvelopes(context.Background(), []coreentitysync.DeliveryEnvelope{{
		Subscriber: coreentitysync.SubscriberRef{Kind: coreentitysync.SubscriberKindPlayer, ID: 1},
		Kind:       coreentitysync.EnvelopeSnapshot,
		Update:     entity.SubjectSyncUpdate{SubjectID: 99},
	}})
	if !errors.Is(err, ErrRoomFrameAdmission) {
		t.Fatalf("panic error = %v", err)
	}
	if stats := sink.Stats(); stats.FailedBatches != 1 || stats.AdmittedFrames != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestRoomReplicationConcurrentSubjects(t *testing.T) {
	recorder := &recordingRoomFrameSink{}
	room, err := NewRoomBroadcaster(9, recorder)
	if err != nil {
		t.Fatal(err)
	}
	const subjects = 16
	for i := int64(1); i <= subjects; i++ {
		state := testRoomState(1000+i, nil)
		if err := room.RegisterSubject(state); err != nil {
			t.Fatal(err)
		}
		subscriber := coreentitysync.SubscriberRef{Kind: coreentitysync.SubscriberKindPlayer, ID: i}
		if _, err := room.Subscribe(context.Background(), subscriber, state.SubjectID(), entity.SyncProfile{}); err != nil {
			t.Fatal(err)
		}
	}
	var wg stdsync.WaitGroup
	for i := int64(1); i <= subjects; i++ {
		subjectID := 1000 + i
		wg.Add(1)
		go func() {
			defer wg.Done()
			state, err := room.subject(subjectID)
			if err != nil {
				t.Errorf("subject: %v", err)
				return
			}
			state.MarkDirty(1)
			if err := room.FlushSubject(context.Background(), subjectID); err != nil {
				t.Errorf("flush: %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestRoomReplicationSchedulesDirtyAtConfiguredRate(t *testing.T) {
	recorder := &recordingRoomFrameSink{}
	room, err := NewRoomBroadcaster(10, recorder)
	if err != nil {
		t.Fatal(err)
	}
	state := testRoomState(401, nil)
	if err := room.RegisterSubject(state); err != nil {
		t.Fatal(err)
	}
	subscriber := coreentitysync.SubscriberRef{Kind: coreentitysync.SubscriberKindPlayer, ID: 3}
	if _, err := room.Subscribe(context.Background(), subscriber, 401, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	recorder.reset()
	if err := room.Start(context.Background(), 5*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	state.MarkDirty(2)
	deadline := time.Now().Add(time.Second)
	for len(recorder.lastBatch()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := room.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	frames := recorder.lastBatch()
	if len(frames) != 1 || frames[0].Entries[0].Update.Mask != 2 {
		t.Fatalf("scheduled frames = %+v", frames)
	}
	state.MarkDirty(4)
	if room.Stats().PendingSubjects != 0 {
		t.Fatal("stopped room retained dirty notification")
	}
}

func TestRoomReplicationQueuesDirtyMarkedBeforeRegistration(t *testing.T) {
	recorder := &recordingRoomFrameSink{}
	room, err := NewRoomBroadcaster(11, recorder)
	if err != nil {
		t.Fatal(err)
	}
	state := testRoomState(402, nil)
	state.MarkDirty(8)
	if err := room.RegisterSubject(state); err != nil {
		t.Fatal(err)
	}
	if got := room.Stats().PendingSubjects; got != 1 {
		t.Fatalf("pending subjects = %d, want 1", got)
	}
	if err := room.FlushDirty(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state.PendingDirty() || room.Stats().PendingSubjects != 0 {
		t.Fatal("flush without subscribers did not commit dirty state")
	}
}

func TestRoomReplicationRetriesRetirementUntilLeaveAdmitted(t *testing.T) {
	recorder := &recordingRoomFrameSink{}
	room, err := NewRoomBroadcaster(12, recorder)
	if err != nil {
		t.Fatal(err)
	}
	state := testRoomState(403, nil)
	if err := room.RegisterSubject(state); err != nil {
		t.Fatal(err)
	}
	subscriber := coreentitysync.SubscriberRef{Kind: coreentitysync.SubscriberKindPlayer, ID: 4}
	if _, err := room.Subscribe(context.Background(), subscriber, 403, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	recorder.reset()
	recorder.setFail(true)
	if err := room.RetireSubject(context.Background(), 403); !errors.Is(err, ErrRoomFrameAdmission) {
		t.Fatalf("retire error = %v", err)
	}
	if stats := room.Stats(); stats.PendingRetirements != 1 {
		t.Fatalf("retirement was lost: %+v", stats)
	}
	if _, err := room.Subscribe(context.Background(), subscriber, 403, entity.SyncProfile{}); !errors.Is(err, ErrRoomSubjectRetiring) {
		t.Fatalf("subscribe while retiring error = %v", err)
	}
	recorder.setFail(false)
	if err := room.FlushDirty(context.Background()); err != nil {
		t.Fatal(err)
	}
	frames := recorder.lastBatch()
	if len(frames) != 1 || len(frames[0].Entries) != 1 || frames[0].Entries[0].Kind != coreentitysync.EnvelopeLeave {
		t.Fatalf("leave frames = %+v", frames)
	}
	if stats := room.Stats(); stats.PendingRetirements != 0 {
		t.Fatalf("retirement remained queued: %+v", stats)
	}
	if err := room.FlushSubject(context.Background(), 403); !errors.Is(err, ErrRoomSubjectNotRegistered) {
		t.Fatalf("retired subject flush error = %v", err)
	}
}

func TestRoomReplicationStopReturnsFinalAdmissionError(t *testing.T) {
	recorder := &recordingRoomFrameSink{}
	room, err := NewRoomBroadcaster(13, recorder)
	if err != nil {
		t.Fatal(err)
	}
	state := testRoomState(404, nil)
	if err := room.RegisterSubject(state); err != nil {
		t.Fatal(err)
	}
	subscriber := coreentitysync.SubscriberRef{Kind: coreentitysync.SubscriberKindPlayer, ID: 5}
	if _, err := room.Subscribe(context.Background(), subscriber, 404, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	if err := room.Start(context.Background(), 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	recorder.setFail(true)
	if err := room.RetireSubject(context.Background(), 404); err == nil {
		t.Fatal("retire unexpectedly succeeded")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := room.Stop(ctx); !errors.Is(err, ErrRoomFrameAdmission) {
		t.Fatalf("stop error = %v", err)
	}
	if room.Stats().LastError == "" {
		t.Fatal("last error was not observable")
	}
}

func BenchmarkRoomReplicationFlush100Subscribers(b *testing.B) {
	sink := ReliableRoomFrameSinkFunc(func(context.Context, []RoomFrame) error { return nil })
	room, err := NewRoomBroadcaster(1, sink)
	if err != nil {
		b.Fatal(err)
	}
	state := testRoomState(1, nil)
	if err := room.RegisterSubject(state); err != nil {
		b.Fatal(err)
	}
	for i := int64(1); i <= 100; i++ {
		subscriber := coreentitysync.SubscriberRef{Kind: coreentitysync.SubscriberKindPlayer, ID: i}
		if _, err := room.Subscribe(context.Background(), subscriber, 1, entity.SyncProfile{}); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		state.MarkDirty(1)
		if err := room.FlushSubject(context.Background(), 1); err != nil {
			b.Fatal(err)
		}
	}
}
