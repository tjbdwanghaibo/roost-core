// Match：冗余广播、追帧分页与切回 live、desync 裁决、会话互斥与重连、预算含传输开销、慢追帧不阻塞 Tick、指标名钉住。

package lockstep

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/nettransport"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// testSendWait makes Tick wait for every catch-up page it launches, so
// in-memory transports behave synchronously and tests can count pages per
// tick deterministically. The asynchronous path (a page outliving the wait)
// is covered by TestMatchPromiseSlowCatchupDoesNotStallTick and the
// end-to-end gate.
const testSendWait = time.Minute

type recordingTransport struct {
	datagrams map[nettransport.SessionID][][]byte
	reliable  map[nettransport.SessionID][][]byte
	failing   map[nettransport.SessionID]error
}

func newRecordingTransport() *recordingTransport {
	return &recordingTransport{
		datagrams: make(map[nettransport.SessionID][][]byte),
		reliable:  make(map[nettransport.SessionID][][]byte),
		failing:   make(map[nettransport.SessionID]error),
	}
}

func (t *recordingTransport) SendDatagram(_ context.Context, session nettransport.SessionID, packet []byte) error {
	if err := t.failing[session]; err != nil {
		return err
	}
	t.datagrams[session] = append(t.datagrams[session], append([]byte(nil), packet...))
	return nil
}

func (t *recordingTransport) SendReliable(_ context.Context, session nettransport.SessionID, packet []byte) error {
	if err := t.failing[session]; err != nil {
		return err
	}
	t.reliable[session] = append(t.reliable[session], append([]byte(nil), packet...))
	return nil
}

func newTestMatch(t *testing.T, transport *recordingTransport, onDesync func(DesyncVerdict)) *Match {
	t.Helper()
	match, err := NewMatch(MatchConfig{
		Sequencer:          SequencerConfig{Players: []PlayerID{1, 2}, MaxInputBytes: 16},
		RedundancyDepth:    3,
		CatchupBatchFrames: 10,
		CatchupSendWait:    testSendWait,
		Datagrams:          transport,
		Reliable:           transport,
		OnDesync:           onDesync,
	})
	if err != nil {
		t.Fatal(err)
	}
	return match
}

func decodeAll(t *testing.T, packets [][]byte) map[FrameID]Frame {
	t.Helper()
	frames := make(map[FrameID]Frame)
	for _, packet := range packets {
		decoded, err := DecodeBroadcast(packet)
		if err != nil {
			t.Fatal(err)
		}
		for _, frame := range decoded {
			frames[frame.ID] = frame
		}
	}
	return frames
}

