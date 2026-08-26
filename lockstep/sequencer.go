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

// MaxInputPayloadBytes bounds one input payload; real lockstep inputs are a
// few bytes, so anything near this limit is a protocol bug.
const MaxInputPayloadBytes = 1024

// SequencerConfig shapes a match's sequencer.
type SequencerConfig struct {
	// Players fixes the seat set for the match.
	Players []PlayerID
	// SubmitWindow is how many frames ahead of the next uncut frame an input
	// may target (protects the buffer from a client spraying far-future
	// frames). Zero selects the default of 2.
	SubmitWindow FrameID
}

// Sequencer turns submitted inputs into sequenced frames. It is
// single-owner state: the room's serial handler drives it — no locks, in
// line with the nest execution model.
type Sequencer struct {
	players map[PlayerID]struct{}
	window  FrameID
	next    FrameID
	pending map[FrameID]map[PlayerID][]byte
}

func NewSequencer(config SequencerConfig) (*Sequencer, error) {
	if len(config.Players) == 0 {
		return nil, fmt.Errorf("%w: at least one player", ErrConfigInvalid)
	}
	window := config.SubmitWindow
	if window == 0 {
		window = 2
	}
	players := make(map[PlayerID]struct{}, len(config.Players))
	for _, player := range config.Players {
		if _, duplicate := players[player]; duplicate {
			return nil, fmt.Errorf("%w: duplicate player %d", ErrConfigInvalid, player)
		}
		players[player] = struct{}{}
	}
	return &Sequencer{
		players: players,
		window:  window,
		next:    1,
		pending: make(map[FrameID]map[PlayerID][]byte),
	}, nil
}

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
// redundancy); a frame farther ahead than the submit window is rejected.
func (s *Sequencer) SubmitInput(player PlayerID, frame FrameID, payload []byte) (FrameID, error) {
	if _, known := s.players[player]; !known {
		return 0, ErrPlayerUnknown
	}
	if len(payload) > MaxInputPayloadBytes {
		return 0, ErrPayloadTooBig
	}
	if frame < s.next {
		frame = s.next
	}
	if frame > s.next+s.window {
		return 0, ErrFrameTooEarly
	}
	inputs := s.pending[frame]
	if inputs == nil {
		inputs = make(map[PlayerID][]byte)
		s.pending[frame] = inputs
	}
	if _, submitted := inputs[player]; submitted {
		return frame, nil
	}
	inputs[player] = append([]byte(nil), payload...)
	return frame, nil
}

// Advance cuts the next frame from whatever has arrived (optimistic frame
// locking: nobody is waited for). Inputs are ordered by PlayerID, so the
// frame bytes are a deterministic function of the submissions.
func (s *Sequencer) Advance() Frame {
	frame := Frame{ID: s.next}
	if inputs := s.pending[s.next]; len(inputs) > 0 {
		players := make([]PlayerID, 0, len(inputs))
		for player := range inputs {
			players = append(players, player)
		}
		sort.Slice(players, func(i, j int) bool { return players[i] < players[j] })
		frame.Inputs = make([]Input, 0, len(players))
		for _, player := range players {
			frame.Inputs = append(frame.Inputs, Input{Player: player, Payload: inputs[player]})
		}
		delete(s.pending, s.next)
	}
	s.next++
	return frame
}
