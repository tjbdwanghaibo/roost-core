package room

import (
	"bytes"
	"context"
	"fmt"
	stdsync "sync"
	"testing"

	coreentitysync "github.com/tjbdwanghaibo/roost-core/entitysync"
	kit "github.com/tjbdwanghaibo/roost-core/nettransport"
	core "github.com/tjbdwanghaibo/roost-core/statesync"
)

// RR-20260920-02：一次 delta 是"相对上一帧的改动"，不是"最新的完整状态"。
//
// 房间在 delta 入队成功之后就提交了这一批、清掉了 dirty；传输层如果之后把这一帧
// 丢掉，没有任何人会再产生它。而 latest-only 的 datagram 通道正是"同 stream 只留
// 最后一帧"——第二帧不是第一帧的超集，于是第一帧独有的字段（实跑里是 pos_x /
// equipment）对那个客户端永久消失，服务端全程没有任何错误。
//
// 所以普通 delta 必须走可靠通道：队列上限和 slow-consumer 驱逐把背压**说出来**，
// 而不是安静地少发一个字段。
func TestOrdinaryDeltasGoOnALaneThatDoesNotDropThem(t *testing.T) {
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
	// A snapshot first, so the session has a baseline and what follows is an
	// ordinary delta rather than a lifecycle frame.
	if err := sink.AdmitRoomFrames(context.Background(), []RoomFrame{{
		RoomID: 7, Frame: 1, Subscriber: subscriber, SessionSequence: 1,
		Entries: []RoomFrameEntry{{Kind: coreentitysync.EnvelopeSnapshot, Update: testRoomUpdate(1001, 1, 0, true, []byte("snapshot"))}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := sink.AdmitRoomFrames(context.Background(), []RoomFrame{{
		RoomID: 7, Frame: 2, Subscriber: subscriber, SessionSequence: 2,
		Entries: []RoomFrameEntry{{Kind: coreentitysync.EnvelopeDelta, Update: testRoomUpdate(1001, 2, 1, false, []byte("pos_x"))}},
	}}); err != nil {
		t.Fatal(err)
	}
	delta := transport.batches[1][0]
	if len(delta.Datagrams) != 0 {
		t.Fatalf("an ordinary delta was put on the latest-only datagram lane, where the next delta replaces it: %d datagram(s)", len(delta.Datagrams))
	}
	if len(delta.Reliable) == 0 {
		t.Fatal("an ordinary delta was not queued on a lane that keeps it")
	}
}

// 同一个 session 连续两帧 delta，通过真正的 AsyncTransport：两帧都必须到达下游。
// 这是上面那条规则的行为版本——它不看走了哪条通道，只看"第一帧独有的内容有没有
// 活下来"。
func TestTwoConsecutiveDeltasBothReachTheClient(t *testing.T) {
	downstream := &collectingTransport{}
	async, err := kit.NewAsyncTransport(downstream, kit.DefaultAsyncTransportConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = async.Close(context.Background()) })
	if err := async.RegisterSession(core.SessionInfo{ID: 9}); err != nil {
		t.Fatal(err)
	}
	sink, err := NewRoomTransportSink(RoomTransportSinkConfig{
		Transport: async,
		Sessions: RoomSessionResolverFunc(func(_ context.Context, subscriber coreentitysync.SubscriberRef) (core.SessionID, error) {
			return core.SessionID(subscriber.ID), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	subscriber := coreentitysync.SubscriberRef{ID: 9}
	frames := []RoomFrame{
		{RoomID: 7, Frame: 1, Subscriber: subscriber, SessionSequence: 1,
			Entries: []RoomFrameEntry{{Kind: coreentitysync.EnvelopeSnapshot, Update: testRoomUpdate(1001, 1, 0, true, []byte("snapshot"))}}},
		{RoomID: 7, Frame: 2, Subscriber: subscriber, SessionSequence: 2,
			Entries: []RoomFrameEntry{{Kind: coreentitysync.EnvelopeDelta, Update: testRoomUpdate(1001, 2, 1, false, []byte("pos_x"))}}},
		{RoomID: 7, Frame: 3, Subscriber: subscriber, SessionSequence: 3,
			Entries: []RoomFrameEntry{{Kind: coreentitysync.EnvelopeDelta, Update: testRoomUpdate(1001, 3, 2, false, []byte("equipment"))}}},
	}
	for _, frame := range frames {
		if err := sink.AdmitRoomFrames(context.Background(), []RoomFrame{frame}); err != nil {
			t.Fatal(err)
		}
	}
	// Close drains the queues, so everything admitted has been handed down.
	if err := async.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"snapshot", "pos_x", "equipment"} {
		if !downstream.sawPayload(want) {
			t.Fatalf("%q never reached the client; a committed change was dropped in transport: %s", want, downstream.summary())
		}
	}
}

// collectingTransport is the wire end: it keeps every payload it is handed,
// on either lane, so a test can ask whether a change survived the trip.
type collectingTransport struct {
	mu        stdsync.Mutex
	reliable  [][]byte
	datagrams [][]byte
}

func (t *collectingTransport) SendReliable(_ context.Context, _ core.SessionID, payload []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reliable = append(t.reliable, append([]byte(nil), payload...))
	return nil
}

func (t *collectingTransport) SendDatagram(_ context.Context, _ core.SessionID, payload []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.datagrams = append(t.datagrams, append([]byte(nil), payload...))
	return nil
}

// sawPayload looks for the marker bytes anywhere in what was sent. The frames
// are encoded, so this is a containment check rather than a decode — it is
// enough to answer "did this change survive", which is the question.
func (t *collectingTransport) sawPayload(marker string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, payload := range append(append([][]byte{}, t.reliable...), t.datagrams...) {
		if bytes.Contains(payload, []byte(marker)) {
			return true
		}
	}
	return false
}

func (t *collectingTransport) summary() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return fmt.Sprintf("%d reliable, %d datagram payload(s) reached the client", len(t.reliable), len(t.datagrams))
}

// The datagram machinery stays reachable for a deployment whose frames really
// are self-contained; turning it on is an explicit statement, and this pins
// that the switch still does what it says.
func TestLatestOnlyDeltasRemainsAvailableAsAnExplicitChoice(t *testing.T) {
	transport := &recordingAtomicTransport{}
	sink, err := NewRoomTransportSink(RoomTransportSinkConfig{
		Transport:        transport,
		LatestOnlyDeltas: true,
		Sessions: RoomSessionResolverFunc(func(_ context.Context, subscriber coreentitysync.SubscriberRef) (core.SessionID, error) {
			return core.SessionID(subscriber.ID), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	subscriber := coreentitysync.SubscriberRef{ID: 9}
	if err := sink.AdmitRoomFrames(context.Background(), []RoomFrame{{
		RoomID: 7, Frame: 1, Subscriber: subscriber, SessionSequence: 1,
		Entries: []RoomFrameEntry{{Kind: coreentitysync.EnvelopeSnapshot, Update: testRoomUpdate(1001, 1, 0, true, []byte("snapshot"))}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := sink.AdmitRoomFrames(context.Background(), []RoomFrame{{
		RoomID: 7, Frame: 2, Subscriber: subscriber, SessionSequence: 2,
		Entries: []RoomFrameEntry{{Kind: coreentitysync.EnvelopeDelta, Update: testRoomUpdate(1001, 2, 1, false, []byte("delta"))}},
	}}); err != nil {
		t.Fatal(err)
	}
	if len(transport.batches[1][0].Datagrams) == 0 {
		t.Fatal("LatestOnlyDeltas did not put the delta on the datagram lane")
	}
	// The snapshot is a lifecycle frame and stays reliable either way.
	if len(transport.batches[0][0].Reliable) == 0 {
		t.Fatal("a snapshot left the reliable lane")
	}
}
