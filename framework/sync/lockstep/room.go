// Package lockstep is deterministic input-frame synchronization bound to
// rooms and transports: a Room owns one match's
// sequencer, frame history, redundant broadcast encoder and desync detector,
// broadcasts cut frames to attached sessions over the datagram lane (loss is
// healed by frame redundancy, never by retransmission), and pages catch-up
// frames to reconnecting sessions over the reliable lane (KCP, QUIC or the
// AEAD UDP transport via nettransport's senders).
//
// A Room is single-owner state, like the sequencer it wraps: the room
// entity's serial handler drives Attach/SubmitInput/Tick — no locks, in line
// with the nest execution model. One Room serves exactly one match; call
// Close when the match ends.
//
// Transport note: wire Datagrams to a raw transport sender (UDP/KCP/QUIC).
// nettransport.AsyncTransport is reliable-only and is the wrong place for
// lockstep frames anyway: a queue that could fold or delay input frames would
// silently lose more than redundancy can heal.
package lockstep

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/nettransport"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

var (
	ErrRoomConfigInvalid  = errors.New("lockstep room: invalid config")
	ErrRoomClosed         = errors.New("lockstep room: room is closed")
	ErrPlayerDetached     = errors.New("lockstep room: player has no attached session")
	ErrSessionInUse       = errors.New("lockstep room: session already bound to another receiver")
	ErrCatchupUnavailable = errors.New("lockstep room: no reliable sender for catch-up")
	ErrCatchupFuture      = errors.New("lockstep room: catch-up start beyond the next frame")
	ErrCatchupUnservable  = errors.New("lockstep room: catch-up range trimmed from history")
	ErrHashFrameInvalid   = errors.New("lockstep room: hash report for an uncut or invalid frame")
)

// Metric names. They are part of the package contract: the alert rules in
// OBSERVABILITY.md are keyed on them, and a guard test pins the set
// (RR-20261006-61 — the consolidation once dropped the prefix and the desync
// alert went silent).
const (
	MetricFrames        = "lockstep.frame.total"
	MetricInputLate     = "lockstep.input.late.total"
	MetricInputRejected = "lockstep.input.rejected.total"
	MetricCatchupFrames = "lockstep.catchup.frames.total"
	MetricDesync        = "lockstep.desync.total"
)

// DefaultMaxDatagramBytes is the datagram PAYLOAD bound used when neither
// the config nor the datagram sender declares one: the protocol transports'
// default (nettransport.DefaultMaxDatagram — KCP / QUIC payload, and the
// AEAD UDP transport's 1232-byte packet minus its 32-byte envelope).
const DefaultMaxDatagramBytes = nettransport.DefaultMaxDatagram

// Catch-up send defaults (see RoomConfig.CatchupSendTimeout / CatchupSendWait).
const (
	DefaultCatchupSendTimeout = 2 * time.Second
	DefaultCatchupSendWait    = 5 * time.Millisecond
)

