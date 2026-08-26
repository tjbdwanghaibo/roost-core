// Package lockstep is the deterministic input-frame synchronization core:
// the server sequences player inputs into fixed-rate frames and broadcasts
// them; simulation runs on the clients (and optionally on a server-side
// arbiter), which must be bit-deterministic — fixed-point math, injected
// randomness, no wall clock (the contract cube-skill's runtime already
// satisfies).
//
// This is INPUT-frame synchronization (lockstep), not to be confused with
// the state-frame path: entitysync replicates per-subject state deltas and
// cube-kit's sync/room_replication batches them into per-room state frames.
// A lockstep frame carries only what players pressed.
//
// The sequencer implements optimistic frame locking: frames are cut on the
// host's clock, never waiting for slow clients — a missing input is an empty
// input, and late inputs are folded into the next uncut frame. The package
// has no dependencies and no transport opinions; cube-kit's lockstep package
// wires it to rooms and datagram/reliable lanes.
package lockstep

import (
	"errors"
	"fmt"
	"sort"
)

// Lockstep errors.
var (
	ErrPlayerUnknown  = errors.New("lockstep: unknown player")
	ErrFrameTooEarly  = errors.New("lockstep: input frame beyond the submit window")
	ErrConfigInvalid  = errors.New("lockstep: invalid sequencer config")
	ErrPayloadTooBig  = errors.New("lockstep: input payload exceeds limit")
	ErrFrameCorrupt   = errors.New("lockstep: frame encoding corrupt")
	ErrHistoryUnknown = errors.New("lockstep: frame not in history")
)

// PlayerID is a seat inside one match (assigned at match start).
type PlayerID int32

// FrameID is the monotonic logical frame number, starting at 1.
type FrameID uint32

// Input is one player's payload for one frame. The payload is opaque to the
// framework: the game defines its own input encoding.
type Input struct {
	Player  PlayerID
	Payload []byte
}

// Frame is one sequenced logical frame. Inputs are ordered by ascending
// PlayerID and contain only the players that submitted anything — an absent
// player IS the empty input (optimistic frame locking).
type Frame struct {
	ID     FrameID
	Inputs []Input
}

// MaxInputPayloadBytes is the absolute upper bound for one input payload;
// real lockstep inputs are a few bytes, so anything near this limit is a
// protocol bug. Matches must usually configure a much tighter
// SequencerConfig.MaxInputBytes — the kit Room refuses configurations whose
// redundancy × players × payload budget exceeds one datagram.
const MaxInputPayloadBytes = 1024

// MaxSubmitWindow bounds SubmitWindow: the pending buffer is
// (window+1) × players × payload, purely configuration-amplified.
const MaxSubmitWindow = 64

// SequencerConfig shapes a match's sequencer.
type SequencerConfig struct {
	// Players fixes the seat set for the match.
	Players []PlayerID
	// SubmitWindow is how far ahead of the next uncut frame an input may
	// target: frames next..next+window (window+1 slots, endpoints included)
	// are accepted. Protects the buffer from a client spraying far-future
	// frames. Zero selects the default of 2; the maximum is MaxSubmitWindow.
	SubmitWindow FrameID
	// MaxInputBytes bounds one input payload for this match (zero selects
	// MaxInputPayloadBytes). Real inputs are a handful of bytes: keep this
	// tight — it is the dominant term of the broadcast packet budget.
	MaxInputBytes int
}

// Sequencer turns submitted inputs into sequenced frames. It is
// single-owner state: the room's serial handler drives it — no locks, in
// line with the nest execution model.
type Sequencer struct {
	players  map[PlayerID]struct{}
	window   FrameID
	maxInput int
	next     FrameID
	pending  map[FrameID]map[PlayerID]pendingInput
}

// pendingInput remembers whether the payload arrived by late-folding: a
// folded (stale) payload must not block the player's real input for the
// frame it was folded into.
type pendingInput struct {
	payload []byte
	folded  bool
}

