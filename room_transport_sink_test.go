package room

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/cube-core/entity"
	coreentitysync "github.com/tjbdwanghaibo/cube-core/entitysync"
	core "github.com/tjbdwanghaibo/cube-core/statesync"
	kit "github.com/tjbdwanghaibo/cube-kit/nettransport"
)

type recordingAtomicTransport struct {
	err         error
	slowSession core.SessionID
	removed     []core.SessionID
	batches     [][]kit.OutboundFrame
}

type parallelRoomTransport struct {
	firstEntered  chan struct{}
	secondEntered chan struct{}
	releaseFirst  chan struct{}
}

type discardAtomicTransport struct{}

func (discardAtomicTransport) AdmitBatch(context.Context, []kit.OutboundFrame) error { return nil }

func (transport *parallelRoomTransport) AdmitBatch(_ context.Context, frames []kit.OutboundFrame) error {
	if len(frames) != 1 {
		return errors.New("unexpected batch")
	}
	switch frames[0].Session {
	case 1:
		close(transport.firstEntered)
		<-transport.releaseFirst
	case 2:
		close(transport.secondEntered)
	}
	return nil
}

func (transport *recordingAtomicTransport) AdmitBatch(_ context.Context, frames []kit.OutboundFrame) error {
	copyOfFrames := make([]kit.OutboundFrame, len(frames))
	copy(copyOfFrames, frames)
	transport.batches = append(transport.batches, copyOfFrames)
	if transport.slowSession != 0 {
		for _, frame := range frames {
			if frame.Session == transport.slowSession {
				return kit.AdmissionError{Session: transport.slowSession, Err: kit.ErrReliableBackpressure}
			}
		}
	}
	return transport.err
}

func (transport *recordingAtomicTransport) RemoveSession(session core.SessionID) bool {
	transport.removed = append(transport.removed, session)
	return true
}