// RoomConfig shapes one match's lockstep room.
type RoomConfig struct {
	// Sequencer fixes the seat set, submit window and per-input payload cap
	// (see the Sequencer in this package).
	Sequencer SequencerConfig
	// RedundancyDepth is how many recent frames each broadcast datagram
	// carries (normalized via NormalizeRedundancyDepth: <= 0
	// selects 3, MaxBroadcastFrames is the ceiling). Depth N heals up to
	// N-1 consecutive lost datagrams without retransmission.
	RedundancyDepth int
	// MaxDatagramBytes is the datagram PAYLOAD bound the room's broadcast
	// must fit — what the sender accepts, after its own envelope (AEAD
	// header and tag, framing). Zero takes the bound the Datagrams sender
	// declares (nettransport.DatagramPayloadLimiter: the KCP, QUIC and UDP
	// transports do), else DefaultMaxDatagramBytes; a value above the
	// sender's declared bound is refused. NewRoom refuses configurations
	// whose worst-case packet — depth × players × max payload plus wire
	// overhead, i.e. every seat at full payload — exceeds it: a single
	// full-payload client must never be able to push the room's broadcast
	// past what the transport can send (RR-20261006-62).
	MaxDatagramBytes int
	// HashQuorum is the minimum number of AGREEING hash reports before a
	// keyframe is judged (<= 0 derives majority-of-seats: len(players)/2+1
	// — with that choice a colluding minority reporting first can never
	// convict an honest player). When every seat has reported and no hash
	// reaches the quorum — a two-seat room with different hashes, a 2:2
	// split, all different — the frame is ruled with no majority: every
	// seat is an outlier and the verdict has NoMajority set
	// (RR-20261006-64).
	HashQuorum int
	// CatchupBatchFrames is how many history frames one catching-up session
	// receives per tick over the reliable lane (<= 0 selects 32; capped at
	// MaxBroadcastFrames; 1 is refused). The per-tick cap is the rate limit
	// that keeps a 10s reconnect from flooding the link in one burst — but
	// every tick also cuts one new frame, so a batch of 1 pages exactly as
	// fast as the head moves and the backlog never closes (RR-20260914-05).
	// The net catch-up rate is batch-1 frames per tick; it must be positive.
	CatchupBatchFrames int
	// CatchupMaxFailures abandons a catch-up after this many consecutive
	// reliable-lane send failures (<= 0 selects 8) instead of pinning the
	// session forever in a state where it receives neither live frames nor
	// history. The abandonment surfaces in Tick's joined error.
	CatchupMaxFailures int
	// CatchupSendTimeout bounds one catch-up page send on the reliable lane
	// (<= 0 selects DefaultCatchupSendTimeout). A send still blocked past
	// it — a peer that stopped acknowledging, a full KCP window — fails and
	// counts toward CatchupMaxFailures.
	CatchupSendTimeout time.Duration
	// CatchupSendWait is how long Tick waits for the pages it launched in
	// this tick (<= 0 selects DefaultCatchupSendWait). Pages are sent off
	// the room's goroutine, at most one in flight per session; a page that
	// does not finish within the wait stays in flight and is collected on
	// a later Tick, so a slow client delays only its own catch-up, never
	// the room (RR-20261006-63). Tick never waits on a page launched by an
	// earlier tick.
	CatchupSendWait time.Duration
	// Datagrams broadcasts cut frames (required). Loss-tolerant lane: the
	// AEAD UDP transport, or any raw DatagramSender (see the package note).
	Datagrams nettransport.DatagramSender
	// Reliable pages catch-up frames to reconnecting sessions (optional;
	// StartCatchup fails without it). KCP or QUIC transports fit here: the
	// Room bounds every page send with CatchupSendTimeout and never blocks
	// Tick on it, so the caller's context needs no deadline.
	Reliable nettransport.ReliableSender
	// OnDesync is invoked whenever a keyframe ruling gains outliers that
	// were not surfaced before (set difference, not cardinality — an
	// equal-size flip of the outlier set fires too). Nil ignores verdicts.
	OnDesync func(DesyncVerdict)
}

// catchupState is one session's paging cursor.
type catchupState struct {
	next     FrameID
	failures int
	// inflight is the page currently being sent, or nil. At most one page
	// per session is in flight; the cursor moves only when it succeeds.
	inflight *catchupSend
}

// catchupSend is one page handed to the reliable lane off the room's
// goroutine. done is buffered so the sender never blocks on a room that
// stopped caring (detach, close).
type catchupSend struct {
	start    FrameID // cursor when the page was read
	end      FrameID // cursor after this page succeeds
	frames   int
	done     chan error
	cancel   context.CancelFunc
	finished bool
	err      error
}

// poll collects the send's result without blocking.
func (s *catchupSend) poll() bool {
	if !s.finished {
		select {
		case s.err = <-s.done:
			s.finished = true
		default:
		}
	}
	return s.finished
}

// drop releases a send whose result no longer matters.
func (s *catchupState) drop() {
	if s.inflight != nil {
		s.inflight.cancel()
		s.inflight = nil
	}
}

// Room is one match's server-side lockstep state.
type Room struct {
	sequencer    *Sequencer
	history      *History
	encoder      *RedundantEncoder
	detector     *DesyncDetector
	datagrams    nettransport.DatagramSender
	reliable     nettransport.ReliableSender
	onDesync     func(DesyncVerdict)
	depth        int
	catchupBatch int
	catchupMax   int
	sendTimeout  time.Duration
	sendWait     time.Duration
	closed       bool
	// sessions binds attached seats to their transport session.
	sessions map[PlayerID]nettransport.SessionID
	// spectators are receive-only sessions: they get live broadcasts and
	// may catch up, but hold no seat and submit nothing.
	spectators map[nettransport.SessionID]struct{}
	// sessionOwners tracks which receiver (seat or spectator) holds each
	// session id, so one session can never serve two receivers.
	sessionOwners map[nettransport.SessionID]PlayerID // spectators use ownerSpectator
	// catchups holds each catching-up session's paging state.
	catchups map[nettransport.SessionID]*catchupState
	// ruled tracks the outliers already surfaced per judged frame, so
	// OnDesync fires exactly on set growth/change, not cardinality change.
	ruled map[FrameID]map[PlayerID]struct{}
}