func NewSequencer(config SequencerConfig) (*Sequencer, error) {
	if len(config.Players) == 0 {
		return nil, fmt.Errorf("%w: at least one player", ErrConfigInvalid)
	}
	window := config.SubmitWindow
	if window == 0 {
		window = 2
	}
	if window > MaxSubmitWindow {
		return nil, fmt.Errorf("%w: submit window %d exceeds %d", ErrConfigInvalid, window, MaxSubmitWindow)
	}
	maxInput := config.MaxInputBytes
	if maxInput == 0 {
		maxInput = MaxInputPayloadBytes
	}
	if maxInput < 0 || maxInput > MaxInputPayloadBytes {
		return nil, fmt.Errorf("%w: max input bytes %d outside (0, %d]", ErrConfigInvalid, maxInput, MaxInputPayloadBytes)
	}
	players := make(map[PlayerID]struct{}, len(config.Players))
	for _, player := range config.Players {
		if _, duplicate := players[player]; duplicate {
			return nil, fmt.Errorf("%w: duplicate player %d", ErrConfigInvalid, player)
		}
		players[player] = struct{}{}
	}
	return &Sequencer{
		players:  players,
		window:   window,
		maxInput: maxInput,
		next:     1,
		pending:  make(map[FrameID]map[PlayerID]pendingInput),
	}, nil
}

// MaxInputBytes is the effective per-input payload bound for this match.
func (s *Sequencer) MaxInputBytes() int { return s.maxInput }

// NextFrame is the id the next Advance will cut.
func (s *Sequencer) NextFrame() FrameID { return s.next }

// KnownPlayer reports whether the player holds a seat in this match.
func (s *Sequencer) KnownPlayer(player PlayerID) bool {
	_, known := s.players[player]
	return known
}

// SubmitInput records a player's input for a frame and returns the frame it
// was folded into. A late input (frame already cut) folds into the next
// uncut frame — the optimistic-locking recovery the caller can meter by
// comparing the returned id with the requested one. Duplicate submissions
// for a frame keep the first payload (idempotent against datagram
// redundancy) — with one asymmetry: an explicitly targeted input always
// replaces a payload that only got there by late-folding, so a stale folded
// packet can never shadow the player's real input for the frame.
// A frame farther ahead than the submit window is rejected.
func (s *Sequencer) SubmitInput(player PlayerID, frame FrameID, payload []byte) (FrameID, error) {
	if _, known := s.players[player]; !known {
		return 0, ErrPlayerUnknown
	}
	if len(payload) > s.maxInput {
		return 0, ErrPayloadTooBig
	}
	folded := frame < s.next
	if folded {
		frame = s.next
	}
	if frame > s.next+s.window {
		return 0, ErrFrameTooEarly
	}
	inputs := s.pending[frame]
	if inputs == nil {
		inputs = make(map[PlayerID]pendingInput)
		s.pending[frame] = inputs
	}
	if existing, submitted := inputs[player]; submitted {
		if !existing.folded || folded {
			return frame, nil
		}
		// fallthrough: explicit input overwrites a folded placeholder
	}
	inputs[player] = pendingInput{payload: append([]byte(nil), payload...), folded: folded}
	return frame, nil
}

// Advance cuts the next frame from whatever has arrived (optimistic frame
// locking: nobody is waited for). Inputs are ordered by PlayerID, so the
// frame bytes are a deterministic function of the submissions.
//
// One Sequencer serves one match: the frame id space is uint32 and is never
// recycled. Exhausting it (2^32 frames ≈ years of a single match) panics
// explicitly rather than silently emitting the reserved frame id 0.
func (s *Sequencer) Advance() Frame {
	if s.next == ^FrameID(0) {
		panic("lockstep: frame id space exhausted — one Sequencer serves exactly one match")
	}
	frame := Frame{ID: s.next}
	if inputs := s.pending[s.next]; len(inputs) > 0 {
		players := make([]PlayerID, 0, len(inputs))
		for player := range inputs {
			players = append(players, player)
		}
		sort.Slice(players, func(i, j int) bool { return players[i] < players[j] })
		frame.Inputs = make([]Input, 0, len(players))
		for _, player := range players {
			frame.Inputs = append(frame.Inputs, Input{Player: player, Payload: inputs[player].payload})
		}
		delete(s.pending, s.next)
	}
	s.next++
	return frame
}
