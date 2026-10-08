package lockstep_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/metrics"
	"github.com/tjbdwanghaibo/roost-core/robot"
	"github.com/tjbdwanghaibo/roost-core/sync/lockstep"
	"github.com/tjbdwanghaibo/roost-core/sync/nettransport"
)

// End-to-end gate for the lockstep room (RR-20261006-61..65): two real
// lockstep clients (FrameAssembler + frame hasher, the production client
// shape) and one robot.LockstepBot connect to one Room over KCP on the
// loopback interface. Every seat submits a full-size input every frame, so
// broadcasts sit at the largest size the Room admits for the transport. During
// the run client B reports a corrupted keyframe hash, client A and the robot
// each lose a run of broadcasts beyond the redundancy depth, and a
// spectator's catch-up stalls on the reliable lane. The gate requires:
//
//   - no broadcast is refused by the transport (budget counts its overhead);
//   - the desync is ruled against B and lockstep.desync.total moves;
//   - A and the robot fill their gaps by catch-up and reach the head;
//   - the stalled catch-up never stalls Tick (context without deadline).
const (
	e2eTickPeriod   = 10 * time.Millisecond
	e2eFrames       = 300
	e2eKeyframe     = 10
	e2eDesyncFrame  = 50
	e2eDepth        = 1 // the depth at which per-packet wire overhead is largest relative to the budget
	e2eSeatA        = lockstep.PlayerID(1)
	e2eSeatB        = lockstep.PlayerID(2)
	e2eSeatBot      = lockstep.PlayerID(3)
	e2eSpectator    = nettransport.SessionID(4)
	e2eSpectateTick = 30
	// e2eDatagramBound is the server transport's datagram payload bound.
	e2eDatagramBound = 1150
	// e2eConvergeWithin bounds how long after the main run every client
	// must reach the head. It is well below the time the robot's default
	// 256-frame out-of-order buffer takes to overflow after its loss
	// (~2.5s at 10ms ticks): healing must come from requesting the gap, not
	// from the overflow fallback.
	e2eConvergeWithin = 1500 * time.Millisecond
)

const (
	upInput byte = iota + 1
	upHash
	upCatchup
)

func encodeUplink(kind byte, frame lockstep.FrameID, data []byte) []byte {
	message := make([]byte, 5, 5+len(data))
	message[0] = kind
	binary.BigEndian.PutUint32(message[1:], uint32(frame))
	return append(message, data...)
}

type uplinkMessage struct {
	session nettransport.SessionID
	payload []byte
}

// watchedDatagrams records every broadcast the Room hands the transport.
type watchedDatagrams struct {
	inner   nettransport.DatagramSender
	mu      sync.Mutex
	maxSize int
	refused []error
}

func (w *watchedDatagrams) SendDatagram(ctx context.Context, session nettransport.SessionID, payload []byte) error {
	err := w.inner.SendDatagram(ctx, session, payload)
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(payload) > w.maxSize {
		w.maxSize = len(payload)
	}
	if err != nil {
		w.refused = append(w.refused, fmt.Errorf("session %d, %d bytes: %w", session, len(payload), err))
	}
	return err
}

// MaxDatagramPayload forwards the transport's declared bound, if any.
func (w *watchedDatagrams) MaxDatagramPayload() int {
	if bound, ok := w.inner.(interface{ MaxDatagramPayload() int }); ok {
		return bound.MaxDatagramPayload()
	}
	return 0
}

// stallingReliable passes reliable sends through to KCP, except for one
// session whose peer has stopped acknowledging: its sends block until their
// context ends — what kcp-go's Write does once the send window is full and
// no write deadline is set.
type stallingReliable struct {
	inner   nettransport.ReliableSender
	stalled nettransport.SessionID
	release chan struct{}
	stalls  atomic.Int64
}

func (s *stallingReliable) SendReliable(ctx context.Context, session nettransport.SessionID, payload []byte) error {
	if session != s.stalled {
		return s.inner.SendReliable(ctx, session, payload)
	}
	s.stalls.Add(1)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.release:
		return errors.New("stalled peer released at test end")
	}
}

