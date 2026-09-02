// Package lockstep binds cube-core/lockstep (deterministic input-frame
// synchronization) to rooms and transports: a Room owns one match's
// sequencer, frame history, redundant broadcast encoder and desync detector,
// broadcasts cut frames to attached sessions over the datagram lane (loss is
// healed by frame redundancy, never by retransmission), and pages catch-up
// frames to reconnecting sessions over the reliable lane (KCP, QUIC or the
// AEAD UDP transport via the replication package's senders).
//
// A Room is single-owner state, like the sequencer it wraps: the room
// entity's serial handler drives Attach/SubmitInput/Tick — no locks, in line
// with the nest execution model. One Room serves exactly one match; call
// Close when the match ends.
//
// Transport note: wire Datagrams to a raw transport sender (UDP/KCP/QUIC).
// Do NOT route lockstep frames through nettransport.AsyncTransport's
// datagram lane — its latest-only per-stream folding is built for STATE
// frames (a newer state replaces an older one); lockstep INPUT frames are
// each irreplaceable, and folding under transient congestion silently loses
// frames beyond what redundancy can heal.
package lockstep

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/tjbdwanghaibo/cube-core/lockstep"
	"github.com/tjbdwanghaibo/cube-core/metrics"
	corestate "github.com/tjbdwanghaibo/cube-core/statesync"
	"github.com/tjbdwanghaibo/cube-kit/nettransport"
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

// DefaultMaxDatagramBytes matches the replication UDP transport's default
// packet bound (IPv6 minimum MTU minus headers).
const DefaultMaxDatagramBytes = 1232

// RoomConfig shapes one match's lockstep room.
type RoomConfig struct {
	// Sequencer fixes the seat set, submit window and per-input payload cap
	// (see cube-core/lockstep).
	Sequencer lockstep.SequencerConfig
	// RedundancyDepth is how many recent frames each broadcast datagram
	// carries (normalized via lockstep.NormalizeRedundancyDepth: <= 0
	// selects 3, MaxBroadcastFrames is the ceiling). Depth N heals up to
	// N-1 consecutive lost datagrams without retransmission.
	RedundancyDepth int
	// MaxDatagramBytes is the transport's datagram payload bound (zero
	// selects DefaultMaxDatagramBytes). NewRoom refuses configurations
	// whose worst-case packet — depth × players × max payload plus wire
	// overhead — exceeds it: a single full-payload client must never be
	// able to push the room's broadcast past what the transport can send.
	MaxDatagramBytes int
	// HashQuorum is the minimum number of AGREEING hash reports before a
	// keyframe is judged (<= 0 derives majority-of-seats: len(players)/2+1
	// — with that choice a colluding minority reporting first can never
	// convict an honest player).
	HashQuorum int
	// CatchupBatchFrames is how many history frames one catching-up session
	// receives per tick over the reliable lane (<= 0 selects 32; capped at
	// lockstep.MaxBroadcastFrames). The per-tick cap is the rate limit that
	// keeps a 10s reconnect from flooding the link in one burst.
	CatchupBatchFrames int
	// CatchupMaxFailures abandons a catch-up after this many consecutive
	// reliable-lane send failures (<= 0 selects 8) instead of pinning the
	// session forever in a state where it receives neither live frames nor
	// history. The abandonment surfaces in Tick's joined error.
	CatchupMaxFailures int
	// Datagrams broadcasts cut frames (required). Loss-tolerant lane: the
	// AEAD UDP transport, or any raw DatagramSender — never a latest-only
	// folding queue (see the package note).
	Datagrams nettransport.DatagramSender
	// Reliable pages catch-up frames to reconnecting sessions (optional;
	// StartCatchup fails without it). KCP or QUIC transports fit here.
	Reliable nettransport.ReliableSender
	// OnDesync is invoked whenever a keyframe ruling gains outliers that
	// were not surfaced before (set difference, not cardinality — an
	// equal-size flip of the outlier set fires too). Nil ignores verdicts.
	OnDesync func(lockstep.DesyncVerdict)
}

// catchupState is one session's paging cursor.
type catchupState struct {
	next     lockstep.FrameID
	failures int
}