// ownerSpectator marks a session owned by a spectator in sessionOwners.
const ownerSpectator PlayerID = -1

// NewRoom builds a lockstep room for one match.
func NewRoom(config RoomConfig) (*Room, error) {
	if config.Datagrams == nil {
		return nil, fmt.Errorf("%w: datagram sender is required", ErrRoomConfigInvalid)
	}
	sequencer, err := NewSequencer(config.Sequencer)
	if err != nil {
		return nil, err
	}
	depth := NormalizeRedundancyDepth(config.RedundancyDepth)
	maxDatagram, err := datagramBudget(config.MaxDatagramBytes, config.Datagrams)
	if err != nil {
		return nil, err
	}
	// Worst-case broadcast packet: header + depth frames, each carrying
	// every seat at the full payload cap plus varint overhead. Refusing the
	// configuration here is what keeps a single full-payload client from
	// blacking out the whole room's downlink at runtime. The bound is the
	// sender's PAYLOAD bound — its own envelope is already taken off.
	const packetHeader = 2 + 5  // magic+version + frame count varint
	const frameOverhead = 5 + 5 // frame id + input count varints
	const inputOverhead = 5 + 5 // player id + payload length varints
	players := len(config.Sequencer.Players)
	worst := packetHeader + depth*(frameOverhead+players*(inputOverhead+sequencer.MaxInputBytes()))
	if worst > maxDatagram {
		return nil, fmt.Errorf("%w: worst-case packet %dB (depth %d × %d players × %dB payload) exceeds datagram bound %dB — lower Sequencer.MaxInputBytes or RedundancyDepth", ErrRoomConfigInvalid, worst, depth, players, sequencer.MaxInputBytes(), maxDatagram)
	}
	batch := config.CatchupBatchFrames
	if batch <= 0 {
		batch = 32
	}
	if batch == 1 {
		return nil, fmt.Errorf("%w: catch-up batch of 1 frame per tick can never overtake the head (one frame is cut per tick); use >= 2", ErrRoomConfigInvalid)
	}
	if batch > MaxBroadcastFrames {
		batch = MaxBroadcastFrames
	}
	maxFailures := config.CatchupMaxFailures
	if maxFailures <= 0 {
		maxFailures = 8
	}
	quorum := config.HashQuorum
	if quorum <= 0 {
		quorum = players/2 + 1
	}
	detector := NewDesyncDetector(quorum)
	detector.seats = players
	sendTimeout := config.CatchupSendTimeout
	if sendTimeout <= 0 {
		sendTimeout = DefaultCatchupSendTimeout
	}
	sendWait := config.CatchupSendWait
	if sendWait <= 0 {
		sendWait = DefaultCatchupSendWait
	}
	return &Room{
		sequencer:     sequencer,
		history:       NewHistory(),
		encoder:       NewRedundantEncoder(depth),
		detector:      detector,
		datagrams:     config.Datagrams,
		reliable:      config.Reliable,
		onDesync:      config.OnDesync,
		depth:         depth,
		catchupBatch:  batch,
		catchupMax:    maxFailures,
		sendTimeout:   sendTimeout,
		sendWait:      sendWait,
		sessions:      make(map[PlayerID]nettransport.SessionID),
		spectators:    make(map[nettransport.SessionID]struct{}),
		sessionOwners: make(map[nettransport.SessionID]PlayerID),
		catchups:      make(map[nettransport.SessionID]*catchupState),
		ruled:         make(map[FrameID]map[PlayerID]struct{}),
	}, nil
}

