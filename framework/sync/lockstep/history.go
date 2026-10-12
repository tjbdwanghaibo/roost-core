package lockstep

// History 保存一场对局的连续帧，供重连追帧、旁观和回放读取。
// 宿主通过 TrimBefore 控制保留窗口；裁掉的帧不能再用于追帧。
//
// 由对局的串行 handler 独占。Replay 和 ReadRange 返回共享只读视图，
// 只在下一次 Append/TrimBefore 之前有效；跨调用保留时必须复制，不能并发读写。
type History struct {
	first  FrameID
	frames []Frame
	// retainedCapacity 记录整个底层数组容量，包含已裁掉的前缀；
	// frames 的 cap 只覆盖后缀，不能据此判断实际保留的内存。
	retainedCapacity int
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
		h.retainedCapacity = cap(h.frames)
	}
}

// Latest 返回当前保留的最新帧 ID，空历史返回 0。
func (h *History) Latest() FrameID {
	if len(h.frames) == 0 {
		return 0
	}
	return h.first + FrameID(len(h.frames)) - 1
}

// Len 返回当前保留的帧数。
func (h *History) Len() int { return len(h.frames) }

// FirstID 返回当前保留的最早帧 ID，空历史返回 0。
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
		h.retainedCapacity = 0
	case h.retainedCapacity > max(256, 2*len(remaining)):
		// 大幅裁剪才收缩，避免很短的尾部长期持有大数组。
		h.frames = append([]Frame(nil), remaining...)
		h.retainedCapacity = cap(h.frames)
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