// Room is one match's server-side lockstep state.
type Room struct {
	sequencer    *lockstep.Sequencer
	history      *lockstep.History
	encoder      *lockstep.RedundantEncoder
	detector     *lockstep.DesyncDetector
	datagrams    nettransport.DatagramSender
	reliable     nettransport.ReliableSender
	onDesync     func(lockstep.DesyncVerdict)
	catchupBatch int
	catchupMax   int
	closed       bool
	// sessions binds attached seats to their transport session.
	sessions map[lockstep.PlayerID]corestate.SessionID
	// spectators are receive-only sessions: they get live broadcasts and
	// may catch up, but hold no seat and submit nothing.
	spectators map[corestate.SessionID]struct{}
	// sessionOwners tracks which receiver (seat or spectator) holds each
	// session id, so one session can never serve two receivers.
	sessionOwners map[corestate.SessionID]lockstep.PlayerID // spectators use ownerSpectator
	// catchups holds each catching-up session's paging state.
	catchups map[corestate.SessionID]*catchupState
	// ruled tracks the outliers already surfaced per judged frame, so
	// OnDesync fires exactly on set growth/change, not cardinality change.
	ruled map[lockstep.FrameID]map[lockstep.PlayerID]struct{}
}

// ownerSpectator marks a session owned by a spectator in sessionOwners.
const ownerSpectator lockstep.PlayerID = -1

// NewRoom builds a lockstep room for one match.
func NewRoom(config RoomConfig) (*Room, error) {
	if config.Datagrams == nil {
		return nil, fmt.Errorf("%w: datagram sender is required", ErrRoomConfigInvalid)
	}
	sequencer, err := lockstep.NewSequencer(config.Sequencer)
	if err != nil {
		return nil, err
	}
	depth := lockstep.NormalizeRedundancyDepth(config.RedundancyDepth)
	maxDatagram := config.MaxDatagramBytes
	if maxDatagram == 0 {
		maxDatagram = DefaultMaxDatagramBytes
	}
	// Worst-case broadcast packet: header + depth frames, each carrying
	// every seat at the full payload cap plus varint overhead. Refusing the
	// configuration here is what keeps a single full-payload client from
	// blacking out the whole room's downlink at runtime.
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
	if batch > lockstep.MaxBroadcastFrames {
		batch = lockstep.MaxBroadcastFrames
	}
	maxFailures := config.CatchupMaxFailures
	if maxFailures <= 0 {
		maxFailures = 8
	}
	quorum := config.HashQuorum
	if quorum <= 0 {
		quorum = players/2 + 1
	}
	return &Room{
		sequencer:     sequencer,
		history:       lockstep.NewHistory(),
		encoder:       lockstep.NewRedundantEncoder(depth),
		detector:      lockstep.NewDesyncDetector(quorum),
		datagrams:     config.Datagrams,
		reliable:      config.Reliable,
		onDesync:      config.OnDesync,
		catchupBatch:  batch,
		catchupMax:    maxFailures,
		sessions:      make(map[lockstep.PlayerID]corestate.SessionID),
		spectators:    make(map[corestate.SessionID]struct{}),
		sessionOwners: make(map[corestate.SessionID]lockstep.PlayerID),
		catchups:      make(map[corestate.SessionID]*catchupState),
		ruled:         make(map[lockstep.FrameID]map[lockstep.PlayerID]struct{}),
	}, nil
}

// Attach binds a seat to a transport session; the session starts receiving
// live frame broadcasts on the next Tick. Re-attaching the SAME session is
// idempotent and keeps a pending catch-up; attaching a NEW session
// (reconnect) replaces the previous one and drops its catch-up. A session
// already serving another seat or a spectator is refused — two receivers on
// one session would double-send and cross-cancel each other's catch-up.
func (r *Room) Attach(player lockstep.PlayerID, session corestate.SessionID) error {
	if r.closed {
		return ErrRoomClosed
	}
	if !r.sequencer.KnownPlayer(player) {
		return lockstep.ErrPlayerUnknown
	}
	if owner, bound := r.sessionOwners[session]; bound && owner != player {
		return ErrSessionInUse
	}
	if previous, attached := r.sessions[player]; attached {
		if previous == session {
			return nil // idempotent re-attach keeps the catch-up cursor
		}
		delete(r.catchups, previous)
		delete(r.sessionOwners, previous)
	}
	r.sessions[player] = session
	r.sessionOwners[session] = player
	return nil
}