type e2eEndpoint struct {
	session   nettransport.SessionID
	transport *nettransport.KCPTransport
	inbox     chan []byte
	next      atomic.Uint32 // highest applied frame + 1
	catchups  atomic.Int64
	dropped   atomic.Int64
}

func newestFrame(packet []byte) lockstep.FrameID {
	frames, err := lockstep.DecodeBroadcast(packet)
	if err != nil || len(frames) == 0 {
		return 0
	}
	return frames[len(frames)-1].ID
}

func (e *e2eEndpoint) send(ctx context.Context, kind byte, frame lockstep.FrameID, data []byte) error {
	sendCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return e.transport.SendReliable(sendCtx, e.session, encodeUplink(kind, frame, data))
}

func TestLockstepEndToEndGate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	key := bytes.Repeat([]byte{9}, 32)
	serverCrypt, err := nettransport.NewKCPAESGCM(key)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := nettransport.ListenKCP("127.0.0.1:0", serverCrypt, 10, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	// A path MTU below the protocol default (VPN / tunnel): the Room must
	// size its budget from what the transport declares, not from a constant.
	serverConfig := nettransport.DefaultKCPTransportConfig()
	serverConfig.MaxDatagramBytes = e2eDatagramBound
	server, err := nettransport.NewKCPTransport(serverConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	connect := func(session nettransport.SessionID, drop func(lockstep.FrameID) bool) *e2eEndpoint {
		t.Helper()
		clientCrypt, err := nettransport.NewKCPAESGCM(key)
		if err != nil {
			t.Fatal(err)
		}
		clientSession, err := nettransport.DialKCP(listener.Addr().String(), clientCrypt, 10, 3)
		if err != nil {
			t.Fatal(err)
		}
		_ = clientSession.SetWriteDeadline(time.Now().Add(3 * time.Second))
		if _, err := clientSession.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		_ = clientSession.SetWriteDeadline(time.Time{})
		serverSession, err := listener.AcceptKCP()
		if err != nil {
			t.Fatal(err)
		}
		_ = serverSession.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := serverSession.Read(make([]byte, 32)); err != nil {
			t.Fatal(err)
		}
		_ = serverSession.SetReadDeadline(time.Time{})
		if err := server.BindSession(session, serverSession); err != nil {
			t.Fatal(err)
		}
		if err := server.RegisterSession(nettransport.SessionInfo{ID: session}); err != nil {
			t.Fatal(err)
		}
		client, err := nettransport.NewKCPTransport(nettransport.KCPTransportConfig{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Close() })
		if err := client.BindSession(session, clientSession); err != nil {
			t.Fatal(err)
		}
		if err := client.RegisterSession(nettransport.SessionInfo{ID: session}); err != nil {
			t.Fatal(err)
		}
		endpoint := &e2eEndpoint{session: session, transport: client, inbox: make(chan []byte, 8192)}
		if err := client.BindDatagramHandler(session, func(_ nettransport.SessionID, packet []byte) {
			if drop != nil && drop(newestFrame(packet)) {
				endpoint.dropped.Add(1)
				return
			}
			select {
			case endpoint.inbox <- packet:
			default: // a full inbox is just more loss
			}
		}); err != nil {
			t.Fatal(err)
		}
		go func() { // catch-up pages arrive on the reliable lane
			for {
				page, err := client.ReceiveReliable(ctx, session)
				if err != nil {
					return
				}
				select {
				case endpoint.inbox <- page:
				case <-ctx.Done():
					return
				}
			}
		}()
		return endpoint
	}

	dropRange := func(from, to lockstep.FrameID) func(lockstep.FrameID) bool {
		return func(id lockstep.FrameID) bool { return id >= from && id <= to }
	}
	clientA := connect(nettransport.SessionID(e2eSeatA), dropRange(60, 66))
	clientB := connect(nettransport.SessionID(e2eSeatB), nil)
	botEnd := connect(nettransport.SessionID(e2eSeatBot), dropRange(250, 254))
	spectator := connect(e2eSpectator, nil)
	_ = spectator

	// Full load: the largest per-input payload the Room admits on this
	// transport, with every seat submitting it every frame.
	datagrams := &watchedDatagrams{inner: server}
	reliable := &stallingReliable{inner: server, stalled: e2eSpectator, release: make(chan struct{})}
	defer close(reliable.release)
	seats := []lockstep.PlayerID{e2eSeatA, e2eSeatB, e2eSeatBot}
	var verdictMu sync.Mutex
	var verdicts []lockstep.DesyncVerdict
	newRoom := func(maxInput int) (*lockstep.Room, error) {
		return lockstep.NewRoom(lockstep.RoomConfig{
			Sequencer:       lockstep.SequencerConfig{Players: seats, MaxInputBytes: maxInput},
			RedundancyDepth: e2eDepth,
			Datagrams:       datagrams,
			Reliable:        reliable,
			OnDesync: func(v lockstep.DesyncVerdict) {
				verdictMu.Lock()
				verdicts = append(verdicts, v)
				verdictMu.Unlock()
			},
		})
	}
	payloadSize := 1024
	var room *lockstep.Room
	for ; payloadSize > 0; payloadSize-- {
		if room, err = newRoom(payloadSize); err == nil {
			break
		}
	}
	if room == nil {
		t.Fatal("no payload size fits the transport")
	}
	full := bytes.Repeat([]byte{0x5A}, payloadSize)
	for _, seat := range seats {
		if err := room.Attach(seat, nettransport.SessionID(seat)); err != nil {
			t.Fatal(err)
		}
	}
	if err := room.AttachSpectator(e2eSpectator); err != nil {
		t.Fatal(err)
	}
	desyncBefore := counterValue("lockstep.desync.total")

	// Real clients: assembler + hasher, input two frames ahead, keyframe
	// hash every e2eKeyframe frames, catch-up on any unhealed gap.
	runClient := func(endpoint *e2eEndpoint, player lockstep.PlayerID, corruptAt lockstep.FrameID) {
		assembler := lockstep.NewFrameAssembler(0)
		hasher := robot.NewFrameHasher()
		var requested lockstep.FrameID
		for {
			var packet []byte
			select {
			case packet = <-endpoint.inbox:
			case <-ctx.Done():
				return
			}
			frames, ingestErr := assembler.Ingest(packet)
			for _, frame := range frames {
				hasher.Fold(frame)
				endpoint.next.Store(uint32(frame.ID + 1))
				_ = endpoint.send(ctx, upInput, frame.ID+2, full)
				if frame.ID%e2eKeyframe == 0 {
					sum := hasher.Sum()
					if frame.ID == corruptAt {
						sum ^= 1 // injected desync
					}
					var data [8]byte
					binary.BigEndian.PutUint64(data[:], sum)
					_ = endpoint.send(ctx, upHash, frame.ID, data[:])
				}
			}
			if (ingestErr != nil || assembler.Gap()) && requested != assembler.Next() {
				requested = assembler.Next()
				endpoint.catchups.Add(1)
				_ = endpoint.send(ctx, upCatchup, requested, nil)
			}
		}
	}
	go runClient(clientA, e2eSeatA, 0)
	go runClient(clientB, e2eSeatB, e2eDesyncFrame)

	var botErr atomic.Value
	bot, err := robot.NewLockstepBot(robot.LockstepBotConfig{
		Player:           e2eSeatBot,
		Sink:             botSink{endpoint: botEnd, ctx: ctx},
		Input:            func(lockstep.FrameID) []byte { return full },
		KeyframeInterval: e2eKeyframe,
	})
	if err != nil {
		t.Fatal(err)
	}
	botLoopCtx, stopBot := context.WithCancel(context.Background())
	defer stopBot()
	botStats := make(chan robot.LockstepBotStats, 1)
	go func() {
		defer func() { botStats <- bot.Stats() }()
		for {
			select {
			case packet := <-botEnd.inbox:
				if err := bot.HandleBroadcast(packet); err != nil {
					botErr.Store(err)
				}
				botEnd.next.Store(uint32(bot.Next()))
			case <-botLoopCtx.Done():
				return
			}
		}
	}()

	// Server: one goroutine owns the Room. Uplink messages are applied
	// between ticks; Tick always gets a context without a deadline.
	uplink := make(chan uplinkMessage, 8192)
	for _, seat := range seats {
		session := nettransport.SessionID(seat)
		go func() {
			for {
				payload, err := server.ReceiveReliable(ctx, session)
				if err != nil {
					return
				}
				select {
				case uplink <- uplinkMessage{session: session, payload: payload}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	var (
		serverMu      sync.Mutex
		mainHead      lockstep.FrameID
		maxTick       time.Duration
		applyErrors   []error
		tickTooLarge  []error
		mainElapsed   time.Duration
		stopTicking   = make(chan struct{})
		serverStopped = make(chan struct{})
		mainDone      = make(chan struct{})
	)
	apply := func(message uplinkMessage) error {
		if len(message.payload) < 5 {
			return fmt.Errorf("short uplink %d bytes", len(message.payload))
		}
		player := lockstep.PlayerID(message.session)
		frame := lockstep.FrameID(binary.BigEndian.Uint32(message.payload[1:5]))
		data := message.payload[5:]
		switch message.payload[0] {
		case upInput:
			_, err := room.SubmitInput(player, frame, data)
			return err
		case upHash:
			return room.ReportHash(player, frame, binary.BigEndian.Uint64(data))
		case upCatchup:
			return room.StartCatchup(player, frame)
		}
		return fmt.Errorf("unknown uplink kind %d", message.payload[0])
	}
	go func() {
		defer close(serverStopped)
		ticker := time.NewTicker(e2eTickPeriod)
		defer ticker.Stop()
		start := time.Now()
		for tick := 1; ; tick++ {
			select {
			case <-ticker.C:
			case <-stopTicking:
				return
			}
		drain:
			for {
				select {
				case message := <-uplink:
					if err := apply(message); err != nil {
						serverMu.Lock()
						applyErrors = append(applyErrors, err)
						serverMu.Unlock()
					}
				default:
					break drain
				}
			}
			if tick == e2eSpectateTick {
				if err := room.SpectatorCatchup(e2eSpectator, 1); err != nil {
					serverMu.Lock()
					applyErrors = append(applyErrors, err)
					serverMu.Unlock()
				}
			}
			began := time.Now()
			frame, err := room.Tick(context.Background())
			took := time.Since(began)
			serverMu.Lock()
			if took > maxTick {
				maxTick = took
			}
			if err != nil && errors.Is(err, nettransport.ErrPayloadTooLarge) {
				tickTooLarge = append(tickTooLarge, err)
			}
			if tick == e2eFrames {
				mainHead = frame.ID
				mainElapsed = time.Since(start)
				close(mainDone)
			}
			serverMu.Unlock()
		}
	}()
	defer func() {
		select {
		case <-stopTicking:
		default:
			close(stopTicking)
		}
		cancel()
		select {
		case <-serverStopped:
		case <-time.After(5 * time.Second):
		}
	}()

	budget := e2eFrames*e2eTickPeriod + 5*time.Second
	select {
	case <-mainDone:
	case <-time.After(budget):
		t.Fatalf("the room did not cut %d frames within %v: Tick stalled (spectator catch-up stalls=%d)", e2eFrames, budget, reliable.stalls.Load())
	}

	// Convergence: every client reaches the main-run head while the room
	// keeps ticking (late losses are detected by later packets).
	converged := func() bool {
		serverMu.Lock()
		head := mainHead
		serverMu.Unlock()
		for _, endpoint := range []*e2eEndpoint{clientA, clientB, botEnd} {
			if lockstep.FrameID(endpoint.next.Load()) <= head {
				return false
			}
		}
		return true
	}
	deadline := time.Now().Add(e2eConvergeWithin)
	for !converged() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	close(stopTicking)
	<-serverStopped

	serverMu.Lock()
	defer serverMu.Unlock()
	datagrams.mu.Lock()
	defer datagrams.mu.Unlock()
	t.Logf("payload %dB, max broadcast %dB, main run %v, max Tick %v, stalls %d, A drops %d catchups %d, bot drops %d",
		payloadSize, datagrams.maxSize, mainElapsed, maxTick, reliable.stalls.Load(), clientA.dropped.Load(), clientA.catchups.Load(), botEnd.dropped.Load())

	// 1. Broadcasts never refused, and the run was genuinely at full load.
	if len(datagrams.refused) > 0 || len(tickTooLarge) > 0 {
		t.Errorf("transport refused %d broadcasts (first: %v); Tick reported %d payload-too-large errors", len(datagrams.refused), firstErr(datagrams.refused), len(tickTooLarge))
	}
	if datagrams.maxSize < len(seats)*payloadSize {
		t.Errorf("largest broadcast %dB < %d seats × %dB: the run never reached full load", datagrams.maxSize, len(seats), payloadSize)
	}
	if len(applyErrors) > 0 {
		t.Errorf("%d uplink messages failed (first: %v)", len(applyErrors), applyErrors[0])
	}
	// 2. Desync ruled against B, and metered.
	verdictMu.Lock()
	ruled := slices.ContainsFunc(verdicts, func(v lockstep.DesyncVerdict) bool {
		return v.Frame == e2eDesyncFrame && slices.Equal(v.Outliers, []lockstep.PlayerID{e2eSeatB})
	})
	verdictMu.Unlock()
	if !ruled {
		t.Errorf("desync at frame %d not ruled against seat %d; verdicts = %+v", e2eDesyncFrame, e2eSeatB, verdicts)
	}
	if delta := counterValue("lockstep.desync.total") - desyncBefore; delta < 1 {
		t.Errorf("lockstep.desync.total moved by %d, want >= 1", delta)
	}
	// 3. Catch-up healed both injected losses and nobody is stuck.
	if clientA.dropped.Load() == 0 || botEnd.dropped.Load() == 0 {
		t.Errorf("loss injection did not happen: A dropped %d, bot dropped %d", clientA.dropped.Load(), botEnd.dropped.Load())
	}
	if clientA.catchups.Load() == 0 {
		t.Error("client A never requested a catch-up for its lost frames")
	}
	stats := finishE2EBot(cancel, stopBot, botStats)
	if stats.CatchupsRequested == 0 {
		t.Error("the robot never requested a catch-up for its lost frames")
	}
	if err, _ := botErr.Load().(error); err != nil {
		t.Errorf("robot: %v", err)
	}
	for name, endpoint := range map[string]*e2eEndpoint{"A": clientA, "B": clientB, "robot": botEnd} {
		if next := lockstep.FrameID(endpoint.next.Load()); next <= mainHead {
			t.Errorf("client %s stuck at frame %d, head %d", name, next, mainHead)
		}
	}
	// 4. The stalled catch-up never stalled the room.
	if reliable.stalls.Load() == 0 {
		t.Error("the spectator's catch-up never reached the stalled lane")
	}
	if maxTick > 100*time.Millisecond {
		t.Errorf("slowest Tick %v: a stalled catch-up send is holding the room", maxTick)
	}
}

type botSink struct {
	endpoint *e2eEndpoint
	ctx      context.Context
}

func (s botSink) SubmitInput(frame lockstep.FrameID, payload []byte) error {
	return s.endpoint.send(s.ctx, upInput, frame, payload)
}

func (s botSink) ReportHash(frame lockstep.FrameID, hash uint64) error {
	var data [8]byte
	binary.BigEndian.PutUint64(data[:], hash)
	return s.endpoint.send(s.ctx, upHash, frame, data[:])
}

func (s botSink) RequestCatchup(from lockstep.FrameID) error {
	return s.endpoint.send(s.ctx, upCatchup, from, nil)
}

func counterValue(name string) int64 {
	var total int64
	for _, metric := range metrics.Snapshot() {
		if metric.Name == name {
			total += metric.Value
		}
	}
	return total
}

func firstErr(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return errs[0]
}

// finishE2EBot 汇合机器人所有者，再结束其网络依赖。
func finishE2EBot(cancelTransport, stopLoop context.CancelFunc, stats <-chan robot.LockstepBotStats) robot.LockstepBotStats {
	stopLoop()
	result := <-stats
	cancelTransport()
	return result
}
