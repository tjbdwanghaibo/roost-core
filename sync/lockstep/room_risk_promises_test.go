package lockstep

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/metrics"
	"github.com/tjbdwanghaibo/roost-core/sync/nettransport"
)

// lockstepMetricNames is the pinned metric surface of this package. The
// alert rules in OBSERVABILITY.md are keyed on these exact names; the
// consolidation commit c8249526 silently dropped the "lockstep." prefix and
// the desync alert stopped firing (RR-20261006-61).
var lockstepMetricNames = []string{
	"lockstep.catchup.frames.total",
	"lockstep.desync.total",
	"lockstep.frame.total",
	"lockstep.input.late.total",
	"lockstep.input.rejected.total",
}

// TestRoomPromiseMetricNamesArePinned drives every metered path of the Room
// and requires the emitted names to be exactly the pinned set, each listed
// in OBSERVABILITY.md (RR-20261006-61).
func TestRoomPromiseMetricNamesArePinned(t *testing.T) {
	previous := metrics.DefaultRegistry()
	metrics.SetDefaultRegistry(metrics.NewRegistry())
	t.Cleanup(func() { metrics.SetDefaultRegistry(previous) })

	transport := newRecordingTransport()
	room, err := NewRoom(RoomConfig{
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
		if err := room.Attach(player, nettransport.SessionID(100+player)); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := room.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := room.SubmitInput(1, 1, []byte{1}); err != nil { // late
		t.Fatal(err)
	}
	if _, err := room.SubmitInput(9, 0, []byte{1}); err == nil { // rejected
		t.Fatal("unknown seat accepted")
	}
	if err := room.StartCatchup(2, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := room.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	_ = room.ReportHash(1, 2, 7)
	_ = room.ReportHash(2, 2, 7)
	_ = room.ReportHash(3, 2, 8) // desync: seat 3 is the outlier

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
	doc, err := os.ReadFile(filepath.Join("..", "..", "OBSERVABILITY.md"))
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

// TestRoomPromiseBudgetCountsTransportOverhead: a configuration NewRoom
// accepts must never produce a full-load broadcast the transport refuses —
// the whole room's downlink would black out at once (RR-20261006-62).
func TestRoomPromiseBudgetCountsTransportOverhead(t *testing.T) {
	sender := &limitedDatagrams{limit: nettransport.DefaultMaxDatagram}
	room, err := NewRoom(RoomConfig{
		Sequencer:       SequencerConfig{Players: []PlayerID{1, 2}, MaxInputBytes: 595},
		RedundancyDepth: 1,
		Datagrams:       sender,
	})
	if err != nil {
		if !errors.Is(err, ErrRoomConfigInvalid) {
			t.Fatalf("NewRoom error = %v, want ErrRoomConfigInvalid", err)
		}
		return // refused up front: the promise holds
	}
	for player := PlayerID(1); player <= 2; player++ {
		if err := room.Attach(player, nettransport.SessionID(player)); err != nil {
			t.Fatal(err)
		}
	}
	fullLoadTicks(t, room)
}

// TestRoomPromiseBudgetFollowsTheSender: the budget is the sender's declared
// payload bound; a configured bound above it is refused, and the AEAD UDP
// transport declares its packet bound minus the envelope Seal adds.
func TestRoomPromiseBudgetFollowsTheSender(t *testing.T) {
	sender := &limitedDatagrams{limit: 1200}
	if _, err := NewRoom(RoomConfig{
		Sequencer:        SequencerConfig{Players: []PlayerID{1, 2}, MaxInputBytes: 8},
		MaxDatagramBytes: 1232,
		Datagrams:        sender,
	}); !errors.Is(err, ErrRoomConfigInvalid) {
		t.Fatalf("MaxDatagramBytes above the sender's bound: err = %v, want ErrRoomConfigInvalid", err)
	}
	// Largest payload admitted for 2 seats at depth 1 fits the sender.
	accepted := 0
	for size := 1024; size > 0; size-- {
		if _, err := NewRoom(RoomConfig{Sequencer: SequencerConfig{Players: []PlayerID{1, 2}, MaxInputBytes: size}, RedundancyDepth: 1, Datagrams: sender}); err == nil {
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
func fullLoadTicks(t *testing.T, room *Room) {
	t.Helper()
	full := make([]byte, 595)
	for i := 0; i < 3; i++ {
		for player := PlayerID(1); player <= 2; player++ {
			if _, err := room.SubmitInput(player, room.NextFrame(), full); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := room.Tick(context.Background()); err != nil {
			t.Fatalf("NewRoom accepted the config but the full-load broadcast of frame %d was refused: %v", i+1, err)
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

// TestRoomPromiseSlowCatchupDoesNotStallTick: one catching-up client whose
// reliable lane stalls must not stop the room — Tick keeps cutting and
// broadcasting to everyone else, with no deadline on the caller's context
// (RR-20261006-63).
func TestRoomPromiseSlowCatchupDoesNotStallTick(t *testing.T) {
	datagrams := newRecordingTransport()
	reliable := &blockingReliable{release: make(chan struct{})}
	t.Cleanup(func() { close(reliable.release) })
	room, err := NewRoom(RoomConfig{
		Sequencer: SequencerConfig{Players: []PlayerID{1, 2}, MaxInputBytes: 8},
		Datagrams: datagrams,
		Reliable:  reliable,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(room.Close)
	for player := PlayerID(1); player <= 2; player++ {
		if err := room.Attach(player, nettransport.SessionID(player)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		if _, err := room.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if err := room.StartCatchup(2, 1); err != nil {
		t.Fatal(err)
	}
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		for i := 0; i < 20; i++ {
			_, _ = room.Tick(context.Background())
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

// TestRoomPromiseTwoSeatDesyncIsRuled: in a two-seat room any hash
// disagreement must produce a verdict — the default majority quorum (2)
// can never be met by a 1:1 split (RR-20261006-64).
func TestRoomPromiseTwoSeatDesyncIsRuled(t *testing.T) {
	var verdicts []DesyncVerdict
	room, err := NewRoom(RoomConfig{
		Sequencer: SequencerConfig{Players: []PlayerID{1, 2}, MaxInputBytes: 8},
		Datagrams: newRecordingTransport(),
		OnDesync:  func(v DesyncVerdict) { verdicts = append(verdicts, v) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := room.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := room.ReportHash(1, 1, 0xA); err != nil {
		t.Fatal(err)
	}
	if err := room.ReportHash(2, 1, 0xB); err != nil {
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
	// Agreement in a two-seat room stays silent.
	if _, err := room.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = room.ReportHash(1, 2, 0xC)
	_ = room.ReportHash(2, 2, 0xC)
	if len(verdicts) != 1 {
		t.Fatalf("agreeing two-seat hashes produced a verdict: %+v", verdicts[1:])
	}
}

// TestRoomPromiseNoMajorityNeedsEverySeat: with more seats the no-majority
// ruling waits for every seat — a 1:1 split among the first two reports of
// a three-seat room is not a verdict yet, and the third report decides.
func TestRoomPromiseNoMajorityNeedsEverySeat(t *testing.T) {
	var verdicts []DesyncVerdict
	room, err := NewRoom(RoomConfig{
		Sequencer: SequencerConfig{Players: []PlayerID{1, 2, 3, 4}, MaxInputBytes: 8},
		Datagrams: newRecordingTransport(),
		OnDesync:  func(v DesyncVerdict) { verdicts = append(verdicts, v) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := room.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = room.ReportHash(1, 1, 0xA)
	_ = room.ReportHash(2, 1, 0xB)
	_ = room.ReportHash(3, 1, 0xA)
	if len(verdicts) != 0 {
		t.Fatalf("verdict before quorum or every seat: %+v", verdicts)
	}
	_ = room.ReportHash(4, 1, 0xB) // 2:2, quorum 3 unreachable
	if len(verdicts) != 1 || !verdicts[0].NoMajority || !slices.Equal(verdicts[0].Outliers, []PlayerID{1, 2, 3, 4}) {
		t.Fatalf("2:2 split verdicts = %+v, want one NoMajority naming all four seats", verdicts)
	}
}

// delayedReliable completes every send after a fixed delay — longer than
// the Room's default send wait, so every page finishes on a later Tick.
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

// TestRoomPromiseSlowButLiveCatchupConverges: when every page outlives
// Tick's wait, the catch-up still reaches the head and hands over to live
// broadcasts with no frame missing (RR-20261006-63).
func TestRoomPromiseSlowButLiveCatchupConverges(t *testing.T) {
	datagrams := newRecordingTransport()
	reliable := &delayedReliable{delay: 20 * time.Millisecond, pages: make(map[nettransport.SessionID][][]byte)}
	room, err := NewRoom(RoomConfig{
		Sequencer:          SequencerConfig{Players: []PlayerID{1, 2}, MaxInputBytes: 8},
		RedundancyDepth:    3,
		CatchupBatchFrames: 8,
		Datagrams:          datagrams,
		Reliable:           reliable,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(room.Close)
	if err := room.Attach(1, 1); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		if _, err := room.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if err := room.Attach(2, 2); err != nil {
		t.Fatal(err)
	}
	if err := room.StartCatchup(2, 1); err != nil {
		t.Fatal(err)
	}
	ticks := 0
	for ; room.CatchingUp(2) && ticks < 200; ticks++ {
		if _, err := room.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if room.CatchingUp(2) {
		t.Fatalf("catch-up with 20ms pages still running after %d ticks", ticks)
	}
	final, err := room.Tick(context.Background())
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

// TestRoomPromiseCatchupRewindWhileInFlightIsKept: StartCatchup moving the
// cursor back while a page is in flight must survive that page's success —
// the earlier frames are paged next, not skipped.
func TestRoomPromiseCatchupRewindWhileInFlightIsKept(t *testing.T) {
	reliable := &gatedReliable{gate: make(chan struct{})}
	room, err := NewRoom(RoomConfig{
		Sequencer:          SequencerConfig{Players: []PlayerID{1}, MaxInputBytes: 8},
		CatchupBatchFrames: 64,
		CatchupSendWait:    time.Millisecond,
		Datagrams:          newRecordingTransport(),
		Reliable:           reliable,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(room.Close)
	if err := room.Attach(1, 1); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if _, err := room.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := room.StartCatchup(1, 6); err != nil {
		t.Fatal(err)
	}
	if _, err := room.Tick(ctx); err != nil { // page 6..11 launched, held at the gate
		t.Fatal(err)
	}
	if err := room.StartCatchup(1, 2); err != nil { // rewind while in flight
		t.Fatal(err)
	}
	for i := 0; room.CatchingUp(1) && i < 50; i++ {
		select {
		case reliable.gate <- struct{}{}:
		case <-time.After(time.Second):
		}
		time.Sleep(5 * time.Millisecond)
		if _, err := room.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if room.CatchingUp(1) {
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
