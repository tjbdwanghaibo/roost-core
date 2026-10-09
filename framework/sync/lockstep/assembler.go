package lockstep

import "fmt"

// FrameAssembler is the client-side counterpart of the broadcast encoder:
// it ingests redundant broadcast packets (and catch-up pages), deduplicates
// the overlapping frames, and releases frames strictly in order. Robots and
// real clients share it — a lockstep bot built on the assembler exercises
// the exact wire contract production clients run.
//
// Single-owner state, like the rest of the package: one goroutine drives it.
type FrameAssembler struct {
	next   FrameID
	buffer map[FrameID]Frame
	// maxBuffer bounds out-of-order frames held while a gap is open; beyond
	// it Ingest reports the gap as unrecoverable so the client can request
	// a catch-up instead of buffering unboundedly.
	maxBuffer int

	duplicates uint64
	healed     uint64
}

// NewFrameAssembler builds an assembler expecting frame 1 first. maxBuffer
// bounds the out-of-order window (<= 0 selects 256).
func NewFrameAssembler(maxBuffer int) *FrameAssembler {
	if maxBuffer <= 0 {
		maxBuffer = 256
	}
	return &FrameAssembler{next: 1, buffer: make(map[FrameID]Frame), maxBuffer: maxBuffer}
}

// Ingest decodes one broadcast packet (or catch-up page) and returns the
// frames that became releasable in order. Duplicate frames (redundancy,
// catch-up overlap) are dropped; frames beyond the buffer bound fail.
func (a *FrameAssembler) Ingest(packet []byte) ([]Frame, error) {
	frames, err := DecodeBroadcast(packet)
	if err != nil {
		return nil, err
	}
	return a.IngestFrames(frames)
}

// IngestFrames is Ingest for already-decoded frames.
func (a *FrameAssembler) IngestFrames(frames []Frame) ([]Frame, error) {
	var released []Frame
	for _, frame := range frames {
		if frame.ID < a.next {
			a.duplicates++
			continue
		}
		if frame.ID == a.next {
			// The awaited frame releases immediately, then drains any
			// contiguous run buffered behind it — so a gap-closing catch-up
			// frame is never rejected by the buffer bound.
			released = append(released, frame)
			a.next++
			for {
				buffered, ok := a.buffer[a.next]
				if !ok {
					break
				}
				delete(a.buffer, a.next)
				released = append(released, buffered)
				a.next++
			}
			continue
		}
		if _, seen := a.buffer[frame.ID]; seen {
			a.duplicates++
			continue
		}
		if len(a.buffer) >= a.maxBuffer {
			return released, fmt.Errorf("%w: out-of-order buffer full at frame %d (gap since %d) — request a catch-up", ErrHistoryUnknown, frame.ID, a.next)
		}
		a.buffer[frame.ID] = frame
		a.healed++ // arrived ahead of a lost predecessor that must be healed
	}
	return released, nil
}

// Next is the frame id the assembler is waiting for.
func (a *FrameAssembler) Next() FrameID { return a.next }

// Gap reports whether a frame gap is currently open (frames buffered ahead
// of the missing Next) — the signal to request a catch-up from Next.
func (a *FrameAssembler) Gap() bool { return len(a.buffer) > 0 }

// Duplicates counts frames dropped as redundant copies.
func (a *FrameAssembler) Duplicates() uint64 { return a.duplicates }
