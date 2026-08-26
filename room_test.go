package lockstep

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/cube-core/lockstep"
	corerep "github.com/tjbdwanghaibo/cube-core/replication"
)

type recordingTransport struct {
	datagrams map[corerep.SessionID][][]byte
	reliable  map[corerep.SessionID][][]byte
	failing   map[corerep.SessionID]error
}

func newRecordingTransport() *recordingTransport {
	return &recordingTransport{
		datagrams: make(map[corerep.SessionID][][]byte),
		reliable:  make(map[corerep.SessionID][][]byte),
		failing:   make(map[corerep.SessionID]error),
	}
}

func (t *recordingTransport) SendDatagram(_ context.Context, session corerep.SessionID, packet []byte) error {
	if err := t.failing[session]; err != nil {
		return err
	}
	t.datagrams[session] = append(t.datagrams[session], append([]byte(nil), packet...))
	return nil
}

func (t *recordingTransport) SendReliable(_ context.Context, session corerep.SessionID, packet []byte) error {
	if err := t.failing[session]; err != nil {
		return err
	}
	t.reliable[session] = append(t.reliable[session], append([]byte(nil), packet...))
	return nil
}

func newTestRoom(t *testing.T, transport *recordingTransport, onDesync func(lockstep.DesyncVerdict)) *Room {
	t.Helper()
	room, err := NewRoom(RoomConfig{
		Sequencer:          lockstep.SequencerConfig{Players: []lockstep.PlayerID{1, 2}, MaxInputBytes: 16},
		RedundancyDepth:    3,
		CatchupBatchFrames: 10,
		Datagrams:          transport,
		Reliable:           transport,
		OnDesync:           onDesync,
	})
	if err != nil {
		t.Fatal(err)
	}
	return room
}

func decodeAll(t *testing.T, packets [][]byte) map[lockstep.FrameID]lockstep.Frame {
	t.Helper()
	frames := make(map[lockstep.FrameID]lockstep.Frame)
	for _, packet := range packets {
		decoded, err := lockstep.DecodeBroadcast(packet)
		if err != nil {
			t.Fatal(err)
		}
		for _, frame := range decoded {
			frames[frame.ID] = frame
		}
	}
	return frames
}

