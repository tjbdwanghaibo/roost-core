package entitysync

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tjbdwanghaibo/cube-core/entity"
)

type recordingEnvelopeSink struct {
	mu        sync.Mutex
	batches   [][]DeliveryEnvelope
	rejectErr error
}

type discardEnvelopeSink struct{}

func (discardEnvelopeSink) AdmitEnvelopes(context.Context, []DeliveryEnvelope) error { return nil }

func (s *recordingEnvelopeSink) AdmitEnvelopes(_ context.Context, envelopes []DeliveryEnvelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rejectErr != nil {
		return s.rejectErr
	}
	s.batches = append(s.batches, append([]DeliveryEnvelope(nil), envelopes...))
	return nil
}

func (s *recordingEnvelopeSink) snapshot() [][]DeliveryEnvelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]DeliveryEnvelope, len(s.batches))
	for i := range s.batches {
		out[i] = append([]DeliveryEnvelope(nil), s.batches[i]...)
	}
	return out
}

func newSubscriptionTestState(t *testing.T, subjectID int64, packCount *int) *entity.SubjectSyncState {
	t.Helper()
	return entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{
		Enabled: true, SubjectID: subjectID,
		Packer: entity.SubjectSyncPackFunc{
			Snapshot: func(profile entity.SyncProfile) (entity.FrozenSyncPayload, error) {
				*packCount++
				return entity.TakeFrozenSyncPayload(1, []byte("snapshot:"+profile.Key)), nil
			},
			Delta: func(profile entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) {
				*packCount++
				return entity.TakeFrozenSyncPayload(1, []byte("delta:"+profile.Key)), nil
			},
		},
	})
}

