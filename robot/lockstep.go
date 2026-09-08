package robot

import (
	"errors"
	"fmt"
	"hash/fnv"

	"github.com/tjbdwanghaibo/roost-core/lockstep"
)

// LockstepSink carries the bot's outbound lockstep traffic. The wire that
// moves it is business-defined (a robot session call, a datagram lane, an
// in-process Room in tests) — the bot only decides what to send and when.
type LockstepSink interface {
	// SubmitInput sends this player's input for an upcoming frame.
	SubmitInput(frame lockstep.FrameID, payload []byte) error
	// ReportHash sends the simulation hash sampled at a keyframe.
	ReportHash(frame lockstep.FrameID, hash uint64) error
	// RequestCatchup asks the server to replay history from a frame the
	// redundancy window could not heal.
	RequestCatchup(from lockstep.FrameID) error
}

// LockstepBotConfig shapes one lockstep client bot.
type LockstepBotConfig struct {
	Player lockstep.PlayerID
	Sink   LockstepSink
	// Simulate applies one authoritative frame to the bot's simulation.
	// Optional: bots that only exercise the wire (load tests) can omit it —
	// the built-in frame hasher still folds every frame in, so keyframe
	// hashes stay comparable across bots.
	Simulate func(frame lockstep.Frame) error
	// Input produces this player's input for the given frame, called once
	// per applied frame for frame+1. Return nil to skip the frame. Optional.
	Input func(frame lockstep.FrameID) []byte
	// Hash overrides the state hash sampled at keyframes. Optional: the
	// default folds every applied frame's inputs with FNV-1a — on a
	// deterministic simulation, same inputs means same state, so input-chain
	// hashes desync exactly when the state would.
	Hash func(frame lockstep.FrameID) uint64
	// KeyframeInterval samples a hash report every N frames (<= 0 selects
	// 30). Must match what the server-side detector expects.
	KeyframeInterval lockstep.FrameID
	// MaxBuffer bounds the assembler's out-of-order window (<= 0 selects
	// the assembler default).
	MaxBuffer int
}

// LockstepBotStats counts what the bot did — assertions for regression
// tests, throughput signals for load tests.
type LockstepBotStats struct {
	FramesApplied     uint64
	InputsSubmitted   uint64
	HashesReported    uint64
	CatchupsRequested uint64
	DuplicatesDropped uint64
}

// LockstepBot is the client half of the kit lockstep Room: it ingests
// redundant broadcasts through a core FrameAssembler (dedup + strict frame
// order), applies frames to the simulation, submits inputs, and reports
// keyframe hashes. Single-owner like the server side: one goroutine (the
// robot's) drives it.
type LockstepBot struct {
	cfg       LockstepBotConfig
	assembler *lockstep.FrameAssembler
	hasher    *FrameHasher
	stats     LockstepBotStats
	// catchupFrom dedups catch-up requests: while one is outstanding for
	// this frame, further unhealable-gap errors stay quiet.
	catchupFrom lockstep.FrameID
}

// NewLockstepBot builds a bot expecting frame 1 first.
func NewLockstepBot(cfg LockstepBotConfig) (*LockstepBot, error) {
	if cfg.Sink == nil {
		return nil, errors.New("robot lockstep: sink is required")
	}
	if cfg.KeyframeInterval <= 0 {
		cfg.KeyframeInterval = 30
	}
	return &LockstepBot{
		cfg:       cfg,
		assembler: lockstep.NewFrameAssembler(cfg.MaxBuffer),
		hasher:    NewFrameHasher(),
	}, nil
}

// HandleBroadcast ingests one broadcast packet (or catch-up page). Frames
// that became releasable are applied in order; an unhealable gap triggers
// one catch-up request per gap instead of failing the bot.
func (b *LockstepBot) HandleBroadcast(packet []byte) error {
	frames, err := b.assembler.Ingest(packet)
	if applyErr := b.apply(frames); applyErr != nil {
		return applyErr
	}
	if err != nil {
		return b.requestCatchup(err)
	}
	return nil
}

// HandleFrames is HandleBroadcast for already-decoded frames.
func (b *LockstepBot) HandleFrames(frames []lockstep.Frame) error {
	released, err := b.assembler.IngestFrames(frames)
	if applyErr := b.apply(released); applyErr != nil {
		return applyErr
	}
	if err != nil {
		return b.requestCatchup(err)
	}
	return nil
}

func (b *LockstepBot) apply(frames []lockstep.Frame) error {
	for _, frame := range frames {
		b.hasher.Fold(frame)
		if b.cfg.Simulate != nil {
			if err := b.cfg.Simulate(frame); err != nil {
				return fmt.Errorf("robot lockstep: simulate frame %d: %w", frame.ID, err)
			}
		}
		b.stats.FramesApplied++
		if b.cfg.Input != nil {
			if payload := b.cfg.Input(frame.ID + 1); payload != nil {
				if err := b.cfg.Sink.SubmitInput(frame.ID+1, payload); err != nil {
					return fmt.Errorf("robot lockstep: submit input for frame %d: %w", frame.ID+1, err)
				}
				b.stats.InputsSubmitted++
			}
		}
		if frame.ID%b.cfg.KeyframeInterval == 0 {
			hash := b.hasher.Sum()
			if b.cfg.Hash != nil {
				hash = b.cfg.Hash(frame.ID)
			}
			if err := b.cfg.Sink.ReportHash(frame.ID, hash); err != nil {
				return fmt.Errorf("robot lockstep: report hash for frame %d: %w", frame.ID, err)
			}
			b.stats.HashesReported++
		}
	}
	b.stats.DuplicatesDropped = b.assembler.Duplicates()
	return nil
}

func (b *LockstepBot) requestCatchup(cause error) error {
	from := b.assembler.Next()
	if b.catchupFrom == from {
		return nil // already requested for this gap
	}
	if err := b.cfg.Sink.RequestCatchup(from); err != nil {
		return fmt.Errorf("robot lockstep: request catch-up from frame %d: %w (cause: %v)", from, err, cause)
	}
	b.catchupFrom = from
	b.stats.CatchupsRequested++
	return nil
}

// Next is the frame id the bot is waiting for.
func (b *LockstepBot) Next() lockstep.FrameID { return b.assembler.Next() }

// Stats snapshots the bot's counters.
func (b *LockstepBot) Stats() LockstepBotStats { return b.stats }

// FrameHasher folds applied frames into a running FNV-1a hash. Two bots that
// applied the same frame sequence hold the same sum — the default keyframe
// hash when the bot has no real simulation state to sample.
type FrameHasher struct {
	sum uint64
}

// NewFrameHasher starts from the FNV-1a offset basis.
func NewFrameHasher() *FrameHasher {
	h := fnv.New64a()
	return &FrameHasher{sum: h.Sum64()}
}

// Fold mixes one frame (id plus every input's player and payload, in the
// server-decided order) into the running hash.
func (f *FrameHasher) Fold(frame lockstep.Frame) {
	h := fnv.New64a()
	var buf [8]byte
	putUint64 := func(v uint64) {
		for i := 0; i < 8; i++ {
			buf[i] = byte(v >> (8 * i))
		}
		_, _ = h.Write(buf[:])
	}
	putUint64(f.sum)
	putUint64(uint64(frame.ID))
	for _, input := range frame.Inputs {
		putUint64(uint64(input.Player))
		putUint64(uint64(len(input.Payload)))
		_, _ = h.Write(input.Payload)
	}
	f.sum = h.Sum64()
}

// Sum is the running hash over everything folded so far.
func (f *FrameHasher) Sum() uint64 { return f.sum }
