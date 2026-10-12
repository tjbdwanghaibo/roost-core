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
// handler, until the next Append or TrimBefore. Callers retaining a replay
// beyond that point must copy it. Concurrent reads and mutations are unsupported.
type History struct {
	first  FrameID
	frames []Frame
	// allocated 包含已裁掉前缀仍占用的底层数组容量，不能只看当前切片的 cap。
	allocated int
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
	grows := len(h.frames) == cap(h.frames)
	h.frames = append(h.frames, frame)
	if grows {
		h.allocated = cap(h.frames)
	}
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
// become unservable (the match abandons them with an explicit error).
func (h *History) TrimBefore(keep FrameID) {
	if len(h.frames) == 0 || keep <= h.first {
		return
	}
	// 使用差值避免 first+len 在 FrameID 上界溢出；逐帧裁剪不复制整段历史。
	offset := int(min(uint64(keep-h.first), uint64(len(h.frames))))
	clear(h.frames[:offset]) // 立即解除丢弃帧的 Inputs/Payload 引用。
	remaining := h.frames[offset:]
	switch {
	case len(remaining) == 0:
		h.frames = nil
		h.allocated = 0
	case h.allocated > max(256, 2*len(remaining)):
		// 大幅裁剪才收缩，避免很短的尾部长期持有大数组。
		h.frames = append([]Frame(nil), remaining...)
		h.allocated = cap(h.frames)
	default:
		h.frames = remaining
	}
	h.first += FrameID(offset)
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
	end := offset + min(limit, len(h.frames)-offset)
	return h.frames[offset:end]
}

// Replay returns the whole match as one slice (the replay artifact). The
// slice is shared with the history; callers must not mutate frames.
func (h *History) Replay() []Frame { return h.frames }