// datagramBudget resolves the payload bound the room's worst-case broadcast
// must fit: the sender's declared bound unless the config narrows it, and
// never more than the sender can carry.
func datagramBudget(configured int, sender nettransport.DatagramSender) (int, error) {
	declared := 0
	if limiter, ok := sender.(nettransport.DatagramPayloadLimiter); ok {
		declared = limiter.MaxDatagramPayload()
	}
	switch {
	case configured < 0:
		return 0, fmt.Errorf("%w: MaxDatagramBytes %d is negative", ErrRoomConfigInvalid, configured)
	case configured == 0 && declared > 0:
		return declared, nil
	case configured == 0:
		return DefaultMaxDatagramBytes, nil
	case declared > 0 && configured > declared:
		return 0, fmt.Errorf("%w: MaxDatagramBytes %dB exceeds the datagram sender's payload bound %dB", ErrRoomConfigInvalid, configured, declared)
	default:
		return configured, nil
	}
}

// Attach binds a seat to a transport session; the session starts receiving
// live frame broadcasts on the next Tick. Re-attaching the SAME session is
// idempotent and keeps a pending catch-up; attaching a NEW session
// (reconnect) replaces the previous one and drops its catch-up. A session
// already serving another seat or a spectator is refused — two receivers on
// one session would double-send and cross-cancel each other's catch-up.
func (r *Room) Attach(player PlayerID, session nettransport.SessionID) error {
	if r.closed {
		return ErrRoomClosed
	}
	if !r.sequencer.KnownPlayer(player) {
		return ErrPlayerUnknown
	}
	if owner, bound := r.sessionOwners[session]; bound && owner != player {
		return ErrSessionInUse
	}
	if previous, attached := r.sessions[player]; attached {
		if previous == session {
			return nil // idempotent re-attach keeps the catch-up cursor
		}
		r.dropCatchup(previous)
		delete(r.sessionOwners, previous)
	}
	r.sessions[player] = session
	r.sessionOwners[session] = player
	return nil
}

// Detach unbinds a seat (disconnect). The seat stays in the match — its
// inputs simply stop arriving, which optimistic frame locking already
// tolerates as empty inputs.
func (r *Room) Detach(player PlayerID) {
	if session, attached := r.sessions[player]; attached {
		r.dropCatchup(session)
		delete(r.sessionOwners, session)
		delete(r.sessions, player)
	}
}

// DetachSession 在 Room 的串行 owner 中处理断线通知，仅移除通知所属的生命周期。
// Gate 的 receiver ID 在进程内不复用；旧连接的迟到关闭不能拆掉已经重绑的新连接。
// 完整 Gate/Game/BindID 校验仍由接入层完成，Room 不重复持有连接身份表。
func (r *Room) DetachSession(player PlayerID, session nettransport.SessionID) bool {
	current, attached := r.sessions[player]
	if !attached || current != session {
		return false
	}
	r.Detach(player)
	return true
}

// AttachSpectator binds a receive-only session: it gets live broadcasts and
// may catch up via SpectatorCatchup, but holds no seat.
func (r *Room) AttachSpectator(session nettransport.SessionID) error {
	if r.closed {
		return ErrRoomClosed
	}
	if owner, bound := r.sessionOwners[session]; bound && owner != ownerSpectator {
		return ErrSessionInUse
	}
	r.spectators[session] = struct{}{}
	r.sessionOwners[session] = ownerSpectator
	return nil
}

// DetachSpectator unbinds a spectator session.
func (r *Room) DetachSpectator(session nettransport.SessionID) {
	if _, ok := r.spectators[session]; ok {
		delete(r.spectators, session)
		delete(r.sessionOwners, session)
		r.dropCatchup(session)
	}
}

// dropCatchup ends a session's catch-up and cancels its in-flight page.
func (r *Room) dropCatchup(session nettransport.SessionID) {
	if state, ok := r.catchups[session]; ok {
		state.drop()
		delete(r.catchups, session)
	}
}

// NextFrame is the id the next Tick will cut.
func (r *Room) NextFrame() FrameID { return r.sequencer.NextFrame() }

// SubmitInput feeds one player's input into the sequencer and returns the
// frame it was folded into. Late inputs (frame already cut) are folded
// forward and metered as lockstep.input.late.total; rejected inputs are
// metered as lockstep.input.rejected.total{reason} — the first signal of a
// malicious or version-skewed client.
func (r *Room) SubmitInput(player PlayerID, frame FrameID, payload []byte) (FrameID, error) {
	if r.closed {
		return 0, ErrRoomClosed
	}
	late := frame != 0 && frame < r.sequencer.NextFrame()
	folded, err := r.sequencer.SubmitInput(player, frame, payload)
	if err != nil {
		metrics.IncCounter(MetricInputRejected, metrics.Labels{"reason": rejectReason(err)}, 1)
		return 0, err
	}
	if late {
		metrics.IncCounter(MetricInputLate, nil, 1)
	}
	return folded, nil
}

