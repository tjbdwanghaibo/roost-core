package lockstep

// History stores every sequenced frame of a match: the catch-up source for
// reconnects, mid-game joins and spectators, and — because inputs plus a
// deterministic simulation ARE the match — the replay file, for free.
//
// Memory budget: the worst case is duration × rate × players × payload —
// e.g. one hour at 30 fps with 10 players × 8-byte inputs ≈ 50 MB. Hosts
// that do not need the full replay must TrimBefore periodically (catch-up
// older than the trim point becomes unservable).
//
// History is single-owner state like the rest of the package: Replay and
// ReadRange return views that are only valid inside the owning serial
// handler — Append may reallocate the backing array under a concurrent
// reader.
type History struct {
	first  FrameID
	frames []Frame
}

func NewHistory() *History { return &History{} }

// Append records a cut frame. Frames must arrive in sequence (the sequencer
// produces them that way); a gap or replay indicates a wiring bug and
// panics.
func (h *History) Append(frame Frame) {
	if len(h.frames) == 0 {
		h.first = frame.ID
	} else if expected := h.first + FrameID(len(h.frames)); frame.ID != expected {
		panic("lockstep: history append out of sequence")
	}
	h.frames = append(h.frames, frame)
}

// Latest is the newest stored frame id (0 when empty).
func (h *History) Latest() FrameID {
	if len(h.frames) == 0 {
		return 0
	}
	return h.first + FrameID(len(h.frames)) - 1
}

// Len is the stored frame count.
func (h *History) Len() int { return len(h.frames) }

// FirstID is the oldest stored frame id (0 when empty).
func (h *History) FirstID() FrameID {
	if len(h.frames) == 0 {
		return 0
	}
	return h.first
}

// TrimBefore drops all frames with id < keep, bounding memory for hosts
// that do not need the full replay. Catch-up requests older than keep
// become unservable (the room abandons them with an explicit error).
func (h *History) TrimBefore(keep FrameID) {
	if len(h.frames) == 0 || keep <= h.first {
		return
	}
	if keep > h.first+FrameID(len(h.frames)) {
		keep = h.first + FrameID(len(h.frames))
	}
	offset := int(keep - h.first)
	remaining := make([]Frame, len(h.frames)-offset)
	copy(remaining, h.frames[offset:])
	h.frames = remaining // fresh backing array: trimmed payloads become collectable
	h.first = keep
}

// ReadRange returns up to limit frames starting at from, for catch-up
// paging. from == 0 or before the first stored frame starts at the
// beginning; a from beyond the latest frame returns nil.
func (h *History) ReadRange(from FrameID, limit int) []Frame {
	if len(h.frames) == 0 || limit <= 0 {
		return nil
	}
	if from < h.first {
		from = h.first
	}
	offset := int(from - h.first)
	if offset >= len(h.frames) {
		return nil
	}
	end := offset + limit
	if end > len(h.frames) {
		end = len(h.frames)
	}
	return h.frames[offset:end]
}

// Replay returns the whole match as one slice (the replay artifact). The
// slice is shared with the history; callers must not mutate frames.
func (h *History) Replay() []Frame { return h.frames }
