// Package lockstep is the deterministic input-frame synchronization core:
// the server sequences player inputs into fixed-rate frames and broadcasts
// them; simulation runs on the clients (and optionally on a server-side
// arbiter), which must be bit-deterministic — fixed-point math, injected
// randomness, no wall clock (the contract roost-skill's runtime already
// satisfies).
//
// This is INPUT-frame synchronization (lockstep), not to be confused with
// the state-frame path: entitysync replicates per-subject state deltas, one
// frame per session per tick. A lockstep frame carries only what players
// pressed.
//
// The sequencer implements optimistic frame locking: frames are cut on the
// host's clock, never waiting for slow clients — a missing input is an empty
// input, and late inputs are folded into the next uncut frame.
//
// Sequencer 只负责输入排序；同包 Room 负责会话、广播与追帧，并通过
// sync/nettransport 接入传输。实时冗余广播走 datagram，历史追帧走可靠通道；
// 游戏模拟和输入含义由业务决定，不在排序器中实现。
package lockstep

import (
	"errors"
	"fmt"
	"slices"
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

// ReplayHorizon is how many frames behind the next uncut frame a player's
// original input frame ids stay remembered (U-0193, RR-20260914-04). A
// retransmission of an input already folded into a frame within this
// horizon is idempotent; one older than the horizon is indistinguishable
// from a first arrival and folds like one.
const ReplayHorizon FrameID = MaxSubmitWindow

// replayWindowSize is the number of distinct original frame ids that can be
// legal at one instant: ReplayHorizon behind next, next itself, and the
// submit window ahead of it. It is the ring's capacity, which makes the
// dedup memory bounded at ADMISSION rather than at the next Advance
// (U-0197, RR-20260914-08) — a client submitting thousands of distinct old
// frame ids between two ticks grows nothing. Because every legal original
// is within this span of every other, no two of them share a slot.
const replayWindowSize = int(ReplayHorizon) + MaxSubmitWindow + 1

// replaySlot is one ring cell. target is zero on an empty cell: a
// remembered target is always at least next, and next starts at 1.
type replaySlot struct {
	original FrameID
	target   FrameID
}

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
	// accepted remembers, per player, which ORIGINAL frame ids have been
	// taken and which frame each was folded into. The pending map only
	// deduplicates within the target frame and is deleted when that frame
	// is cut, so without this a retransmission of an input already in frame
	// N was re-folded into frame N+1 and executed twice (RR-20260914-04).
	//
	// Each seat's ring holds replayWindowSize slots indexed by original id
	// modulo that size, allocated on the seat's first input. Ids outside
	// the horizon are neither read nor written, so an entry left behind by
	// a long-cut frame is never trusted and is simply overwritten by the
	// id that eventually maps onto it — no eviction pass, and no growth
	// between ticks (RR-20260914-08).
	accepted map[PlayerID][]replaySlot
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
	if len(config.Players) > MaxFrameInputs {
		// A frame can carry at most MaxFrameInputs inputs on the wire; a
		// room with more seats than that emits frames its own decoder
		// rejects (RR-20260914-07). The byte budget in NewRoom does not
		// cover this — tiny payloads fit any number of seats.
		return nil, fmt.Errorf("%w: %d players exceeds the wire limit of %d inputs per frame", ErrConfigInvalid, len(config.Players), MaxFrameInputs)
	}
	players := make(map[PlayerID]struct{}, len(config.Players))
	for _, player := range config.Players {
		if player < 0 {
			// Negative ids are not seats: the room keeps a reserved
			// negative value for spectator ownership, and a seat there
			// would let one session be both (RR-20260914-06).
			return nil, fmt.Errorf("%w: player id %d is negative", ErrConfigInvalid, player)
		}
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
		accepted: make(map[PlayerID][]replaySlot, len(players)),
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
	original := frame
	folded := frame < s.next
	if folded {
		frame = s.next
	}
	if frame > s.next+s.window {
		return 0, ErrFrameTooEarly
	}
	// Identity, and only now: the ring is indexed by the id the client
	// asked for, so it must not be touched before that id has been judged
	// (U-0199). Nothing is lost by waiting — every remembered original
	// satisfied original <= next+window when it was written, next only
	// grows and window is fixed at construction, so an original the check
	// above refuses can never have an identity here.
	//
	// An original already taken is a retransmission, whatever frame it
	// would fold into now: answer with the frame it went into and change
	// nothing (U-0193).
	remembered := original >= s.replayFloor()
	if remembered {
		if slot := s.slot(player, original); slot.target != 0 && slot.original == original {
			return slot.target, nil
		}
	}
	inputs := s.pending[frame]
	if inputs == nil {
		inputs = make(map[PlayerID]pendingInput)
		s.pending[frame] = inputs
	}
	if existing, submitted := inputs[player]; submitted {
		if !existing.folded || folded {
			// Lost to what the frame already holds. Still an accepted
			// identity: its retransmission must not fold into a later
			// frame either.
			s.remember(player, original, frame, remembered)
			return frame, nil
		}
		// fallthrough: explicit input overwrites a folded placeholder
	}
	inputs[player] = pendingInput{payload: append([]byte(nil), payload...), folded: folded}
	s.remember(player, original, frame, remembered)
	return frame, nil
}

// replayFloor is the oldest original id still inside the replay horizon.
// Anything below it is treated as a first arrival: not looked up, and not
// written — writing it would let a client evict a live identity by
// submitting a very old id that lands on the same slot.
func (s *Sequencer) replayFloor() FrameID {
	if s.next <= ReplayHorizon {
		return 0
	}
	return s.next - ReplayHorizon
}

// replaySlotIndex maps an original frame id to its ring cell. The modulo
// runs in the FrameID domain on purpose: FrameID is uint32 and the id comes
// straight from the client, so converting to int first would wrap to a
// NEGATIVE index on a platform whose int is 32 bits (GOARCH=386/arm) and
// panic on the indexing — taking the room's goroutine with it (U-0199).
func replaySlotIndex(original FrameID) int {
	return int(original % FrameID(replayWindowSize))
}

// slot returns the seat's ring cell for original, allocating the ring on
// first use. A seat that never submits costs nothing.
func (s *Sequencer) slot(player PlayerID, original FrameID) *replaySlot {
	ring := s.accepted[player]
	if ring == nil {
		ring = make([]replaySlot, replayWindowSize)
		s.accepted[player] = ring
	}
	return &ring[replaySlotIndex(original)]
}

// remember records that original was folded into target. inHorizon is the
// caller's floor check, taken before the fold moved frame.
func (s *Sequencer) remember(player PlayerID, original, target FrameID, inHorizon bool) {
	if !inHorizon {
		return
	}
	*s.slot(player, original) = replaySlot{original: original, target: target}
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
		slices.Sort(players)
		frame.Inputs = make([]Input, 0, len(players))
		for _, player := range players {
			frame.Inputs = append(frame.Inputs, Input{Player: player, Payload: inputs[player].payload})
		}
		delete(s.pending, s.next)
	}
	s.next++
	// No eviction pass: the ring is fixed-capacity and replayFloor decides
	// what is still trusted, so advancing the frame retires stale
	// identities for free (U-0197).
	return frame
}