func rejectReason(err error) string {
	switch {
	case errors.Is(err, ErrPlayerUnknown):
		return "unknown_player"
	case errors.Is(err, ErrFrameTooEarly):
		return "too_early"
	case errors.Is(err, ErrPayloadTooBig):
		return "payload_too_big"
	default:
		return "other"
	}
}

// Tick cuts the next frame, records it in history, and broadcasts the
// redundant packet to every attached session (seats and spectators, in
// deterministic order); it then pages pending catch-ups over the reliable
// lane. Broadcast errors don't stop the frame — the frame is cut and
// history is authoritative regardless of delivery — but they are joined and
// returned so the caller can drop dead sessions.
//
// Catch-up pages are sent off the room's goroutine (one in flight per
// session, each bounded by CatchupSendTimeout) and Tick waits at most
// CatchupSendWait for the pages it launched: a client whose reliable lane
// stalls slows only its own catch-up, never the room (RR-20261006-63).
func (r *Room) Tick(ctx context.Context) (Frame, error) {
	if r.closed {
		return Frame{}, ErrRoomClosed
	}
	frame := r.sequencer.Advance()
	r.history.Append(frame)
	packet := r.encoder.Push(frame)
	metrics.IncCounter(MetricFrames, nil, 1)

	var errs []error
	// Pages that finished since the last tick first: a session whose
	// remaining backlog rides in this tick's redundant packet goes live now.
	if err := r.collectCatchups(); err != nil {
		errs = append(errs, err)
	}
	for _, receiver := range r.broadcastOrder() {
		if _, catching := r.catchups[receiver.session]; catching {
			continue // live frames resume once the catch-up pages reach the head
		}
		if err := r.datagrams.SendDatagram(ctx, receiver.session, packet); err != nil {
			errs = append(errs, fmt.Errorf("receiver %d session %d: %w", receiver.owner, receiver.session, err))
		}
	}
	if err := r.pumpCatchup(ctx); err != nil {
		errs = append(errs, err)
	}
	return frame, errors.Join(errs...)
}

type broadcastReceiver struct {
	owner   PlayerID
	session nettransport.SessionID
}

// broadcastOrder returns seats (by ascending player) then spectators (by
// ascending session): deterministic iteration keeps error text and delivery
// order reproducible.
func (r *Room) broadcastOrder() []broadcastReceiver {
	receivers := make([]broadcastReceiver, 0, len(r.sessions)+len(r.spectators))
	players := make([]PlayerID, 0, len(r.sessions))
	for player := range r.sessions {
		players = append(players, player)
	}
	slices.Sort(players)
	for _, player := range players {
		receivers = append(receivers, broadcastReceiver{owner: player, session: r.sessions[player]})
	}
	specs := make([]nettransport.SessionID, 0, len(r.spectators))
	for session := range r.spectators {
		specs = append(specs, session)
	}
	slices.Sort(specs)
	for _, session := range specs {
		receivers = append(receivers, broadcastReceiver{owner: ownerSpectator, session: session})
	}
	return receivers
}

// StartCatchup begins paging history to the player's attached session,
// starting at from (0 = from the beginning; values beyond the next uncut
// frame are refused). Pages of CatchupBatchFrames go out over the reliable
// lane once per Tick until the cursor reaches the head, then the session
// switches back to live datagram broadcasts. Calling again while already
// catching up moves the cursor to min(current, from) — it never re-pages
// forward past history the client is still missing.
func (r *Room) StartCatchup(player PlayerID, from FrameID) error {
	session, attached := r.sessions[player]
	if !attached {
		return ErrPlayerDetached
	}
	return r.startCatchup(session, from)
}

// SpectatorCatchup begins paging history to an attached spectator session.
func (r *Room) SpectatorCatchup(session nettransport.SessionID, from FrameID) error {
	if _, ok := r.spectators[session]; !ok {
		return ErrPlayerDetached
	}
	return r.startCatchup(session, from)
}

