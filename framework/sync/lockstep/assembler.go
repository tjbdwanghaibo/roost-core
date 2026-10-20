package lockstep

import (
	"errors"
	"fmt"
)

var (
	// ErrSnapshotUnsupported 表示只会追帧的客户端收到了快照分块：Ingest 不处理快照，调用方应改用 Receive，
	// 或（没有恢复手段时）终止本场。不能跳过快照继续模拟。
	ErrSnapshotUnsupported = errors.New("lockstep: snapshot recovery not supported by this client")
	// ErrSnapshotNotApplied 表示 Receive 已交出快照，调用方还没调用 ResumeAfterSnapshot 就继续投包。
	ErrSnapshotNotApplied = errors.New("lockstep: snapshot delivered but not applied")
	// ErrSnapshotMismatch 表示客户端恢复快照后算出的状态 hash 与服务端给出的不同（见 SnapshotMismatchError）。
	ErrSnapshotMismatch = errors.New("lockstep: restored state hash differs from the snapshot")
)

// SnapshotMismatchError 是恢复后状态 hash 不符的详情；errors.Is(err, ErrSnapshotMismatch) 成立。
// 字节摘要已核对过，状态仍不同，说明客户端版本 / 规则 / 确定性与服务器参考不一致：再要一份快照结果相同，
// 客户端应终止本场、交业务提示更新或踢出（不自动重试，也不自动上报）。
type SnapshotMismatchError struct {
	Frame FrameID
	Want  uint64 // 快照携带的状态 hash
	Got   uint64 // 客户端恢复后算出的状态 hash
}

func (e *SnapshotMismatchError) Error() string {
	return fmt.Sprintf("%v: frame %d, want %d, got %d", ErrSnapshotMismatch, e.Frame, e.Want, e.Got)
}

func (e *SnapshotMismatchError) Unwrap() error { return ErrSnapshotMismatch }

// Delivery 是 Receive 处理一个下行包的结果。
type Delivery struct {
	// Frames 是按序可应用的帧。
	Frames []Frame
	// Snapshot 非 nil 表示快照已完整、字节摘要已核对；调用方恢复状态（并核对状态 hash）后必须调用
	// ResumeAfterSnapshot(Snapshot.Frame)，之前再调用 Receive / IngestFrames 返回 ErrSnapshotNotApplied。
	Snapshot *ReceivedSnapshot
}

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

	// snapshots 重组 CA 快照分块（只有 Receive 用）；传输进行中收到的 C7 帧丢弃。
	snapshots *SnapshotReceiver
	// unapplied 是已交出、还没 ResumeAfterSnapshot 的快照帧号；hasUnapplied 为 false 时无意义。
	unapplied    FrameID
	hasUnapplied bool
}

// NewFrameAssembler builds an assembler expecting frame 1 first. maxBuffer
// bounds the out-of-order window (<= 0 selects 256). Snapshots received
// through Receive are limited to DefaultMaxSnapshotBytes.
func NewFrameAssembler(maxBuffer int) *FrameAssembler {
	return NewFrameAssemblerLimits(maxBuffer, 0)
}

// NewFrameAssemblerLimits 同 NewFrameAssembler，另指定客户端可接受的快照上限 maxSnapshotBytes
// （<= 0 取 DefaultMaxSnapshotBytes，超过 MaxSnapshotBytes 按 MaxSnapshotBytes）：首块声明的总长超过它即坏包，
// 不分配内存，防恶意包让客户端分配大块内存。
func NewFrameAssemblerLimits(maxBuffer, maxSnapshotBytes int) *FrameAssembler {
	if maxBuffer <= 0 {
		maxBuffer = 256
	}
	return &FrameAssembler{next: 1, buffer: make(map[FrameID]Frame), maxBuffer: maxBuffer, snapshots: NewSnapshotReceiver(maxSnapshotBytes)}
}