func TestSubscriptionCoordinatorSharesProfilePayload(t *testing.T) {
	sink := &recordingEnvelopeSink{}
	coordinator := NewSubscriptionCoordinator(sink)
	packCount := 0
	state := newSubscriptionTestState(t, 1001, &packCount)
	profile := entity.SyncProfile{Key: "near", LOD: 1}
	for id := int64(1); id <= 20; id++ {
		if _, err := coordinator.Subscribe(context.Background(), SubscriberRef{Kind: SubscriberKindPlayer, ID: id}, state, profile); err != nil {
			t.Fatal(err)
		}
	}
	packCount = 0
	state.MarkDirty(7)
	prepared, err := state.Prepare([]entity.SyncProfile{profile})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Distribute(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	if packCount != 1 {
		t.Fatalf("profile payload packed %d times for 20 subscribers", packCount)
	}
	batches := sink.snapshot()
	last := batches[len(batches)-1]
	if len(last) != 20 || last[0].Kind != EnvelopeDelta {
		t.Fatalf("unexpected distributed batch: len=%d kind=%d", len(last), last[0].Kind)
	}
	if state.Version() != 1 || state.PendingDirty() {
		t.Fatalf("content state was not committed: version=%d dirty=%v", state.Version(), state.PendingDirty())
	}
}

func TestSubscriptionAdmissionFailureRollsBack(t *testing.T) {
	wantErr := errors.New("queue full")
	sink := &recordingEnvelopeSink{rejectErr: wantErr}
	coordinator := NewSubscriptionCoordinator(sink)
	packCount := 0
	state := newSubscriptionTestState(t, 1002, &packCount)
	subscriber := SubscriberRef{Kind: SubscriberKindPlayer, ID: 11}
	if _, err := coordinator.Subscribe(context.Background(), subscriber, state, entity.SyncProfile{Key: "default"}); !errors.Is(err, wantErr) {
		t.Fatalf("Subscribe error=%v", err)
	}
	if _, ok := coordinator.Get(subscriber, state.SubjectID()); ok {
		t.Fatal("failed subscription remained active")
	}

	sink.rejectErr = nil
	if _, err := coordinator.Subscribe(context.Background(), subscriber, state, entity.SyncProfile{Key: "default"}); err != nil {
		t.Fatal(err)
	}
	state.MarkDirty(1)
	prepared, err := state.Prepare(nil)
	if err != nil {
		t.Fatal(err)
	}
	sink.rejectErr = wantErr
	if err := coordinator.Distribute(context.Background(), prepared); !errors.Is(err, wantErr) {
		t.Fatalf("Distribute error=%v", err)
	}
	if state.Version() != 0 || !state.PendingDirty() {
		t.Fatalf("failed distribution committed state: version=%d dirty=%v", state.Version(), state.PendingDirty())
	}
}

func TestSubscriptionUnsubscribeFailureRestoresActive(t *testing.T) {
	sink := &recordingEnvelopeSink{}
	coordinator := NewSubscriptionCoordinator(sink)
	packCount := 0
	state := newSubscriptionTestState(t, 1003, &packCount)
	subscriber := SubscriberRef{Kind: SubscriberKindPlayer, ID: 12}
	if _, err := coordinator.Subscribe(context.Background(), subscriber, state, entity.SyncProfile{Key: "default"}); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("leave rejected")
	sink.rejectErr = wantErr
	if err := coordinator.Unsubscribe(context.Background(), subscriber, state.SubjectID()); !errors.Is(err, wantErr) {
		t.Fatalf("Unsubscribe error=%v", err)
	}
	got, ok := coordinator.Get(subscriber, state.SubjectID())
	if !ok || got.State != SubscriptionActive {
		t.Fatalf("subscription not restored: %+v ok=%v", got, ok)
	}
	sink.rejectErr = nil
	if err := coordinator.Unsubscribe(context.Background(), subscriber, state.SubjectID()); err != nil {
		t.Fatal(err)
	}
	if _, ok := coordinator.Get(subscriber, state.SubjectID()); ok {
		t.Fatal("subscription remained after admitted leave")
	}
}

func TestSubscriptionCoordinatorFlushSubjectAndContainsSinkPanic(t *testing.T) {
	packCount := 0
	state := newSubscriptionTestState(t, 1004, &packCount)
	panicking := ReliableEnvelopeSinkFunc(func(context.Context, []DeliveryEnvelope) error {
		panic("transport bug")
	})
	coordinator := NewSubscriptionCoordinator(panicking)
	subscriber := SubscriberRef{Kind: SubscriberKindPlayer, ID: 13}
	if _, err := coordinator.Subscribe(context.Background(), subscriber, state, entity.SyncProfile{Key: "default"}); !errors.Is(err, ErrEnvelopeAdmission) {
		t.Fatalf("sink panic was not contained: %v", err)
	}

	sink := &recordingEnvelopeSink{}
	coordinator.SetSink(sink)
	if _, err := coordinator.Subscribe(context.Background(), subscriber, state, entity.SyncProfile{Key: "default"}); err != nil {
		t.Fatal(err)
	}
	state.MarkDirty(1)
	if err := coordinator.FlushSubject(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if state.Version() != 1 || state.PendingDirty() {
		t.Fatalf("FlushSubject did not commit: version=%d dirty=%v", state.Version(), state.PendingDirty())
	}
}

func TestSubscriptionCoordinatorGatesFlushOnDurableWatermark(t *testing.T) {
	// Externalization gate for pipelined commits: a subject whose newest
	// commit LSN is above the durable watermark must not be distributed —
	// dirty state is preserved and the next tick retries.
	sink := &recordingEnvelopeSink{}
	coordinator := NewSubscriptionCoordinator(sink)
	packCount := 0
	state := newSubscriptionTestState(t, 1006, &packCount)
	subscriber := SubscriberRef{Kind: SubscriberKindPlayer, ID: 15}
	if _, err := coordinator.Subscribe(context.Background(), subscriber, state, entity.SyncProfile{Key: "default"}); err != nil {
		t.Fatal(err)
	}

	var watermark atomic.Uint64
	coordinator.SetDurableWatermark(watermark.Load)

	state.MarkDirty(1)
	state.SetLastCommitLSN(7) // enqueued but not durable yet (watermark 0)
	if err := coordinator.FlushSubject(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if state.Version() != 0 || !state.PendingDirty() {
		t.Fatalf("non-durable state escaped the gate: version=%d dirty=%v", state.Version(), state.PendingDirty())
	}

	watermark.Store(7) // fsync caught up
	if err := coordinator.FlushSubject(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if state.Version() != 1 || state.PendingDirty() {
		t.Fatalf("durable state was not flushed: version=%d dirty=%v", state.Version(), state.PendingDirty())
	}

	// Removing the gate restores ungated behavior.
	coordinator.SetDurableWatermark(nil)
	state.MarkDirty(2)
	state.SetLastCommitLSN(99)
	if err := coordinator.FlushSubject(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if state.Version() != 2 {
		t.Fatalf("ungated flush did not run: version=%d", state.Version())
	}
}

func TestSubscriptionCoordinatorCloseIsTerminal(t *testing.T) {
	sink := &recordingEnvelopeSink{}
	coordinator := NewSubscriptionCoordinator(sink)
	packCount := 0
	state := newSubscriptionTestState(t, 1005, &packCount)
	subscriber := SubscriberRef{Kind: SubscriberKindPlayer, ID: 14}
	profile := entity.SyncProfile{Key: "default"}
	if _, err := coordinator.Subscribe(context.Background(), subscriber, state, profile); err != nil {
		t.Fatal(err)
	}

	state.MarkDirty(1)
	prepared, err := state.Prepare([]entity.SyncProfile{profile})
	if err != nil {
		t.Fatal(err)
	}
	coordinator.Close()
	if _, ok := coordinator.Get(subscriber, state.SubjectID()); ok {
		t.Fatal("Close retained subscription membership")
	}
	if _, err := coordinator.Subscribe(context.Background(), subscriber, state, profile); !errors.Is(err, ErrCoordinatorClosed) {
		t.Fatalf("Subscribe after Close error=%v", err)
	}
	if err := coordinator.Unsubscribe(context.Background(), subscriber, state.SubjectID()); !errors.Is(err, ErrCoordinatorClosed) {
		t.Fatalf("Unsubscribe after Close error=%v", err)
	}
	if err := coordinator.Distribute(context.Background(), prepared); !errors.Is(err, ErrCoordinatorClosed) {
		t.Fatalf("Distribute after Close error=%v", err)
	}
	if state.Version() != 0 || !state.PendingDirty() {
		t.Fatalf("closed coordinator committed prepared state: version=%d dirty=%v", state.Version(), state.PendingDirty())
	}
	coordinator.SetSink(sink)
	if _, err := coordinator.Subscribe(context.Background(), subscriber, state, profile); !errors.Is(err, ErrCoordinatorClosed) {
		t.Fatalf("SetSink reopened coordinator: %v", err)
	}
}

func TestSubscriptionCoordinatorDistributeBatchIsAtomic(t *testing.T) {
	sink := &recordingEnvelopeSink{}
	coordinator := NewSubscriptionCoordinator(sink)
	packCount := 0
	first := newSubscriptionTestState(t, 1101, &packCount)
	second := newSubscriptionTestState(t, 1102, &packCount)
	profile := entity.SyncProfile{Key: "default"}
	for id := int64(1); id <= 2; id++ {
		subscriber := SubscriberRef{Kind: SubscriberKindPlayer, ID: id}
		if _, err := coordinator.Subscribe(context.Background(), subscriber, first, profile); err != nil {
			t.Fatal(err)
		}
		if _, err := coordinator.Subscribe(context.Background(), subscriber, second, profile); err != nil {
			t.Fatal(err)
		}
	}
	sink.mu.Lock()
	sink.batches = nil
	sink.mu.Unlock()
	first.MarkDirty(1)
	second.MarkDirty(2)
	firstPrepared, err := first.Prepare([]entity.SyncProfile{profile})
	if err != nil {
		t.Fatal(err)
	}
	secondPrepared, err := second.Prepare([]entity.SyncProfile{profile})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.DistributeBatch(context.Background(), []*entity.PreparedSubjectSync{secondPrepared, firstPrepared}); err != nil {
		t.Fatal(err)
	}
	batches := sink.snapshot()
	if len(batches) != 1 || len(batches[0]) != 4 {
		t.Fatalf("batch admission = %+v", batches)
	}
	if first.Version() != 1 || second.Version() != 1 || first.PendingDirty() || second.PendingDirty() {
		t.Fatalf("batch was not committed: first=(%d,%v) second=(%d,%v)", first.Version(), first.PendingDirty(), second.Version(), second.PendingDirty())
	}

	wantErr := errors.New("batch full")
	sink.rejectErr = wantErr
	first.MarkDirty(4)
	second.MarkDirty(8)
	firstPrepared, err = first.Prepare([]entity.SyncProfile{profile})
	if err != nil {
		t.Fatal(err)
	}
	secondPrepared, err = second.Prepare([]entity.SyncProfile{profile})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.DistributeBatch(context.Background(), []*entity.PreparedSubjectSync{firstPrepared, secondPrepared}); !errors.Is(err, wantErr) {
		t.Fatalf("DistributeBatch error=%v", err)
	}
	if first.Version() != 1 || second.Version() != 1 || !first.PendingDirty() || !second.PendingDirty() {
		t.Fatalf("failed batch partially committed: first=(%d,%v) second=(%d,%v)", first.Version(), first.PendingDirty(), second.Version(), second.PendingDirty())
	}
}

func TestSubscriptionCoordinatorRejectsFinishedBatchBeforeAdmission(t *testing.T) {
	sink := &recordingEnvelopeSink{}
	coordinator := NewSubscriptionCoordinator(sink)
	packCount := 0
	first := newSubscriptionTestState(t, 1201, &packCount)
	second := newSubscriptionTestState(t, 1202, &packCount)
	profile := entity.SyncProfile{Key: "default"}
	subscriber := SubscriberRef{Kind: SubscriberKindPlayer, ID: 1}
	if _, err := coordinator.Subscribe(context.Background(), subscriber, first, profile); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Subscribe(context.Background(), subscriber, second, profile); err != nil {
		t.Fatal(err)
	}
	sink.mu.Lock()
	sink.batches = nil
	sink.mu.Unlock()
	first.MarkDirty(1)
	second.MarkDirty(2)
	firstPrepared, err := first.Prepare(nil)
	if err != nil {
		t.Fatal(err)
	}
	secondPrepared, err := second.Prepare(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := secondPrepared.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.DistributeBatch(context.Background(), []*entity.PreparedSubjectSync{firstPrepared, secondPrepared}); !errors.Is(err, entity.ErrSubjectSyncFinished) {
		t.Fatalf("DistributeBatch error=%v", err)
	}
	if len(sink.snapshot()) != 0 {
		t.Fatal("invalid prepared batch reached durable sink")
	}
	if first.Version() != 0 || !first.PendingDirty() || second.Version() != 1 {
		t.Fatalf("invalid batch changed state: first=(%d,%v) second=%d", first.Version(), first.PendingDirty(), second.Version())
	}
}

func TestSubscriptionCoordinatorCommitRemainsValidWhenStateClosesAfterAdmission(t *testing.T) {
	packCount := 0
	state := newSubscriptionTestState(t, 1203, &packCount)
	coordinator := NewSubscriptionCoordinator(discardEnvelopeSink{})
	profile := entity.SyncProfile{Key: "default"}
	if _, err := coordinator.Subscribe(context.Background(), SubscriberRef{Kind: SubscriberKindPlayer, ID: 1}, state, profile); err != nil {
		t.Fatal(err)
	}
	coordinator.SetSink(ReliableEnvelopeSinkFunc(func(context.Context, []DeliveryEnvelope) error {
		state.Close()
		return nil
	}))
	state.MarkDirty(1)
	prepared, err := state.Prepare(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Distribute(context.Background(), prepared); err != nil {
		t.Fatalf("admitted update failed to commit during Close: %v", err)
	}
	if state.Version() != 1 || state.Enabled() {
		t.Fatalf("closed state version=%d enabled=%v", state.Version(), state.Enabled())
	}
}

func BenchmarkSubscriptionCoordinatorSharedPayload100Subscribers(b *testing.B) {
	coordinator := NewSubscriptionCoordinator(discardEnvelopeSink{})
	state := entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{
		Enabled: true, SubjectID: 2001,
		Packer: entity.SubjectSyncPackFunc{
			Snapshot: func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
				return entity.TakeFrozenSyncPayload(1, make([]byte, 1024)), nil
			},
			Delta: func(entity.SyncProfile, uint64) (entity.FrozenSyncPayload, error) {
				return entity.TakeFrozenSyncPayload(1, make([]byte, 1024)), nil
			},
		},
	})
	profile := entity.SyncProfile{Key: "near", LOD: 1}
	for id := int64(1); id <= 100; id++ {
		if _, err := coordinator.Subscribe(context.Background(), SubscriberRef{Kind: SubscriberKindPlayer, ID: id}, state, profile); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		state.MarkDirty(1)
		prepared, err := state.Prepare([]entity.SyncProfile{profile})
		if err != nil {
			b.Fatal(err)
		}
		if err := coordinator.Distribute(context.Background(), prepared); err != nil {
			b.Fatal(err)
		}
	}
}