func (r *Room) startCatchup(session nettransport.SessionID, from FrameID) error {
	if r.closed {
		return ErrRoomClosed
	}
	if r.reliable == nil {
		return ErrCatchupUnavailable
	}
	if from > r.sequencer.NextFrame() {
		return fmt.Errorf("%w: from %d, next %d", ErrCatchupFuture, from, r.sequencer.NextFrame())
	}
	if existing, catching := r.catchups[session]; catching {
		if from < existing.next {
			existing.next = from
		}
		return nil
	}
	r.catchups[session] = &catchupState{next: from}
	return nil
}

// CatchingUp reports whether the player's session is still paging history.
func (r *Room) CatchingUp(player PlayerID) bool {
	session, attached := r.sessions[player]
	if !attached {
		return false
	}
	_, catching := r.catchups[session]
	return catching
}

// catchupSessions lists catching-up sessions in ascending order, so error
// text and send order are reproducible.
func (r *Room) catchupSessions() []nettransport.SessionID {
	sessions := make([]nettransport.SessionID, 0, len(r.catchups))
	for session := range r.catchups {
		sessions = append(sessions, session)
	}
	slices.Sort(sessions)
	return sessions
}

// collectCatchups settles pages that finished since the previous tick. A
// session whose cursor now falls inside this tick's redundant packet (the
// frames from Latest-depth+1 on) is caught up: the broadcast that follows
// carries the rest. Without this a page that always outlives the wait
// would trail the head by one frame forever.
func (r *Room) collectCatchups() error {
	if len(r.catchups) == 0 {
		return nil
	}
	var errs []error
	for _, session := range r.catchupSessions() {
		state := r.catchups[session]
		if state.inflight == nil || !state.inflight.poll() {
			continue
		}
		if err := r.settleCatchup(session, state); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, catching := r.catchups[session]; catching && state.inflight == nil && state.next+FrameID(r.depth) > r.history.Latest() {
			delete(r.catchups, session)
		}
	}
	return errors.Join(errs...)
}

// settleCatchup applies a finished page's result: success moves the cursor
// (and ends the catch-up once it passes the head); failure keeps the cursor
// and counts toward CatchupMaxFailures.
func (r *Room) settleCatchup(session nettransport.SessionID, state *catchupState) error {
	send := state.inflight
	state.inflight = nil
	send.cancel()
	if send.err != nil {
		state.failures++
		if state.failures >= r.catchupMax {
			delete(r.catchups, session)
			return fmt.Errorf("catchup session %d abandoned after %d failures: %w — the client must reconnect", session, state.failures, send.err)
		}
		return fmt.Errorf("catchup session %d: %w", session, send.err)
	}
	state.failures = 0
	metrics.IncCounter(MetricCatchupFrames, nil, int64(send.frames))
	if state.next >= send.start {
		state.next = send.end
	} // else StartCatchup rewound the cursor while the page was in flight: keep it
	if state.next > r.history.Latest() {
		delete(r.catchups, session)
	}
	return nil
}