// Receive 是支持快照恢复的客户端主入口，处理同一路由上的 C7 广播 / 历史页、C9 恢复失败通知与 CA 快照分块：
//
//   - C7：返回按序可应用的帧（同 Ingest）。快照传输进行中收到的 C7 帧丢弃并计入 Duplicates——
//     快照之后服务端从快照帧 +1 起重发历史页，会覆盖它们。
//   - C9：返回 *CatchupUnservableError（errors.Is ErrCatchupUnservable），Reason 说明失败原因；客户端终止本场。
//   - CA：分块未齐返回空 Delivery；最后一块到齐且 SHA-256 摘要相符时返回 Delivery.Snapshot。
//     坏分块返回 ErrFrameCorrupt，摘要不符返回 ErrSnapshotCorrupt；两者都丢弃进行中的传输。
//
// 快照的任何校验失败（坏分块、摘要不符、调用方恢复后状态 hash 不符）都应终止本场、不自动重试：
// 可靠通道上出现即是实现或版本问题，同一份快照重来结果相同（设计 §7.3）。
func (a *FrameAssembler) Receive(packet []byte) (Delivery, error) {
	if a.hasUnapplied {
		return Delivery{}, fmt.Errorf("%w: snapshot at frame %d", ErrSnapshotNotApplied, a.unapplied)
	}
	switch {
	case IsCatchupNotice(packet):
		notice, err := DecodeCatchupUnservable(packet)
		if err != nil {
			return Delivery{}, err
		}
		return Delivery{}, notice
	case IsSnapshotChunk(packet):
		snapshot, err := a.snapshots.Accept(packet)
		if err != nil || snapshot == nil {
			return Delivery{}, err
		}
		a.unapplied, a.hasUnapplied = snapshot.Frame, true
		return Delivery{Snapshot: snapshot}, nil
	}
	frames, err := DecodeBroadcast(packet)
	if err != nil {
		return Delivery{}, err
	}
	released, err := a.IngestFrames(frames)
	return Delivery{Frames: released}, err
}

// ResumeAfterSnapshot 在调用方用快照恢复状态（并核对状态 hash）之后调用，frame 是快照帧号：
// 下一帧期待 frame+1，乱序缓冲整个清空。缓冲里的帧都是恢复开始前收到的实时帧，服务端在快照之后
// 从 frame+1 起发历史页，追到头才接回实时广播，所以它们一定会被重发；留着大于 frame 的帧反而会在
// 续帧时被跳过、永远留在缓冲里。frame 可以早于已应用的帧（状态整体回到 frame，之后的帧重新应用）。
func (a *FrameAssembler) ResumeAfterSnapshot(frame FrameID) {
	a.next = frame + 1
	clear(a.buffer)
	a.unapplied, a.hasUnapplied = 0, false
}

// SnapshotInProgress 报告是否有一次快照传输已开始、还没到齐（此时收到的 C7 帧被丢弃）。
func (a *FrameAssembler) SnapshotInProgress() bool { return a.snapshots.Active() }

// Ingest decodes one broadcast packet (or catch-up page) and returns the
// frames that became releasable in order. Duplicate frames (redundancy,
// catch-up overlap) are dropped; frames beyond the buffer bound fail.
//
// A catch-up-failed notice from the server returns *CatchupUnservableError
// (errors.Is ErrCatchupUnservable): the frames this client is missing are
// gone from the server's history, so the match cannot continue on this
// client — stop simulating and let the game decide (kick, rejoin a new
// match). Requesting another catch-up cannot help.
//
// Ingest 只服务只会追帧的调用方：快照分块返回 ErrSnapshotUnsupported（明确终止，而不是当坏包）。
// 支持快照恢复的客户端用 Receive。
func (a *FrameAssembler) Ingest(packet []byte) ([]Frame, error) {
	if IsSnapshotChunk(packet) {
		return nil, fmt.Errorf("%w: snapshot chunk received by Ingest; use Receive", ErrSnapshotUnsupported)
	}
	if IsCatchupNotice(packet) {
		notice, err := DecodeCatchupUnservable(packet)
		if err != nil {
			return nil, err
		}
		return nil, notice
	}
	frames, err := DecodeBroadcast(packet)
	if err != nil {
		return nil, err
	}
	return a.IngestFrames(frames)
}

// IngestFrames is Ingest for already-decoded frames. While a snapshot
// transfer is in progress the frames are dropped (counted as duplicates);
// after Receive delivered a snapshot and before ResumeAfterSnapshot it
// returns ErrSnapshotNotApplied.
func (a *FrameAssembler) IngestFrames(frames []Frame) ([]Frame, error) {
	if a.hasUnapplied {
		return nil, fmt.Errorf("%w: snapshot at frame %d", ErrSnapshotNotApplied, a.unapplied)
	}
	if a.snapshots.Active() {
		a.duplicates += uint64(len(frames))
		return nil, nil
	}
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