func TestMatchTickBroadcastsRedundantFrames(t *testing.T) {
	transport := newRecordingTransport()
	match := newTestMatch(t, transport, nil)
	if err := match.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	if err := match.Attach(2, 102); err != nil {
		t.Fatal(err)
	}
	if err := match.Attach(9, 109); !errors.Is(err, ErrPlayerUnknown) {
		t.Fatalf("unknown seat attached: %v", err)
	}
	if _, err := match.SubmitInput(1, 1, []byte{0x11}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := match.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Late input for cut frame 1 folds forward instead of erroring.
	if folded, err := match.SubmitInput(2, 1, []byte{0x22}); err != nil || folded != 4 {
		t.Fatalf("late fold = %d err=%v, want 4", folded, err)
	}
	for _, session := range []nettransport.SessionID{101, 102} {
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

func TestMatchCatchupPagesHistoryThenGoesLive(t *testing.T) {
	transport := newRecordingTransport()
	match := newTestMatch(t, transport, nil)
	if err := match.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// 25 frames pass while player 2 is not attached (dropped mid-match).
	for i := 0; i < 25; i++ {
		if _, err := match.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Player 2 reconnects and catches up from the beginning.
	if err := match.Attach(2, 202); err != nil {
		t.Fatal(err)
	}
	if err := match.StartCatchup(2, 0); err != nil {
		t.Fatal(err)
	}
	if !match.CatchingUp(2) {
		t.Fatal("catch-up not registered")
	}
	// Rate limit: 10 frames per tick — a catching-up session gets reliable
	// pages, not live datagrams.
	if _, err := match.Tick(ctx); err != nil { // cuts frame 26, pages 1..10
		t.Fatal(err)
	}
	if got := len(transport.reliable[202]); got != 1 {
		t.Fatalf("reliable pages after first tick = %d", got)
	}
	if got := len(transport.datagrams[202]); got != 0 {
		t.Fatalf("live datagrams during catch-up = %d", got)
	}
	if _, err := match.Tick(ctx); err != nil { // frame 27, pages 11..20
		t.Fatal(err)
	}
	if _, err := match.Tick(ctx); err != nil { // frame 28, pages 21..28 -> caught up
		t.Fatal(err)
	}
	if match.CatchingUp(2) {
		t.Fatal("catch-up did not finish")
	}
	if _, err := match.Tick(ctx); err != nil { // frame 29 goes live
		t.Fatal(err)
	}
	received := decodeAll(t, transport.reliable[202])
	for _, packets := range transport.datagrams[202] {
		for id, frame := range decodeAll(t, [][]byte{packets}) {
			received[id] = frame
		}
	}
	for id := FrameID(1); id <= 29; id++ {
		if _, ok := received[id]; !ok {
			t.Fatalf("frame %d missing after catch-up + live", id)
		}
	}
	if len(transport.datagrams[202]) != 1 {
		t.Fatalf("live datagrams after catch-up = %d", len(transport.datagrams[202]))
	}
}

func TestMatchCatchupRequiresReliableLane(t *testing.T) {
	transport := newRecordingTransport()
	match, err := NewMatch(MatchConfig{
		Sequencer: SequencerConfig{Players: []PlayerID{1}, MaxInputBytes: 16},
		Datagrams: transport,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := match.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	if err := match.StartCatchup(1, 0); !errors.Is(err, ErrCatchupUnavailable) {
		t.Fatalf("catch-up without reliable lane: %v", err)
	}
	withReliable := newTestMatch(t, transport, nil)
	if err := withReliable.StartCatchup(1, 0); !errors.Is(err, ErrPlayerDetached) {
		t.Fatalf("catch-up without session: %v", err)
	}
}

func TestMatchDesyncVerdictSetSemantics(t *testing.T) {
	transport := newRecordingTransport()
	var verdicts []DesyncVerdict
	match, err := NewMatch(MatchConfig{
		Sequencer: SequencerConfig{Players: []PlayerID{1, 2, 3, 4}, MaxInputBytes: 16},
		// HashQuorum unset: derived majority-of-seats = 3.
		Datagrams: transport,
		OnDesync:  func(v DesyncVerdict) { verdicts = append(verdicts, v) },
	})
	if err != nil {
		t.Fatal(err)
	}
	// A hash report needs a cut frame first.
	for i := 0; i < 20; i++ {
		if _, err := match.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	// Reports are validated: forged seats and future frames are refused.
	if err := match.ReportHash(99, 15, 0xAA); !errors.Is(err, ErrPlayerUnknown) {
		t.Fatalf("forged seat accepted: %v", err)
	}
	if err := match.ReportHash(1, 9999, 0xAA); !errors.Is(err, ErrHashFrameInvalid) {
		t.Fatalf("future frame accepted: %v", err)
	}
	// Two agreeing + one dissenting: no verdict (agreeing group < quorum 3)
	// — a colluding pair reporting first cannot convict the honest player.
	_ = match.ReportHash(1, 15, 0xAA)
	_ = match.ReportHash(2, 15, 0xAA)
	_ = match.ReportHash(3, 15, 0xDD)
	if len(verdicts) != 0 {
		t.Fatalf("verdict before an agreeing quorum: %+v", verdicts)
	}
	// The fourth agreeing report seals the majority; the dissenter is the
	// outlier.
	_ = match.ReportHash(4, 15, 0xAA)
	if len(verdicts) != 1 || !reflect.DeepEqual(verdicts[0].Outliers, []PlayerID{3}) {
		t.Fatalf("verdicts = %+v", verdicts)
	}
	// Re-reporting an already-surfaced ruling must not re-fire.
	if err := match.ReportHash(4, 15, 0xAA); err != nil || len(verdicts) != 1 {
		t.Fatalf("unchanged ruling re-fired: %v %+v", err, verdicts)
	}
	match.TrimHashReports(16)
	// Tombstoned: colluders cannot rebuild a report set post-trim.
	_ = match.ReportHash(1, 15, 0xEE)
	_ = match.ReportHash(2, 15, 0xEE)
	_ = match.ReportHash(3, 15, 0xEE)
	if len(verdicts) != 1 {
		t.Fatalf("post-trim reports produced a verdict: %+v", verdicts)
	}
}

func TestMatchTickSurvivesDeadSession(t *testing.T) {
	transport := newRecordingTransport()
	match := newTestMatch(t, transport, nil)
	if err := match.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	if err := match.Attach(2, 102); err != nil {
		t.Fatal(err)
	}
	transport.failing[102] = errors.New("session gone")
	frame, err := match.Tick(context.Background())
	if err == nil {
		t.Fatal("dead session error swallowed")
	}
	// The frame is still cut, recorded, and delivered to healthy sessions.
	if frame.ID != 1 || match.History().Latest() != 1 || len(transport.datagrams[101]) != 1 {
		t.Fatalf("frame=%+v latest=%d healthy=%d", frame, match.History().Latest(), len(transport.datagrams[101]))
	}
}

type nullTransport struct{}

func (nullTransport) SendDatagram(context.Context, nettransport.SessionID, []byte) error { return nil }
func (nullTransport) SendReliable(context.Context, nettransport.SessionID, []byte) error { return nil }

func BenchmarkMatchTickTenPlayers(b *testing.B) {
	match, err := NewMatch(MatchConfig{
		Sequencer: SequencerConfig{Players: []PlayerID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, SubmitWindow: 4, MaxInputBytes: 16},
		Datagrams: nullTransport{},
		Reliable:  nullTransport{},
	})
	if err != nil {
		b.Fatal(err)
	}
	for player := PlayerID(1); player <= 10; player++ {
		if err := match.Attach(player, nettransport.SessionID(player)); err != nil {
			b.Fatal(err)
		}
	}
	ctx := context.Background()
	payload := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		frame := match.NextFrame()
		for player := PlayerID(1); player <= 10; player++ {
			if _, err := match.SubmitInput(player, frame, payload); err != nil {
				b.Fatal(err)
			}
		}
		if _, err := match.Tick(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

func TestMatchBudgetValidationRejectsOversizedConfig(t *testing.T) {
	transport := newRecordingTransport()
	// Default MaxInputBytes (1024) × depth 3 blows the 1232-byte datagram
	// budget: a single full-payload client would black out the whole match.
	_, err := NewMatch(MatchConfig{
		Sequencer: SequencerConfig{Players: []PlayerID{1, 2}},
		Datagrams: transport,
	})
	if !errors.Is(err, ErrMatchConfigInvalid) {
		t.Fatalf("oversized budget accepted: %v", err)
	}
	// A generous custom datagram bound admits it again.
	if _, err := NewMatch(MatchConfig{
		Sequencer:        SequencerConfig{Players: []PlayerID{1, 2}},
		MaxDatagramBytes: 1 << 20,
		Datagrams:        transport,
	}); err != nil {
		t.Fatalf("relaxed budget rejected: %v", err)
	}
}

func TestMatchSessionExclusivityAndIdempotentReattach(t *testing.T) {
	transport := newRecordingTransport()
	match := newTestMatch(t, transport, nil)
	ctx := context.Background()
	if err := match.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	// Another seat must not steal the session (double-send + catch-up
	// cross-cancel hazard).
	if err := match.Attach(2, 101); !errors.Is(err, ErrSessionInUse) {
		t.Fatalf("session sharing accepted: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := match.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := match.StartCatchup(1, 1); err != nil {
		t.Fatal(err)
	}
	// A defensive duplicate Attach with the SAME session keeps the cursor.
	if err := match.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	if !match.CatchingUp(1) {
		t.Fatal("idempotent re-attach cancelled the catch-up")
	}
	// A NEW session (real reconnect) replaces it and drops the catch-up.
	if err := match.Attach(1, 111); err != nil {
		t.Fatal(err)
	}
	if match.CatchingUp(1) {
		t.Fatal("stale catch-up survived a session replacement")
	}
	// The replaced session id is free again.
	if err := match.Attach(2, 101); err != nil {
		t.Fatalf("released session still reserved: %v", err)
	}
}

func TestMatchCatchupBoundsAndRetryBudget(t *testing.T) {
	transport := newRecordingTransport()
	match, err := NewMatch(MatchConfig{
		Sequencer:          SequencerConfig{Players: []PlayerID{1}, MaxInputBytes: 16},
		CatchupBatchFrames: 4,
		CatchupMaxFailures: 2,
		CatchupSendWait:    testSendWait,
		Datagrams:          transport,
		Reliable:           transport,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := match.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := match.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// A cursor beyond the next uncut frame is refused outright.
	if err := match.StartCatchup(1, match.NextFrame()+10); !errors.Is(err, ErrCatchupFuture) {
		t.Fatalf("future catch-up accepted: %v", err)
	}
	// Reliable lane keeps failing: the catch-up is abandoned after the
	// budget instead of pinning the session forever.
	transport.failing[101] = errors.New("backpressure")
	if err := match.StartCatchup(1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := match.Tick(ctx); err == nil {
		t.Fatal("first failure swallowed")
	}
	_, err = match.Tick(ctx)
	if err == nil || !strings.Contains(err.Error(), "abandoned after 2 failures") {
		t.Fatalf("catch-up not abandoned: %v", err)
	}
	if match.CatchingUp(1) {
		t.Fatal("session pinned in catch-up after abandonment")
	}
	// After abandonment live frames flow again.
	transport.failing[101] = nil
	before := len(transport.datagrams[101])
	if _, err := match.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(transport.datagrams[101]) != before+1 {
		t.Fatal("live broadcast not restored")
	}
}

func TestMatchCatchupAbandonedWhenHistoryTrimmed(t *testing.T) {
	transport := newRecordingTransport()
	match := newTestMatch(t, transport, nil)
	ctx := context.Background()
	if err := match.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		if _, err := match.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	match.TrimHistory(20)
	if err := match.StartCatchup(1, 5); err != nil { // asks for trimmed range
		t.Fatal(err)
	}
	_, err := match.Tick(ctx)
	if err == nil || !errors.Is(err, ErrCatchupUnservable) {
		t.Fatalf("gap-in-history catch-up not abandoned: %v", err)
	}
	if match.CatchingUp(1) {
		t.Fatal("unservable catch-up left pending")
	}
}

func TestMatchSpectatorsReceiveBroadcastAndCatchup(t *testing.T) {
	transport := newRecordingTransport()
	match := newTestMatch(t, transport, nil)
	ctx := context.Background()
	if err := match.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	if err := match.AttachSpectator(500); err != nil {
		t.Fatal(err)
	}
	// A spectator session is exclusive too.
	if err := match.Attach(2, 500); !errors.Is(err, ErrSessionInUse) {
		t.Fatalf("seat stole a spectator session: %v", err)
	}
	for i := 0; i < 12; i++ {
		if _, err := match.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(transport.datagrams[500]) != 12 {
		t.Fatalf("spectator broadcasts = %d", len(transport.datagrams[500]))
	}
	if err := match.SpectatorCatchup(500, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := match.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(transport.reliable[500]) == 0 {
		t.Fatal("spectator catch-up produced no pages")
	}
	match.DetachSpectator(500)
	before := len(transport.datagrams[500])
	if _, err := match.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(transport.datagrams[500]) != before {
		t.Fatal("detached spectator still receiving")
	}
}

func TestMatchCloseRejectsFurtherUse(t *testing.T) {
	transport := newRecordingTransport()
	match := newTestMatch(t, transport, nil)
	if err := match.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	match.Close()
	if _, err := match.Tick(context.Background()); !errors.Is(err, ErrMatchClosed) {
		t.Fatalf("tick after close: %v", err)
	}
	if _, err := match.SubmitInput(1, 1, nil); !errors.Is(err, ErrMatchClosed) {
		t.Fatalf("submit after close: %v", err)
	}
	if err := match.Attach(1, 101); !errors.Is(err, ErrMatchClosed) {
		t.Fatalf("attach after close: %v", err)
	}
}

func TestMatchLateDisconnectCannotDetachReplacement(t *testing.T) {
	match := newTestMatch(t, newRecordingTransport(), nil)
	if err := match.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	if err := match.Attach(1, 202); err != nil {
		t.Fatal(err)
	}
	if match.DetachSession(1, 101) {
		t.Fatal("old disconnect removed replacement")
	}
	command := Command{Operation: OpInput, Frame: 1, Payload: []byte{7}}
	if err := match.HandleCommand(101, command); !errors.Is(err, ErrPlayerDetached) {
		t.Fatalf("old input=%v", err)
	}
	if err := match.HandleCommand(202, command); err != nil {
		t.Fatal(err)
	}
	if !match.DetachSession(1, 202) {
		t.Fatal("current disconnect did not detach")
	}
	if match.DetachSession(1, 202) {
		t.Fatal("disconnect not idempotent")
	}
	if err := match.HandleCommand(202, command); !errors.Is(err, ErrPlayerDetached) {
		t.Fatalf("detached input=%v", err)
	}
}

// 被接受的追帧配置必须能在正常链路下追上帧头并切回 live：补帧净速度大于产帧速度，batch=1 在创建时拒绝；batch=2 从落后 5 帧起
// 有限轮内收敛。
// 历史：RR-20260914-05。

func TestMatchPromiseCatchupBatchMustOutpaceFrameProduction(t *testing.T) {
	for _, batch := range []int{1, 2} {
		t.Run(string(rune('0'+batch)), func(t *testing.T) {
			tr := newRecordingTransport()
			r, err := NewMatch(MatchConfig{
				Sequencer:          SequencerConfig{Players: []PlayerID{1}, MaxInputBytes: 8},
				CatchupBatchFrames: batch, CatchupSendWait: testSendWait, Datagrams: tr, Reliable: tr,
			})
			if err != nil {
				if batch == 1 && errors.Is(err, ErrMatchConfigInvalid) {
					return // refusing the non-converging configuration is the contract
				}
				t.Fatal(err)
			}
			defer r.Close()
			ctx := context.Background()
			for i := 0; i < 5; i++ {
				if _, err := r.Tick(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.Attach(1, 101); err != nil {
				t.Fatal(err)
			}
			if err := r.StartCatchup(1, 1); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 20; i++ {
				if _, err := r.Tick(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if r.CatchingUp(1) {
				t.Fatalf("batch=%d never closes backlog: next=%d head=%d reliable_pages=%d",
					batch, r.catchups[101].next, r.history.Latest(), len(tr.reliable[101]))
			}
		})
	}
}

// lockstepMetricNames is the pinned metric surface of this package. The
// alert rules in OBSERVABILITY.md are keyed on these exact names (历史：RR-20261006-61).
var lockstepMetricNames = []string{
	"lockstep.catchup.frames.total",
	"lockstep.desync.total",
	"lockstep.frame.total",
	"lockstep.input.late.total",
	"lockstep.input.rejected.total",
}

// TestMatchPromiseMetricNamesArePinned drives every metered path of the Match
// and requires the emitted names to be exactly the pinned set, each listed
// in OBSERVABILITY.md (RR-20261006-61).
func TestMatchPromiseMetricNamesArePinned(t *testing.T) {
	previous := metrics.DefaultRegistry()
	metrics.SetDefaultRegistry(metrics.NewRegistry())
	t.Cleanup(func() { metrics.SetDefaultRegistry(previous) })

	transport := newRecordingTransport()
	match, err := NewMatch(MatchConfig{
		Sequencer:          SequencerConfig{Players: []PlayerID{1, 2, 3}, MaxInputBytes: 8},
		RedundancyDepth:    2,
		CatchupBatchFrames: 4,
		CatchupSendWait:    testSendWait,
		Datagrams:          transport,
		Reliable:           transport,
	})
	if err != nil {
		t.Fatal(err)
	}
	for player := PlayerID(1); player <= 3; player++ {
		if err := match.Attach(player, nettransport.SessionID(100+player)); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := match.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := match.SubmitInput(1, 1, []byte{1}); err != nil { // late
		t.Fatal(err)
	}
	if _, err := match.SubmitInput(9, 0, []byte{1}); err == nil { // rejected
		t.Fatal("unknown seat accepted")
	}
	if err := match.StartCatchup(2, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := match.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	_ = match.ReportHash(1, 2, 7)
	_ = match.ReportHash(2, 2, 7)
	_ = match.ReportHash(3, 2, 8) // desync: seat 3 is the outlier

	var emitted []string
	for _, metric := range metrics.Snapshot() {
		if !slices.Contains(emitted, metric.Name) {
			emitted = append(emitted, metric.Name)
		}
	}
	slices.Sort(emitted)
	if !slices.Equal(emitted, lockstepMetricNames) {
		t.Fatalf("lockstep metric names = %v, want %v (OBSERVABILITY.md alerts are keyed on the prefixed names)", emitted, lockstepMetricNames)
	}
	// Source guard: every metrics call in the package names a Metric*
	// constant, never an inline literal that could drift from the pinned set.
	if pinned := []string{MetricCatchupFrames, MetricDesync, MetricFrames, MetricInputLate, MetricInputRejected}; !slices.Equal(pinned, lockstepMetricNames) {
		t.Fatalf("Metric* constants = %v, want %v", pinned, lockstepMetricNames)
	}
	inline := regexp.MustCompile(`metrics\.\w+\(\s*"`)
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}
		body, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		if loc := inline.FindIndex(body); loc != nil {
			t.Errorf("%s: metrics call with an inline name at byte %d; use a Metric* constant", source, loc[0])
		}
	}
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "framework", "OBSERVABILITY.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range lockstepMetricNames {
		if !strings.Contains(string(doc), "`"+name) {
			t.Errorf("OBSERVABILITY.md does not list %s", name)
		}
	}
}

// limitedDatagrams models a protocol transport with a fixed datagram
// payload bound (KCP / QUIC default 1200): oversized payloads are refused,
// exactly like KCPTransport.SendDatagram.
type limitedDatagrams struct {
	limit    int
	rejected int
}

func (d *limitedDatagrams) SendDatagram(_ context.Context, _ nettransport.SessionID, payload []byte) error {
	if len(payload) > d.limit {
		d.rejected++
		return nettransport.ErrPayloadTooLarge
	}
	return nil
}

func (d *limitedDatagrams) MaxDatagramPayload() int { return d.limit }

// TestMatchPromiseBudgetCountsTransportOverhead: a configuration NewMatch
// accepts must never produce a full-load broadcast the transport refuses —
// the whole match's downlink would black out at once (RR-20261006-62).
func TestMatchPromiseBudgetCountsTransportOverhead(t *testing.T) {
	sender := &limitedDatagrams{limit: nettransport.DefaultMaxDatagram}
	match, err := NewMatch(MatchConfig{
		Sequencer:       SequencerConfig{Players: []PlayerID{1, 2}, MaxInputBytes: 595},
		RedundancyDepth: 1,
		Datagrams:       sender,
	})
	if err != nil {
		if !errors.Is(err, ErrMatchConfigInvalid) {
			t.Fatalf("NewMatch error = %v, want ErrMatchConfigInvalid", err)
		}
		return // refused up front: the promise holds
	}
	for player := PlayerID(1); player <= 2; player++ {
		if err := match.Attach(player, nettransport.SessionID(player)); err != nil {
			t.Fatal(err)
		}
	}
	fullLoadTicks(t, match)
}

// TestMatchPromiseBudgetFollowsTheSender: the budget is the sender's declared
// payload bound; a configured bound above it is refused, and the AEAD UDP
// transport declares its packet bound minus the envelope Seal adds.
func TestMatchPromiseBudgetFollowsTheSender(t *testing.T) {
	sender := &limitedDatagrams{limit: 1200}
	if _, err := NewMatch(MatchConfig{
		Sequencer:        SequencerConfig{Players: []PlayerID{1, 2}, MaxInputBytes: 8},
		MaxDatagramBytes: 1232,
		Datagrams:        sender,
	}); !errors.Is(err, ErrMatchConfigInvalid) {
		t.Fatalf("MaxDatagramBytes above the sender's bound: err = %v, want ErrMatchConfigInvalid", err)
	}
	// Largest payload admitted for 2 seats at depth 1 fits the sender.
	accepted := 0
	for size := 1024; size > 0; size-- {
		if _, err := NewMatch(MatchConfig{Sequencer: SequencerConfig{Players: []PlayerID{1, 2}, MaxInputBytes: size}, RedundancyDepth: 1, Datagrams: sender}); err == nil {
			accepted = size
			break
		}
	}
	packet := EncodeBroadcast([]Frame{{ID: 1 << 31, Inputs: []Input{{Player: 1 << 30, Payload: make([]byte, accepted)}, {Player: 1 << 30, Payload: make([]byte, accepted)}}}})
	if len(packet) > sender.limit {
		t.Fatalf("admitted payload %dB encodes to %dB > bound %d", accepted, len(packet), sender.limit)
	}
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	udp, err := nettransport.NewUDPTransport(nettransport.UDPTransportConfig{PacketConn: conn, OwnPacketConn: true})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	protector, err := nettransport.NewAESGCMProtector(make([]byte, 32), [4]byte{1}, [4]byte{2})
	if err != nil {
		t.Fatal(err)
	}
	if protector.Overhead() != nettransport.UDPEnvelopeOverhead {
		t.Fatalf("AEAD envelope overhead = %d, UDPEnvelopeOverhead = %d", protector.Overhead(), nettransport.UDPEnvelopeOverhead)
	}
	if got, want := udp.MaxDatagramPayload(), nettransport.DefaultUDPMaxPacketBytes-protector.Overhead(); got != want {
		t.Fatalf("UDP MaxDatagramPayload = %d, want %d", got, want)
	}
}

// fullLoadTicks submits a full-size input for every seat and ticks: no
// broadcast may be refused.
func fullLoadTicks(t *testing.T, match *Match) {
	t.Helper()
	full := make([]byte, 595)
	for i := 0; i < 3; i++ {
		for player := PlayerID(1); player <= 2; player++ {
			if _, err := match.SubmitInput(player, match.NextFrame(), full); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := match.Tick(context.Background()); err != nil {
			t.Fatalf("NewMatch accepted the config but the full-load broadcast of frame %d was refused: %v", i+1, err)
		}
	}
}

// blockingReliable models a reliable lane whose peer stopped acknowledging:
// the send blocks until its context ends (kcp-go's Write with no deadline).
type blockingReliable struct {
	mu      sync.Mutex
	blocked int
	release chan struct{}
}

func (b *blockingReliable) SendReliable(ctx context.Context, _ nettransport.SessionID, _ []byte) error {
	b.mu.Lock()
	b.blocked++
	b.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.release:
		return errors.New("released by test cleanup")
	}
}

// TestMatchPromiseSlowCatchupDoesNotStallTick: one catching-up client whose
// reliable lane stalls must not stop the match — Tick keeps cutting and
// broadcasting to everyone else, with no deadline on the caller's context
// (RR-20261006-63).
func TestMatchPromiseSlowCatchupDoesNotStallTick(t *testing.T) {
	datagrams := newRecordingTransport()
	reliable := &blockingReliable{release: make(chan struct{})}
	t.Cleanup(func() { close(reliable.release) })
	match, err := NewMatch(MatchConfig{
		Sequencer: SequencerConfig{Players: []PlayerID{1, 2}, MaxInputBytes: 8},
		Datagrams: datagrams,
		Reliable:  reliable,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(match.Close)
	for player := PlayerID(1); player <= 2; player++ {
		if err := match.Attach(player, nettransport.SessionID(player)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		if _, err := match.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if err := match.StartCatchup(2, 1); err != nil {
		t.Fatal(err)
	}
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		for i := 0; i < 20; i++ {
			_, _ = match.Tick(context.Background())
		}
		done <- time.Since(start)
	}()
	select {
	case elapsed := <-done:
		if elapsed > time.Second {
			t.Fatalf("20 ticks with one stalled catch-up took %v", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Tick blocked on a stalled catch-up send: every seat stopped receiving frames")
	}
	if got := len(datagrams.datagrams[1]); got != 25 {
		t.Fatalf("seat 1 live broadcasts = %d, want 25 (one per tick)", got)
	}
}

// TestMatchPromiseTwoSeatDesyncIsRuled: in a two-seat match any hash
// disagreement must produce a verdict — the default majority quorum (2)
// can never be met by a 1:1 split (RR-20261006-64).
func TestMatchPromiseTwoSeatDesyncIsRuled(t *testing.T) {
	var verdicts []DesyncVerdict
	match, err := NewMatch(MatchConfig{
		Sequencer: SequencerConfig{Players: []PlayerID{1, 2}, MaxInputBytes: 8},
		Datagrams: newRecordingTransport(),
		OnDesync:  func(v DesyncVerdict) { verdicts = append(verdicts, v) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := match.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := match.ReportHash(1, 1, 0xA); err != nil {
		t.Fatal(err)
	}
	if err := match.ReportHash(2, 1, 0xB); err != nil {
		t.Fatal(err)
	}
	if len(verdicts) != 1 {
		t.Fatalf("two-seat hash split produced %d verdicts, want 1", len(verdicts))
	}
	if got := verdicts[0].Outliers; !slices.Equal(got, []PlayerID{1, 2}) {
		t.Fatalf("outliers = %v, want both seats", got)
	}
	if !verdicts[0].NoMajority || verdicts[0].Majority != 0 {
		t.Fatalf("verdict = %+v, want NoMajority with no majority hash", verdicts[0])
	}
	// Agreement in a two-seat match stays silent.
	if _, err := match.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = match.ReportHash(1, 2, 0xC)
	_ = match.ReportHash(2, 2, 0xC)
	if len(verdicts) != 1 {
		t.Fatalf("agreeing two-seat hashes produced a verdict: %+v", verdicts[1:])
	}
}

// TestMatchPromiseNoMajorityNeedsEverySeat: with more seats the no-majority
// ruling waits for every seat — a 1:1 split among the first two reports of
// a three-seat match is not a verdict yet, and the third report decides.
func TestMatchPromiseNoMajorityNeedsEverySeat(t *testing.T) {
	var verdicts []DesyncVerdict
	match, err := NewMatch(MatchConfig{
		Sequencer: SequencerConfig{Players: []PlayerID{1, 2, 3, 4}, MaxInputBytes: 8},
		Datagrams: newRecordingTransport(),
		OnDesync:  func(v DesyncVerdict) { verdicts = append(verdicts, v) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := match.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = match.ReportHash(1, 1, 0xA)
	_ = match.ReportHash(2, 1, 0xB)
	_ = match.ReportHash(3, 1, 0xA)
	if len(verdicts) != 0 {
		t.Fatalf("verdict before quorum or every seat: %+v", verdicts)
	}
	_ = match.ReportHash(4, 1, 0xB) // 2:2, quorum 3 unreachable
	if len(verdicts) != 1 || !verdicts[0].NoMajority || !slices.Equal(verdicts[0].Outliers, []PlayerID{1, 2, 3, 4}) {
		t.Fatalf("2:2 split verdicts = %+v, want one NoMajority naming all four seats", verdicts)
	}
}

// delayedReliable completes every send after a fixed delay — longer than
// the Match's default send wait, so every page finishes on a later Tick.
type delayedReliable struct {
	delay time.Duration
	mu    sync.Mutex
	pages map[nettransport.SessionID][][]byte
}

func (d *delayedReliable) SendReliable(ctx context.Context, session nettransport.SessionID, payload []byte) error {
	select {
	case <-time.After(d.delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pages[session] = append(d.pages[session], payload)
	return nil
}

// TestMatchPromiseSlowButLiveCatchupConverges: when every page outlives
// Tick's wait, the catch-up still reaches the head and hands over to live
// broadcasts with no frame missing (RR-20261006-63).
func TestMatchPromiseSlowButLiveCatchupConverges(t *testing.T) {
	datagrams := newRecordingTransport()
	reliable := &delayedReliable{delay: 20 * time.Millisecond, pages: make(map[nettransport.SessionID][][]byte)}
	match, err := NewMatch(MatchConfig{
		Sequencer:          SequencerConfig{Players: []PlayerID{1, 2}, MaxInputBytes: 8},
		RedundancyDepth:    3,
		CatchupBatchFrames: 8,
		Datagrams:          datagrams,
		Reliable:           reliable,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(match.Close)
	if err := match.Attach(1, 1); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		if _, err := match.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if err := match.Attach(2, 2); err != nil {
		t.Fatal(err)
	}
	if err := match.StartCatchup(2, 1); err != nil {
		t.Fatal(err)
	}
	ticks := 0
	for ; match.CatchingUp(2) && ticks < 200; ticks++ {
		if _, err := match.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if match.CatchingUp(2) {
		t.Fatalf("catch-up with 20ms pages still running after %d ticks", ticks)
	}
	final, err := match.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reliable.mu.Lock()
	received := decodeAll(t, reliable.pages[2])
	reliable.mu.Unlock()
	for id, frame := range decodeAll(t, datagrams.datagrams[2]) {
		received[id] = frame
	}
	for id := FrameID(1); id <= final.ID; id++ {
		if _, ok := received[id]; !ok {
			t.Fatalf("frame %d never reached the caught-up session (head %d, %d ticks)", id, final.ID, ticks)
		}
	}
}

// gatedReliable completes one send per token on gate.
type gatedReliable struct {
	gate  chan struct{}
	mu    sync.Mutex
	pages [][]byte
}

func (g *gatedReliable) SendReliable(ctx context.Context, _ nettransport.SessionID, payload []byte) error {
	select {
	case <-g.gate:
	case <-ctx.Done():
		return ctx.Err()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pages = append(g.pages, payload)
	return nil
}

// TestMatchPromiseCatchupRewindWhileInFlightIsKept: StartCatchup moving the
// cursor back while a page is in flight must survive that page's success —
// the earlier frames are paged next, not skipped.
func TestMatchPromiseCatchupRewindWhileInFlightIsKept(t *testing.T) {
	reliable := &gatedReliable{gate: make(chan struct{})}
	match, err := NewMatch(MatchConfig{
		Sequencer:          SequencerConfig{Players: []PlayerID{1}, MaxInputBytes: 8},
		CatchupBatchFrames: 64,
		CatchupSendWait:    time.Millisecond,
		Datagrams:          newRecordingTransport(),
		Reliable:           reliable,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(match.Close)
	if err := match.Attach(1, 1); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if _, err := match.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := match.StartCatchup(1, 6); err != nil {
		t.Fatal(err)
	}
	if _, err := match.Tick(ctx); err != nil { // page 6..11 launched, held at the gate
		t.Fatal(err)
	}
	if err := match.StartCatchup(1, 2); err != nil { // rewind while in flight
		t.Fatal(err)
	}
	for i := 0; match.CatchingUp(1) && i < 50; i++ {
		select {
		case reliable.gate <- struct{}{}:
		case <-time.After(time.Second):
		}
		time.Sleep(5 * time.Millisecond)
		if _, err := match.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if match.CatchingUp(1) {
		t.Fatal("catch-up did not finish")
	}
	reliable.mu.Lock()
	received := decodeAll(t, reliable.pages)
	reliable.mu.Unlock()
	for id := FrameID(2); id <= 5; id++ {
		if _, ok := received[id]; !ok {
			t.Fatalf("frame %d skipped: the rewind was lost when the in-flight page succeeded", id)
		}
	}
}