// Detach unbinds a seat (disconnect). The seat stays in the match — its
// inputs simply stop arriving, which optimistic frame locking already
// tolerates as empty inputs.
func (r *Room) Detach(player lockstep.PlayerID) {
	if session, attached := r.sessions[player]; attached {
		delete(r.catchups, session)
		delete(r.sessionOwners, session)
		delete(r.sessions, player)
	}
}

// AttachSpectator binds a receive-only session: it gets live broadcasts and
// may catch up via SpectatorCatchup, but holds no seat.
func (r *Room) AttachSpectator(session corestate.SessionID) error {
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
func (r *Room) DetachSpectator(session corestate.SessionID) {
	if _, ok := r.spectators[session]; ok {
		delete(r.spectators, session)
		delete(r.sessionOwners, session)
		delete(r.catchups, session)
	}
}

// NextFrame is the id the next Tick will cut.
func (r *Room) NextFrame() lockstep.FrameID { return r.sequencer.NextFrame() }

// SubmitInput feeds one player's input into the sequencer and returns the
// frame it was folded into. Late inputs (frame already cut) are folded
// forward and metered as lockstep.input.late.total; rejected inputs are
// metered as lockstep.input.rejected.total{reason} — the first signal of a
// malicious or version-skewed client.
func (r *Room) SubmitInput(player lockstep.PlayerID, frame lockstep.FrameID, payload []byte) (lockstep.FrameID, error) {
	if r.closed {
		return 0, ErrRoomClosed
	}
	late := frame != 0 && frame < r.sequencer.NextFrame()
	folded, err := r.sequencer.SubmitInput(player, frame, payload)
	if err != nil {
		metrics.IncCounter("lockstep.input.rejected.total", metrics.Labels{"reason": rejectReason(err)}, 1)
		return 0, err
	}
	if late {
		metrics.IncCounter("lockstep.input.late.total", nil, 1)
	}
	return folded, nil
}

func rejectReason(err error) string {
	switch {
	case errors.Is(err, lockstep.ErrPlayerUnknown):
		return "unknown_player"
	case errors.Is(err, lockstep.ErrFrameTooEarly):
		return "too_early"
	case errors.Is(err, lockstep.ErrPayloadTooBig):
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
func (r *Room) Tick(ctx context.Context) (lockstep.Frame, error) {
	if r.closed {
		return lockstep.Frame{}, ErrRoomClosed
	}
	frame := r.sequencer.Advance()
	r.history.Append(frame)
	packet := r.encoder.Push(frame)
	metrics.IncCounter("lockstep.frame.total", nil, 1)

	var errs []error
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
	owner   lockstep.PlayerID
	session corestate.SessionID
}

// broadcastOrder returns seats (by ascending player) then spectators (by
// ascending session): deterministic iteration keeps error text and delivery
// order reproducible.
func (r *Room) broadcastOrder() []broadcastReceiver {
	receivers := make([]broadcastReceiver, 0, len(r.sessions)+len(r.spectators))
	players := make([]lockstep.PlayerID, 0, len(r.sessions))
	for player := range r.sessions {
		players = append(players, player)
	}
	sort.Slice(players, func(i, j int) bool { return players[i] < players[j] })
	for _, player := range players {
		receivers = append(receivers, broadcastReceiver{owner: player, session: r.sessions[player]})
	}
	specs := make([]corestate.SessionID, 0, len(r.spectators))
	for session := range r.spectators {
		specs = append(specs, session)
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i] < specs[j] })
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
func (r *Room) StartCatchup(player lockstep.PlayerID, from lockstep.FrameID) error {
	session, attached := r.sessions[player]
	if !attached {
		return ErrPlayerDetached
	}
	return r.startCatchup(session, from)
}

// SpectatorCatchup begins paging history to an attached spectator session.
func (r *Room) SpectatorCatchup(session corestate.SessionID, from lockstep.FrameID) error {
	if _, ok := r.spectators[session]; !ok {
		return ErrPlayerDetached
	}
	return r.startCatchup(session, from)
}

func (r *Room) startCatchup(session corestate.SessionID, from lockstep.FrameID) error {
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
func (r *Room) CatchingUp(player lockstep.PlayerID) bool {
	session, attached := r.sessions[player]
	if !attached {
		return false
	}
	_, catching := r.catchups[session]
	return catching
}

func (r *Room) pumpCatchup(ctx context.Context) error {
	if len(r.catchups) == 0 {
		return nil
	}
	sessions := make([]corestate.SessionID, 0, len(r.catchups))
	for session := range r.catchups {
		sessions = append(sessions, session)
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i] < sessions[j] })
	var errs []error
	for _, session := range sessions {
		state := r.catchups[session]
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
		if err := r.reliable.SendReliable(ctx, session, lockstep.EncodeBroadcast(page)); err != nil {
			state.failures++
			if state.failures >= r.catchupMax {
				delete(r.catchups, session)
				errs = append(errs, fmt.Errorf("catchup session %d abandoned after %d failures: %w — the client must reconnect", session, state.failures, err))
			} else {
				errs = append(errs, fmt.Errorf("catchup session %d: %w", session, err))
			}
			continue
		}
		state.failures = 0
		metrics.IncCounter("lockstep.catchup.frames.total", nil, int64(len(page)))
		next := page[len(page)-1].ID + 1
		if next > r.history.Latest() {
			delete(r.catchups, session)
		} else {
			state.next = next
		}
	}
	return errors.Join(errs...)
}

// ReportHash records one player's keyframe simulation hash. Reports are
// validated (seat must exist, the frame must already be cut) so a client
// cannot inflate detector state with forged seats or future frames. Once a
// hash gains quorum agreeing reports the ruling runs; OnDesync fires
// whenever the outlier SET changes (new members counted in
// lockstep.desync.total), not merely when it grows in size.
func (r *Room) ReportHash(player lockstep.PlayerID, frame lockstep.FrameID, hash uint64) error {
	if r.closed {
		return ErrRoomClosed
	}
	if !r.sequencer.KnownPlayer(player) {
		metrics.IncCounter("lockstep.input.rejected.total", metrics.Labels{"reason": "hash_unknown_player"}, 1)
		return lockstep.ErrPlayerUnknown
	}
	if frame == 0 || frame > r.history.Latest() {
		metrics.IncCounter("lockstep.input.rejected.total", metrics.Labels{"reason": "hash_invalid_frame"}, 1)
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
	next := make(map[lockstep.PlayerID]struct{}, len(verdict.Outliers))
	for _, outlier := range verdict.Outliers {
		next[outlier] = struct{}{}
	}
	r.ruled[frame] = next
	if newOutliers > 0 {
		metrics.IncCounter("lockstep.desync.total", nil, int64(newOutliers))
	}
	if r.onDesync != nil {
		r.onDesync(verdict)
	}
	return nil
}

// TrimHashReports drops hash-report state for frames before the given id
// (already judged and acted on). Trimmed frames are tombstoned in the
// detector: late reports cannot rebuild a forgeable report set for them.
func (r *Room) TrimHashReports(before lockstep.FrameID) {
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
func (r *Room) TrimHistory(keep lockstep.FrameID) {
	r.history.TrimBefore(keep)
}

// History exposes the match's frame history (catch-up source and replay
// artifact). The returned structure is single-owner state: use it only from
// the same serial handler that drives the Room, and do not mutate frames.
func (r *Room) History() *lockstep.History { return r.history }

// Close ends the match: all receivers and pending catch-ups are released
// and every subsequent operation fails with ErrRoomClosed. One Room serves
// exactly one match — do not reuse it.
func (r *Room) Close() {
	r.closed = true
	r.sessions = make(map[lockstep.PlayerID]corestate.SessionID)
	r.spectators = make(map[corestate.SessionID]struct{})
	r.sessionOwners = make(map[corestate.SessionID]lockstep.PlayerID)
	r.catchups = make(map[corestate.SessionID]*catchupState)
	r.ruled = make(map[lockstep.FrameID]map[lockstep.PlayerID]struct{})
}