// pumpCatchup launches the next page for every catching-up session that has
// none in flight, then waits up to CatchupSendWait for those pages. A page
// still in flight after the wait is collected by a later Tick; Tick never
// waits on it again.
func (r *Room) pumpCatchup(ctx context.Context) error {
	if len(r.catchups) == 0 {
		return nil
	}
	var errs []error
	var launched []nettransport.SessionID
	for _, session := range r.catchupSessions() {
		state := r.catchups[session]
		if state.inflight != nil {
			continue // still sending: only this session waits
		}
		// History trimmed past the cursor: the gap can never be served —
		// abandon loudly instead of paging a stream with a hole in it.
		if first := r.history.FirstID(); state.next != 0 && state.next < first {
			delete(r.catchups, session)
			errs = append(errs, fmt.Errorf("catchup session %d: %w: from %d, history starts at %d", session, ErrCatchupUnservable, state.next, first))
			continue
		}
		page := r.history.ReadRange(state.next, r.catchupBatch)
		if len(page) == 0 {
			delete(r.catchups, session) // caught up: live broadcasts take over
			continue
		}
		state.inflight = r.launchPage(ctx, session, state.next, page)
		launched = append(launched, session)
	}
	if len(launched) == 0 {
		return errors.Join(errs...)
	}
	wait := time.NewTimer(r.sendWait)
	defer wait.Stop()
	expired := false
	for _, session := range launched {
		send := r.catchups[session].inflight
		if !expired {
			select {
			case send.err = <-send.done:
				send.finished = true
			case <-wait.C:
				expired = true
			case <-ctx.Done():
				expired = true
			}
		}
		if !send.poll() {
			continue
		}
		if err := r.settleCatchup(session, r.catchups[session]); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// launchPage sends one page on its own goroutine. The send keeps the
// caller's context values but not its cancellation (it may outlive the
// Tick), and is bounded by CatchupSendTimeout; dropping the catch-up
// cancels it.
func (r *Room) launchPage(ctx context.Context, session nettransport.SessionID, start FrameID, page []Frame) *catchupSend {
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.sendTimeout)
	send := &catchupSend{
		start:  start,
		end:    page[len(page)-1].ID + 1,
		frames: len(page),
		done:   make(chan error, 1),
		cancel: cancel,
	}
	payload := EncodeBroadcast(page)
	reliable := r.reliable
	go func() {
		send.done <- reliable.SendReliable(sendCtx, session, payload)
	}()
	return send
}

// ReportHash records one player's keyframe simulation hash. Reports are
// validated (seat must exist, the frame must already be cut) so a client
// cannot inflate detector state with forged seats or future frames. Once a
// hash gains quorum agreeing reports the ruling runs; OnDesync fires
// whenever the outlier SET changes (new members counted in
// lockstep.desync.total), not merely when it grows in size.
func (r *Room) ReportHash(player PlayerID, frame FrameID, hash uint64) error {
	if r.closed {
		return ErrRoomClosed
	}
	if !r.sequencer.KnownPlayer(player) {
		metrics.IncCounter(MetricInputRejected, metrics.Labels{"reason": "hash_unknown_player"}, 1)
		return ErrPlayerUnknown
	}
	if frame == 0 || frame > r.history.Latest() {
		metrics.IncCounter(MetricInputRejected, metrics.Labels{"reason": "hash_invalid_frame"}, 1)
		return fmt.Errorf("%w: frame %d, latest %d", ErrHashFrameInvalid, frame, r.history.Latest())
	}
	verdict, ready := r.detector.Report(player, frame, hash)
	if !ready || len(verdict.Outliers) == 0 {
		return nil
	}
	surfaced := r.ruled[frame]
	changed := len(verdict.Outliers) != len(surfaced)
	newOutliers := 0
	for _, outlier := range verdict.Outliers {
		if _, seen := surfaced[outlier]; !seen {
			changed = true
			newOutliers++
		}
	}
	if !changed {
		return nil
	}
	next := make(map[PlayerID]struct{}, len(verdict.Outliers))
	for _, outlier := range verdict.Outliers {
		next[outlier] = struct{}{}
	}
	r.ruled[frame] = next
	if newOutliers > 0 {
		metrics.IncCounter(MetricDesync, nil, int64(newOutliers))
	}
	if r.onDesync != nil {
		r.onDesync(verdict)
	}
	return nil
}

// TrimHashReports drops hash-report state for frames before the given id
// (already judged and acted on). Trimmed frames are tombstoned in the
// detector: late reports cannot rebuild a forgeable report set for them.
func (r *Room) TrimHashReports(before FrameID) {
	r.detector.Trim(before)
	for frame := range r.ruled {
		if frame < before {
			delete(r.ruled, frame)
		}
	}
}

// TrimHistory drops stored frames before keep, bounding memory for long
// matches that do not need the full replay. Catch-ups older than keep are
// abandoned on their next pump with ErrCatchupUnservable.
func (r *Room) TrimHistory(keep FrameID) {
	r.history.TrimBefore(keep)
}

// History exposes the match's frame history (catch-up source and replay
// artifact). The returned structure is single-owner state: use it only from
// the same serial handler that drives the Room, and do not mutate frames.
func (r *Room) History() *History { return r.history }

// Close ends the match: all receivers and pending catch-ups are released
// and every subsequent operation fails with ErrRoomClosed. One Room serves
// exactly one match — do not reuse it.
func (r *Room) Close() {
	for _, state := range r.catchups {
		state.drop()
	}
	r.closed = true
	r.sessions = make(map[PlayerID]nettransport.SessionID)
	r.spectators = make(map[nettransport.SessionID]struct{})
	r.sessionOwners = make(map[nettransport.SessionID]PlayerID)
	r.catchups = make(map[nettransport.SessionID]*catchupState)
	r.ruled = make(map[FrameID]map[PlayerID]struct{})
}