func TestRoomTransportSinkRoutesLifecycleAndState(t *testing.T) {
	transport := &recordingAtomicTransport{}
	sink, err := NewRoomTransportSink(RoomTransportSinkConfig{
		Transport: transport,
		Sessions: RoomSessionResolverFunc(func(_ context.Context, subscriber coreentitysync.SubscriberRef) (core.SessionID, error) {
			return core.SessionID(subscriber.ID), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	subscriber := coreentitysync.SubscriberRef{ID: 9}
	snapshot := testRoomUpdate(1001, 1, 0, true, []byte("snapshot"))
	if err := sink.AdmitRoomFrames(context.Background(), []RoomFrame{{
		RoomID: 7, Frame: 1, Subscriber: subscriber, SessionSequence: 1,
		Entries: []RoomFrameEntry{{Kind: coreentitysync.EnvelopeSnapshot, Update: snapshot}},
	}}); err != nil {
		t.Fatal(err)
	}
	first := transport.batches[0][0]
	if first.Session != 9 || len(first.Reliable) == 0 || len(first.Datagrams) != 0 {
		t.Fatalf("snapshot was not routed to reliable lane: %+v", first)
	}
	frame, sequence, decoded, err := DecodeRoomWireFrame(first.Reliable, core.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if frame != 1 || sequence != 1 || len(decoded.Objects) != 1 || decoded.Objects[0].Operation != core.ObjectCreate {
		t.Fatalf("unexpected snapshot frame: frame=%d sequence=%d delta=%+v", frame, sequence, decoded)
	}
	ref := decoded.Objects[0].Ref
	decodedUpdate, err := DecodeRoomSubjectUpdate(decoded.Objects[0].Components[0].Data, 0)
	if err != nil {
		t.Fatal(err)
	}
	if decodedUpdate.SubjectID != snapshot.SubjectID || !decodedUpdate.Payload.Equal(snapshot.Payload) || decodedUpdate.Profile != snapshot.Profile.Normalize() {
		t.Fatalf("subject payload mismatch: %+v", decodedUpdate)
	}

	deltaUpdate := testRoomUpdate(1001, 2, 1, false, []byte("delta"))
	if err := sink.AdmitRoomFrames(context.Background(), []RoomFrame{{
		RoomID: 7, Frame: 2, Subscriber: subscriber, SessionSequence: 2,
		Entries: []RoomFrameEntry{{Kind: coreentitysync.EnvelopeDelta, Update: deltaUpdate}},
	}}); err != nil {
		t.Fatal(err)
	}
	second := transport.batches[1][0]
	if len(second.Datagrams) == 0 || len(second.Reliable) != 0 {
		t.Fatalf("delta was not routed to datagram lane: %+v", second)
	}
	reassembler := core.NewReassembler(core.DefaultLimits(), time.Second)
	var wire []byte
	for _, packet := range second.Datagrams {
		payload, complete, _, pushErr := reassembler.PushFor(9, packet, time.Now())
		if pushErr != nil {
			t.Fatal(pushErr)
		}
		if complete {
			wire = payload
		}
	}
	_, _, decoded, err = DecodeRoomWireFrame(wire, core.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Objects) != 1 || decoded.Objects[0].Operation != core.ObjectUpdate || decoded.Objects[0].Ref != ref {
		t.Fatalf("unexpected delta object: %+v", decoded.Objects)
	}

	leave := deltaUpdate
	leave.Payload = entity.FrozenSyncPayload{}
	if err := sink.AdmitRoomFrames(context.Background(), []RoomFrame{{
		RoomID: 7, Frame: 3, Subscriber: subscriber, SessionSequence: 3,
		Entries: []RoomFrameEntry{{Kind: coreentitysync.EnvelopeLeave, Update: leave}},
	}}); err != nil {
		t.Fatal(err)
	}
	third := transport.batches[2][0]
	_, _, decoded, err = DecodeRoomWireFrame(third.Reliable, core.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Objects[0].Operation != core.ObjectRemove || decoded.Objects[0].Ref != ref {
		t.Fatalf("unexpected leave object: %+v", decoded.Objects[0])
	}

	sink.ReleaseRoomSubject(7, 1001)
	newSnapshot := testRoomUpdate(2002, 1, 0, true, []byte("new"))
	if err := sink.AdmitRoomFrames(context.Background(), []RoomFrame{{
		RoomID: 7, Frame: 4, Subscriber: subscriber, SessionSequence: 4,
		Entries: []RoomFrameEntry{{Kind: coreentitysync.EnvelopeSnapshot, Update: newSnapshot}},
	}}); err != nil {
		t.Fatal(err)
	}
	_, _, decoded, err = DecodeRoomWireFrame(transport.batches[3][0].Reliable, core.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	newRef := decoded.Objects[0].Ref
	if newRef.ID != ref.ID || newRef.Generation == ref.Generation {
		t.Fatalf("released ID was not generation-fenced: old=%+v new=%+v", ref, newRef)
	}
}

func TestRoomTransportSinkAdmissionFailureDoesNotCommitBaseline(t *testing.T) {
	transport := &recordingAtomicTransport{err: errors.New("backpressure")}
	sink, err := NewRoomTransportSink(RoomTransportSinkConfig{
		Transport: transport,
		Sessions:  RoomSessionResolverFunc(func(context.Context, coreentitysync.SubscriberRef) (core.SessionID, error) { return 1, nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	frame := RoomFrame{RoomID: 1, Frame: 1, Subscriber: coreentitysync.SubscriberRef{ID: 1}, SessionSequence: 1,
		Entries: []RoomFrameEntry{{Kind: coreentitysync.EnvelopeSnapshot, Update: testRoomUpdate(10, 1, 0, true, []byte("full"))}}}
	if err := sink.AdmitRoomFrames(context.Background(), []RoomFrame{frame}); err == nil {
		t.Fatal("expected admission failure")
	}
	transport.err = nil
	frame.Frame = 2
	frame.SessionSequence = 2
	frame.Entries[0].Kind = coreentitysync.EnvelopeDelta
	frame.Entries[0].Update = testRoomUpdate(10, 2, 1, false, []byte("delta"))
	if err := sink.AdmitRoomFrames(context.Background(), []RoomFrame{frame}); !errors.Is(err, ErrRoomSubjectBaseline) {
		t.Fatalf("failed admission committed object baseline: %v", err)
	}
}

func TestRoomTransportSinkBaselinesAreSessionScoped(t *testing.T) {
	transport := &recordingAtomicTransport{}
	sink, err := NewRoomTransportSink(RoomTransportSinkConfig{
		Transport: transport,
		Sessions: RoomSessionResolverFunc(func(_ context.Context, subscriber coreentitysync.SubscriberRef) (core.SessionID, error) {
			return core.SessionID(subscriber.ID), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := RoomFrame{
		RoomID: 8, Frame: 1, Subscriber: coreentitysync.SubscriberRef{ID: 1}, SessionSequence: 1,
		Entries: []RoomFrameEntry{{Kind: coreentitysync.EnvelopeSnapshot, Update: testRoomUpdate(42, 1, 0, true, []byte("full"))}},
	}
	if err := sink.AdmitRoomFrames(context.Background(), []RoomFrame{snapshot}); err != nil {
		t.Fatal(err)
	}
	delta := RoomFrame{
		RoomID: 8, Frame: 2, Subscriber: coreentitysync.SubscriberRef{ID: 2}, SessionSequence: 1,
		Entries: []RoomFrameEntry{{Kind: coreentitysync.EnvelopeDelta, Update: testRoomUpdate(42, 2, 1, false, []byte("delta"))}},
	}
	if err := sink.AdmitRoomFrames(context.Background(), []RoomFrame{delta}); !errors.Is(err, ErrRoomSubjectBaseline) {
		t.Fatalf("session without snapshot inherited another session baseline: %v", err)
	}
}

func TestRoomTransportSinkEvictsSlowSubscriberWithoutBlockingRoom(t *testing.T) {
	transport := &recordingAtomicTransport{}
	sink, err := NewRoomTransportSink(RoomTransportSinkConfig{
		Transport: transport,
		Sessions: RoomSessionResolverFunc(func(_ context.Context, subscriber coreentitysync.SubscriberRef) (core.SessionID, error) {
			return core.SessionID(subscriber.ID), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	room, err := NewRoomBroadcaster(15, sink)
	if err != nil {
		t.Fatal(err)
	}
	state := testRoomState(601, nil)
	if err := room.RegisterSubject(state); err != nil {
		t.Fatal(err)
	}
	for id := int64(1); id <= 2; id++ {
		if _, err := room.Subscribe(context.Background(), coreentitysync.SubscriberRef{ID: id}, 601, entity.SyncProfile{}); err != nil {
			t.Fatal(err)
		}
	}
	transport.slowSession = 2
	state.MarkDirty(1)
	if err := room.FlushSubject(context.Background(), 601); err != nil {
		t.Fatalf("healthy recipient was blocked by slow subscriber: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for room.Stats().ActiveSubscribers != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if stats := room.Stats(); stats.ActiveSubscribers != 1 {
		t.Fatalf("slow subscriber was not removed: %+v", stats)
	}
	if stats := sink.Stats(); stats.SlowConsumerEvictions != 1 || stats.EvictedRoomSessions != 0 {
		t.Fatalf("sink lifecycle stats = %+v", stats)
	}
	if len(transport.removed) != 1 || transport.removed[0] != 2 {
		t.Fatalf("removed sessions = %+v", transport.removed)
	}
	last := transport.batches[len(transport.batches)-1]
	if len(last) != 1 || last[0].Session != 1 {
		t.Fatalf("retry batch did not isolate healthy session: %+v", last)
	}
}

func TestRoomTransportSinkKeepsIndependentLifecycleHandlersForSharedRooms(t *testing.T) {
	transport := &recordingAtomicTransport{}
	sink, err := NewRoomTransportSink(RoomTransportSinkConfig{
		Transport: transport,
		Sessions: RoomSessionResolverFunc(func(_ context.Context, subscriber coreentitysync.SubscriberRef) (core.SessionID, error) {
			return core.SessionID(subscriber.ID), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sink.Close(context.Background()) }()
	first, err := NewRoomBroadcaster(21, sink)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewRoomBroadcaster(22, sink)
	if err != nil {
		t.Fatal(err)
	}
	firstState := testRoomState(701, nil)
	secondState := testRoomState(702, nil)
	if err := first.RegisterSubject(firstState); err != nil {
		t.Fatal(err)
	}
	if err := second.RegisterSubject(secondState); err != nil {
		t.Fatal(err)
	}
	subscriber := coreentitysync.SubscriberRef{ID: 9}
	if _, err := first.Subscribe(context.Background(), subscriber, 701, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Subscribe(context.Background(), subscriber, 702, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	transport.slowSession = 9
	firstState.MarkDirty(1)
	if err := first.FlushSubject(context.Background(), 701); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for (first.Stats().ActiveSubscribers != 0 || second.Stats().ActiveSubscribers != 0) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if first.Stats().ActiveSubscribers != 0 || second.Stats().ActiveSubscribers != 0 {
		t.Fatalf("shared sink did not notify every room: first=%+v second=%+v", first.Stats(), second.Stats())
	}
}

func TestRoomTransportSinkDoesNotSerializeIndependentRooms(t *testing.T) {
	transport := &parallelRoomTransport{firstEntered: make(chan struct{}), secondEntered: make(chan struct{}), releaseFirst: make(chan struct{})}
	sink, err := NewRoomTransportSink(RoomTransportSinkConfig{
		Transport: transport,
		Sessions: RoomSessionResolverFunc(func(_ context.Context, subscriber coreentitysync.SubscriberRef) (core.SessionID, error) {
			return core.SessionID(subscriber.ID), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sink.Close(context.Background()) }()
	frame := func(roomID, subscriberID, subjectID int64) RoomFrame {
		return RoomFrame{
			RoomID: roomID, Frame: 1, Subscriber: coreentitysync.SubscriberRef{ID: subscriberID}, SessionSequence: 1,
			Entries: []RoomFrameEntry{{Kind: coreentitysync.EnvelopeSnapshot, Update: testRoomUpdate(subjectID, 1, 0, true, []byte("state"))}},
		}
	}
	firstDone := make(chan error, 1)
	go func() { firstDone <- sink.AdmitRoomFrames(context.Background(), []RoomFrame{frame(31, 1, 801)}) }()
	select {
	case <-transport.firstEntered:
	case <-time.After(time.Second):
		t.Fatal("first room did not reach transport")
	}
	secondDone := make(chan error, 1)
	go func() { secondDone <- sink.AdmitRoomFrames(context.Background(), []RoomFrame{frame(32, 2, 802)}) }()
	select {
	case <-transport.secondEntered:
	case <-time.After(200 * time.Millisecond):
		close(transport.releaseFirst)
		t.Fatal("independent room was serialized behind a blocked room")
	}
	close(transport.releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}

func TestRoomTransportSinkBoundsApplicationCallbacks(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	sink, err := NewRoomTransportSink(RoomTransportSinkConfig{
		Transport:       &recordingAtomicTransport{},
		Sessions:        RoomSessionResolverFunc(func(context.Context, coreentitysync.SubscriberRef) (core.SessionID, error) { return 1, nil }),
		CallbackWorkers: 1, CallbackQueueCapacity: 1,
		OnSlowConsumer: func(context.Context, RoomSlowConsumer) {
			select {
			case <-started:
			default:
				close(started)
			}
			<-release
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	event := RoomSlowConsumer{RoomID: 1, Subscriber: coreentitysync.SubscriberRef{ID: 1}, Session: 1, Err: kit.ErrReliableBackpressure}
	sink.dispatchSlowConsumers([]RoomSlowConsumer{event})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("callback worker did not start")
	}
	sink.dispatchSlowConsumers([]RoomSlowConsumer{event, event})
	if stats := sink.Stats(); stats.CallbackPending != 1 || stats.CallbackCoalesced != 1 {
		t.Fatalf("callback mailbox did not coalesce duplicate lifecycle work: %+v", stats)
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := sink.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkRoomTransportSinkAdmit100Subscribers(b *testing.B) {
	sink, err := NewRoomTransportSink(RoomTransportSinkConfig{
		Transport: discardAtomicTransport{},
		Sessions: RoomSessionResolverFunc(func(_ context.Context, subscriber coreentitysync.SubscriberRef) (core.SessionID, error) {
			return core.SessionID(subscriber.ID), nil
		}),
	})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = sink.Close(context.Background()) }()
	frames := make([]RoomFrame, 100)
	for i := range frames {
		frames[i] = RoomFrame{
			RoomID: 1, Frame: 1, Subscriber: coreentitysync.SubscriberRef{ID: int64(i + 1)}, SessionSequence: 1,
			Entries: []RoomFrameEntry{{Kind: coreentitysync.EnvelopeSnapshot, Update: testRoomUpdate(1, 1, 0, true, []byte("snapshot"))}},
		}
	}
	if err := sink.AdmitRoomFrames(context.Background(), frames); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		version := uint64(iteration + 2)
		for i := range frames {
			frames[i].Frame = version
			frames[i].SessionSequence = version
			frames[i].Entries[0] = RoomFrameEntry{Kind: coreentitysync.EnvelopeDelta, Update: testRoomUpdate(1, version, version-1, false, []byte("delta"))}
		}
		if err := sink.AdmitRoomFrames(context.Background(), frames); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRoomReplicationGlobalFrame100x100(b *testing.B) {
	sink, err := NewRoomTransportSink(RoomTransportSinkConfig{
		Transport: discardAtomicTransport{},
		Sessions: RoomSessionResolverFunc(func(_ context.Context, subscriber coreentitysync.SubscriberRef) (core.SessionID, error) {
			return core.SessionID(subscriber.ID), nil
		}),
	})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = sink.Close(context.Background()) }()
	room, err := NewRoomBroadcaster(1, sink)
	if err != nil {
		b.Fatal(err)
	}
	states := make([]*entity.SubjectSyncState, 100)
	for i := range states {
		states[i] = testRoomState(int64(i+1), nil)
		if err := room.RegisterSubject(states[i]); err != nil {
			b.Fatal(err)
		}
	}
	for subscriberID := int64(1); subscriberID <= 100; subscriberID++ {
		subscriber := coreentitysync.SubscriberRef{ID: subscriberID}
		for subjectID := int64(1); subjectID <= 100; subjectID++ {
			if _, err := room.Subscribe(context.Background(), subscriber, subjectID, entity.SyncProfile{}); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, state := range states {
			state.MarkDirty(1)
		}
		if err := room.FlushDirty(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}

func testRoomUpdate(subjectID int64, version, base uint64, full bool, payload []byte) entity.SubjectSyncUpdate {
	return entity.SubjectSyncUpdate{
		SubjectID: subjectID, Namespace: "combat", SubjectKind: 3,
		Profile: entity.SyncProfile{Key: "owner", LOD: 2, SchemaVersion: 7},
		Version: version, BaseVersion: base, Mask: 5, Full: full, Reason: entity.SyncFullReasonDirty,
		Payload: entity.CopyFrozenSyncPayload(11, payload),
	}
}