func TestRoomTickBroadcastsRedundantFrames(t *testing.T) {
	transport := newRecordingTransport()
	room := newTestRoom(t, transport, nil)
	if err := room.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	if err := room.Attach(2, 102); err != nil {
		t.Fatal(err)
	}
	if err := room.Attach(9, 109); !errors.Is(err, lockstep.ErrPlayerUnknown) {
		t.Fatalf("unknown seat attached: %v", err)
	}
	if _, err := room.SubmitInput(1, 1, []byte{0x11}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := room.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Late input for cut frame 1 folds forward instead of erroring.
	if folded, err := room.SubmitInput(2, 1, []byte{0x22}); err != nil || folded != 4 {
		t.Fatalf("late fold = %d err=%v, want 4", folded, err)
	}
	for _, session := range []corerep.SessionID{101, 102} {
		if len(transport.datagrams[session]) != 3 {
			t.Fatalf("session %d datagrams = %d", session, len(transport.datagrams[session]))
		}
	}
	// The third packet carries frames 1..3 (redundancy depth 3).
	frames := decodeAll(t, transport.datagrams[101][2:])
	if len(frames) != 3 || len(frames[1].Inputs) != 1 || frames[1].Inputs[0].Player != 1 {
		t.Fatalf("redundant frames = %+v", frames)
	}
}

func TestRoomCatchupPagesHistoryThenGoesLive(t *testing.T) {
	transport := newRecordingTransport()
	room := newTestRoom(t, transport, nil)
	if err := room.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// 25 frames pass while player 2 is not attached (dropped mid-match).
	for i := 0; i < 25; i++ {
		if _, err := room.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Player 2 reconnects and catches up from the beginning.
	if err := room.Attach(2, 202); err != nil {
		t.Fatal(err)
	}
	if err := room.StartCatchup(2, 0); err != nil {
		t.Fatal(err)
	}
	if !room.CatchingUp(2) {
		t.Fatal("catch-up not registered")
	}
	// Rate limit: 10 frames per tick — a catching-up session gets reliable
	// pages, not live datagrams.
	if _, err := room.Tick(ctx); err != nil { // cuts frame 26, pages 1..10
		t.Fatal(err)
	}
	if got := len(transport.reliable[202]); got != 1 {
		t.Fatalf("reliable pages after first tick = %d", got)
	}
	if got := len(transport.datagrams[202]); got != 0 {
		t.Fatalf("live datagrams during catch-up = %d", got)
	}
	if _, err := room.Tick(ctx); err != nil { // frame 27, pages 11..20
		t.Fatal(err)
	}
	if _, err := room.Tick(ctx); err != nil { // frame 28, pages 21..28 -> caught up
		t.Fatal(err)
	}
	if room.CatchingUp(2) {
		t.Fatal("catch-up did not finish")
	}
	if _, err := room.Tick(ctx); err != nil { // frame 29 goes live
		t.Fatal(err)
	}
	received := decodeAll(t, transport.reliable[202])
	for _, packets := range transport.datagrams[202] {
		for id, frame := range decodeAll(t, [][]byte{packets}) {
			received[id] = frame
		}
	}
	for id := lockstep.FrameID(1); id <= 29; id++ {
		if _, ok := received[id]; !ok {
			t.Fatalf("frame %d missing after catch-up + live", id)
		}
	}
	if len(transport.datagrams[202]) != 1 {
		t.Fatalf("live datagrams after catch-up = %d", len(transport.datagrams[202]))
	}
}

func TestRoomCatchupRequiresReliableLane(t *testing.T) {
	transport := newRecordingTransport()
	room, err := NewRoom(RoomConfig{
		Sequencer: lockstep.SequencerConfig{Players: []lockstep.PlayerID{1}, MaxInputBytes: 16},
		Datagrams: transport,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := room.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	if err := room.StartCatchup(1, 0); !errors.Is(err, ErrCatchupUnavailable) {
		t.Fatalf("catch-up without reliable lane: %v", err)
	}
	withReliable := newTestRoom(t, transport, nil)
	if err := withReliable.StartCatchup(1, 0); !errors.Is(err, ErrPlayerDetached) {
		t.Fatalf("catch-up without session: %v", err)
	}
}

func TestRoomDesyncVerdictSetSemantics(t *testing.T) {
	transport := newRecordingTransport()
	var verdicts []lockstep.DesyncVerdict
	room, err := NewRoom(RoomConfig{
		Sequencer: lockstep.SequencerConfig{Players: []lockstep.PlayerID{1, 2, 3, 4}, MaxInputBytes: 16},
		// HashQuorum unset: derived majority-of-seats = 3.
		Datagrams: transport,
		OnDesync:  func(v lockstep.DesyncVerdict) { verdicts = append(verdicts, v) },
	})
	if err != nil {
		t.Fatal(err)
	}
	// A hash report needs a cut frame first.
	for i := 0; i < 20; i++ {
		if _, err := room.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	// Reports are validated: forged seats and future frames are refused.
	if err := room.ReportHash(99, 15, 0xAA); !errors.Is(err, lockstep.ErrPlayerUnknown) {
		t.Fatalf("forged seat accepted: %v", err)
	}
	if err := room.ReportHash(1, 9999, 0xAA); !errors.Is(err, ErrHashFrameInvalid) {
		t.Fatalf("future frame accepted: %v", err)
	}
	// Two agreeing + one dissenting: no verdict (agreeing group < quorum 3)
	// — a colluding pair reporting first cannot convict the honest player.
	_ = room.ReportHash(1, 15, 0xAA)
	_ = room.ReportHash(2, 15, 0xAA)
	_ = room.ReportHash(3, 15, 0xDD)
	if len(verdicts) != 0 {
		t.Fatalf("verdict before an agreeing quorum: %+v", verdicts)
	}
	// The fourth agreeing report seals the majority; the dissenter is the
	// outlier.
	_ = room.ReportHash(4, 15, 0xAA)
	if len(verdicts) != 1 || !reflect.DeepEqual(verdicts[0].Outliers, []lockstep.PlayerID{3}) {
		t.Fatalf("verdicts = %+v", verdicts)
	}
	// Re-reporting an already-surfaced ruling must not re-fire.
	if err := room.ReportHash(4, 15, 0xAA); err != nil || len(verdicts) != 1 {
		t.Fatalf("unchanged ruling re-fired: %v %+v", err, verdicts)
	}
	room.TrimHashReports(16)
	// Tombstoned: colluders cannot rebuild a report set post-trim.
	_ = room.ReportHash(1, 15, 0xEE)
	_ = room.ReportHash(2, 15, 0xEE)
	_ = room.ReportHash(3, 15, 0xEE)
	if len(verdicts) != 1 {
		t.Fatalf("post-trim reports produced a verdict: %+v", verdicts)
	}
}

func TestRoomTickSurvivesDeadSession(t *testing.T) {
	transport := newRecordingTransport()
	room := newTestRoom(t, transport, nil)
	if err := room.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	if err := room.Attach(2, 102); err != nil {
		t.Fatal(err)
	}
	transport.failing[102] = errors.New("session gone")
	frame, err := room.Tick(context.Background())
	if err == nil {
		t.Fatal("dead session error swallowed")
	}
	// The frame is still cut, recorded, and delivered to healthy sessions.
	if frame.ID != 1 || room.History().Latest() != 1 || len(transport.datagrams[101]) != 1 {
		t.Fatalf("frame=%+v latest=%d healthy=%d", frame, room.History().Latest(), len(transport.datagrams[101]))
	}
}

type nullTransport struct{}

func (nullTransport) SendDatagram(context.Context, corerep.SessionID, []byte) error { return nil }
func (nullTransport) SendReliable(context.Context, corerep.SessionID, []byte) error { return nil }

func BenchmarkRoomTickTenPlayers(b *testing.B) {
	room, err := NewRoom(RoomConfig{
		Sequencer: lockstep.SequencerConfig{Players: []lockstep.PlayerID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, SubmitWindow: 4, MaxInputBytes: 16},
		Datagrams: nullTransport{},
		Reliable:  nullTransport{},
	})
	if err != nil {
		b.Fatal(err)
	}
	for player := lockstep.PlayerID(1); player <= 10; player++ {
		if err := room.Attach(player, corerep.SessionID(player)); err != nil {
			b.Fatal(err)
		}
	}
	ctx := context.Background()
	payload := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		frame := room.NextFrame()
		for player := lockstep.PlayerID(1); player <= 10; player++ {
			if _, err := room.SubmitInput(player, frame, payload); err != nil {
				b.Fatal(err)
			}
		}
		if _, err := room.Tick(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

func TestRoomBudgetValidationRejectsOversizedConfig(t *testing.T) {
	transport := newRecordingTransport()
	// Default MaxInputBytes (1024) × depth 3 blows the 1232-byte datagram
	// budget: a single full-payload client would black out the whole room.
	_, err := NewRoom(RoomConfig{
		Sequencer: lockstep.SequencerConfig{Players: []lockstep.PlayerID{1, 2}},
		Datagrams: transport,
	})
	if !errors.Is(err, ErrRoomConfigInvalid) {
		t.Fatalf("oversized budget accepted: %v", err)
	}
	// A generous custom datagram bound admits it again.
	if _, err := NewRoom(RoomConfig{
		Sequencer:        lockstep.SequencerConfig{Players: []lockstep.PlayerID{1, 2}},
		MaxDatagramBytes: 1 << 20,
		Datagrams:        transport,
	}); err != nil {
		t.Fatalf("relaxed budget rejected: %v", err)
	}
}

func TestRoomSessionExclusivityAndIdempotentReattach(t *testing.T) {
	transport := newRecordingTransport()
	room := newTestRoom(t, transport, nil)
	ctx := context.Background()
	if err := room.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	// Another seat must not steal the session (double-send + catch-up
	// cross-cancel hazard).
	if err := room.Attach(2, 101); !errors.Is(err, ErrSessionInUse) {
		t.Fatalf("session sharing accepted: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := room.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := room.StartCatchup(1, 1); err != nil {
		t.Fatal(err)
	}
	// A defensive duplicate Attach with the SAME session keeps the cursor.
	if err := room.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	if !room.CatchingUp(1) {
		t.Fatal("idempotent re-attach cancelled the catch-up")
	}
	// A NEW session (real reconnect) replaces it and drops the catch-up.
	if err := room.Attach(1, 111); err != nil {
		t.Fatal(err)
	}
	if room.CatchingUp(1) {
		t.Fatal("stale catch-up survived a session replacement")
	}
	// The replaced session id is free again.
	if err := room.Attach(2, 101); err != nil {
		t.Fatalf("released session still reserved: %v", err)
	}
}

func TestRoomCatchupBoundsAndRetryBudget(t *testing.T) {
	transport := newRecordingTransport()
	room, err := NewRoom(RoomConfig{
		Sequencer:          lockstep.SequencerConfig{Players: []lockstep.PlayerID{1}, MaxInputBytes: 16},
		CatchupBatchFrames: 4,
		CatchupMaxFailures: 2,
		Datagrams:          transport,
		Reliable:           transport,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := room.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := room.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// A cursor beyond the next uncut frame is refused outright.
	if err := room.StartCatchup(1, room.NextFrame()+10); !errors.Is(err, ErrCatchupFuture) {
		t.Fatalf("future catch-up accepted: %v", err)
	}
	// Reliable lane keeps failing: the catch-up is abandoned after the
	// budget instead of pinning the session forever.
	transport.failing[101] = errors.New("backpressure")
	if err := room.StartCatchup(1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := room.Tick(ctx); err == nil {
		t.Fatal("first failure swallowed")
	}
	_, err = room.Tick(ctx)
	if err == nil || !strings.Contains(err.Error(), "abandoned after 2 failures") {
		t.Fatalf("catch-up not abandoned: %v", err)
	}
	if room.CatchingUp(1) {
		t.Fatal("session pinned in catch-up after abandonment")
	}
	// After abandonment live frames flow again.
	transport.failing[101] = nil
	before := len(transport.datagrams[101])
	if _, err := room.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(transport.datagrams[101]) != before+1 {
		t.Fatal("live broadcast not restored")
	}
}

func TestRoomCatchupAbandonedWhenHistoryTrimmed(t *testing.T) {
	transport := newRecordingTransport()
	room := newTestRoom(t, transport, nil)
	ctx := context.Background()
	if err := room.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		if _, err := room.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	room.TrimHistory(20)
	if err := room.StartCatchup(1, 5); err != nil { // asks for trimmed range
		t.Fatal(err)
	}
	_, err := room.Tick(ctx)
	if err == nil || !errors.Is(err, ErrCatchupUnservable) {
		t.Fatalf("gap-in-history catch-up not abandoned: %v", err)
	}
	if room.CatchingUp(1) {
		t.Fatal("unservable catch-up left pending")
	}
}

func TestRoomSpectatorsReceiveBroadcastAndCatchup(t *testing.T) {
	transport := newRecordingTransport()
	room := newTestRoom(t, transport, nil)
	ctx := context.Background()
	if err := room.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	if err := room.AttachSpectator(500); err != nil {
		t.Fatal(err)
	}
	// A spectator session is exclusive too.
	if err := room.Attach(2, 500); !errors.Is(err, ErrSessionInUse) {
		t.Fatalf("seat stole a spectator session: %v", err)
	}
	for i := 0; i < 12; i++ {
		if _, err := room.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(transport.datagrams[500]) != 12 {
		t.Fatalf("spectator broadcasts = %d", len(transport.datagrams[500]))
	}
	if err := room.SpectatorCatchup(500, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := room.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(transport.reliable[500]) == 0 {
		t.Fatal("spectator catch-up produced no pages")
	}
	room.DetachSpectator(500)
	before := len(transport.datagrams[500])
	if _, err := room.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(transport.datagrams[500]) != before {
		t.Fatal("detached spectator still receiving")
	}
}

func TestRoomCloseRejectsFurtherUse(t *testing.T) {
	transport := newRecordingTransport()
	room := newTestRoom(t, transport, nil)
	if err := room.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	room.Close()
	if _, err := room.Tick(context.Background()); !errors.Is(err, ErrRoomClosed) {
		t.Fatalf("tick after close: %v", err)
	}
	if _, err := room.SubmitInput(1, 1, nil); !errors.Is(err, ErrRoomClosed) {
		t.Fatalf("submit after close: %v", err)
	}
	if err := room.Attach(1, 101); !errors.Is(err, ErrRoomClosed) {
		t.Fatalf("attach after close: %v", err)
	}
}
