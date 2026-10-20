package lockstep

import (
	"encoding/binary"
	"fmt"
)

// Wire format for lockstep broadcast packets. One datagram carries the most
// recent N frames (redundancy: a lost packet is healed by the frames riding
// on its successors — no retransmission, no added latency, the classic
// mobile-MOBA loss strategy). Empty frames cost only their varint frame id,
// so idle stretches compress to a few bytes per frame.
//
//	packet  = magic(1) version(1) frameCount(uvarint) frame...
//	frame   = frameID(uvarint) inputCount(uvarint) input...
//	input   = player(uvarint) payloadLen(uvarint) payload
const (
	wireMagic   = 0xC7
	wireVersion = 1
	// MaxBroadcastFrames bounds frames per packet on decode (a corrupt or
	// hostile packet cannot allocate unbounded frames).
	MaxBroadcastFrames = 64
	// MaxFrameInputs bounds inputs per frame on decode: a frame can never
	// carry more inputs than seats, and no match has this many seats. Caps
	// the decode-side memory amplification of a hostile reliable-lane page.
	MaxFrameInputs = 256
)

// RedundantEncoder keeps the most recent frames in a fixed ring and encodes
// each broadcast packet with all of them.
type RedundantEncoder struct {
	ring    []Frame // fixed length = depth
	count   int
	next    int
	ordered []Frame // reused oldest-first view for encoding
}

// NormalizeRedundancyDepth maps a configured depth to the effective one:
// depth <= 0 selects 3, and the decoder-side MaxBroadcastFrames is the hard
// ceiling — a deeper encoder would emit packets every receiver rejects.
func NormalizeRedundancyDepth(depth int) int {
	if depth <= 0 {
		return 3
	}
	if depth > MaxBroadcastFrames {
		return MaxBroadcastFrames
	}
	return depth
}

// NewRedundantEncoder builds an encoder carrying depth frames per packet
// (normalized via NormalizeRedundancyDepth).
func NewRedundantEncoder(depth int) *RedundantEncoder {
	depth = NormalizeRedundancyDepth(depth)
	return &RedundantEncoder{ring: make([]Frame, depth), ordered: make([]Frame, 0, depth)}
}

// Push appends a cut frame and returns the encoded broadcast packet carrying
// it plus up to depth-1 predecessors. The ring is fixed-size: no sliding
// reallocation, and evicted frames drop their payload references.
func (e *RedundantEncoder) Push(frame Frame) []byte {
	e.ring[e.next] = frame
	e.next = (e.next + 1) % len(e.ring)
	if e.count < len(e.ring) {
		e.count++
	}
	e.ordered = e.ordered[:0]
	start := (e.next - e.count + len(e.ring)) % len(e.ring)
	for i := 0; i < e.count; i++ {
		e.ordered = append(e.ordered, e.ring[(start+i)%len(e.ring)])
	}
	return EncodeBroadcast(e.ordered)
}

// EncodeBroadcast encodes frames (oldest first) into one packet.
func EncodeBroadcast(frames []Frame) []byte {
	size := 2 + binary.MaxVarintLen32
	for _, frame := range frames {
		size += 2*binary.MaxVarintLen32 + 1
		for _, input := range frame.Inputs {
			size += 2*binary.MaxVarintLen32 + len(input.Payload)
		}
	}
	packet := make([]byte, 0, size)
	packet = append(packet, wireMagic, wireVersion)
	packet = binary.AppendUvarint(packet, uint64(len(frames)))
	for _, frame := range frames {
		packet = binary.AppendUvarint(packet, uint64(frame.ID))
		packet = binary.AppendUvarint(packet, uint64(len(frame.Inputs)))
		for _, input := range frame.Inputs {
			packet = binary.AppendUvarint(packet, uint64(uint32(input.Player)))
			packet = binary.AppendUvarint(packet, uint64(len(input.Payload)))
			packet = append(packet, input.Payload...)
		}
	}
	return packet
}

// DecodeBroadcast parses one broadcast packet. Decoding is strict: bad
// magic/version, truncated data, oversized counts and trailing bytes are all
// rejected — a corrupt packet never becomes a silently wrong frame.
func DecodeBroadcast(packet []byte) ([]Frame, error) {
	if len(packet) < 3 || packet[0] != wireMagic || packet[1] != wireVersion {
		return nil, fmt.Errorf("%w: bad header", ErrFrameCorrupt)
	}
	rest := packet[2:]
	frameCount, rest, err := readUvarint(rest)
	if err != nil {
		return nil, err
	}
	if frameCount > MaxBroadcastFrames {
		return nil, fmt.Errorf("%w: %d frames exceeds %d", ErrFrameCorrupt, frameCount, MaxBroadcastFrames)
	}
	frames := make([]Frame, 0, frameCount)
	lastID := uint64(0)
	for index := uint64(0); index < frameCount; index++ {
		var frame Frame
		var id, inputCount uint64
		if id, rest, err = readUvarint(rest); err != nil {
			return nil, err
		}
		if id == 0 || id > uint64(^FrameID(0)) {
			return nil, fmt.Errorf("%w: frame id %d", ErrFrameCorrupt, id)
		}
		// Frames in one packet are strictly increasing (the encoder emits
		// oldest-first): duplicates or reordering mark a forged packet.
		if id <= lastID {
			return nil, fmt.Errorf("%w: frame id %d not increasing", ErrFrameCorrupt, id)
		}
		lastID = id
		frame.ID = FrameID(id)
		if inputCount, rest, err = readUvarint(rest); err != nil {
			return nil, err
		}
		if inputCount > MaxFrameInputs || inputCount > uint64(len(rest)) {
			return nil, fmt.Errorf("%w: input count %d", ErrFrameCorrupt, inputCount)
		}
		for input := uint64(0); input < inputCount; input++ {
			var player, payloadLen uint64
			if player, rest, err = readUvarint(rest); err != nil {
				return nil, err
			}
			if player > uint64(^uint32(0)) {
				return nil, fmt.Errorf("%w: player id %d", ErrFrameCorrupt, player)
			}
			if payloadLen, rest, err = readUvarint(rest); err != nil {
				return nil, err
			}
			if payloadLen > MaxInputPayloadBytes || payloadLen > uint64(len(rest)) {
				return nil, fmt.Errorf("%w: payload length %d", ErrFrameCorrupt, payloadLen)
			}
			payload := append([]byte(nil), rest[:payloadLen]...)
			rest = rest[payloadLen:]
			frame.Inputs = append(frame.Inputs, Input{Player: PlayerID(uint32(player)), Payload: payload})
		}
		frames = append(frames, frame)
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("%w: %d trailing bytes", ErrFrameCorrupt, len(rest))
	}
	return frames, nil
}

// 恢复失败通知：服务端经可靠通道、与追帧页同一路由发给这次恢复（追帧或快照）已失败的会话。
//
//	notice = magic(0xC9) version(2) reason(uvarint) from(uvarint) start(uvarint)
//
// reason 见 RecoveryFailureReason；from 是客户端缺的第一帧（请求起点或追帧游标），start 是服务端当时仍保留的最早帧。
// 只有 history_trimmed 要求 from < start。客户端收到任何原因都终止本场模拟、交由业务决定踢出或重开。
// v1（不带 reason）已删除（项目未上线，不兼容旧客户端）：旧客户端把 v2 当坏包终止。
const (
	noticeMagic   = 0xC9
	noticeVersion = 2
)

// RecoveryFailureReason 是 C9 v2 通知的失败原因。
type RecoveryFailureReason uint8

const (
	// RecoveryHistoryTrimmed：起点已被裁出保留窗口（catchup 模式；原 C9 v1 的唯一语义）。
	RecoveryHistoryTrimmed RecoveryFailureReason = 1
	// RecoverySnapshotUnavailable：快照来源连续失败达到 CatchupMaxFailures。
	RecoverySnapshotUnavailable RecoveryFailureReason = 2
	// RecoverySnapshotTooLarge：快照超过 SnapshotConfig.MaxBytes。
	RecoverySnapshotTooLarge RecoveryFailureReason = 3
	// RecoveryExpired：恢复期间钉住的历史超过 SnapshotConfig.PinFrames 上限。
	RecoveryExpired RecoveryFailureReason = 4
)

func (reason RecoveryFailureReason) String() string {
	switch reason {
	case RecoveryHistoryTrimmed:
		return "history_trimmed"
	case RecoverySnapshotUnavailable:
		return "snapshot_unavailable"
	case RecoverySnapshotTooLarge:
		return "snapshot_too_large"
	case RecoveryExpired:
		return "recovery_expired"
	default:
		return fmt.Sprintf("reason_%d", uint8(reason))
	}
}

// CatchupUnservableError 是一次恢复失败的具体信息；errors.Is(err, ErrCatchupUnservable) 对所有原因成立，
// 客户端“终止、交业务处理”的判断点不变。服务端 StartCatchup / Tick 返回它，客户端 FrameAssembler.Ingest
// 解出失败通知时也返回它。Reason 为 0 按 RecoveryHistoryTrimmed 处理（只设 From / Start 的旧写法）。
type CatchupUnservableError struct {
	Reason RecoveryFailureReason
	From   FrameID // 客户端缺的第一帧
	Start  FrameID // 服务端仍保留的最早帧
}

func (e *CatchupUnservableError) reason() RecoveryFailureReason {
	if e.Reason == 0 {
		return RecoveryHistoryTrimmed
	}
	return e.Reason
}

func (e *CatchupUnservableError) Error() string {
	if reason := e.reason(); reason != RecoveryHistoryTrimmed {
		return fmt.Sprintf("%v (%s): from %d, history starts at %d", ErrCatchupUnservable, reason, e.From, e.Start)
	}
	return fmt.Sprintf("%v: from %d, history starts at %d", ErrCatchupUnservable, e.From, e.Start)
}

func (e *CatchupUnservableError) Unwrap() error { return ErrCatchupUnservable }

// EncodeCatchupUnservable 编码起点已被裁掉（history_trimmed）的失败通知。
func EncodeCatchupUnservable(from, start FrameID) []byte {
	return EncodeRecoveryFailure(RecoveryHistoryTrimmed, from, start)
}

// EncodeRecoveryFailure 编码 C9 v2 恢复失败通知。
func EncodeRecoveryFailure(reason RecoveryFailureReason, from, start FrameID) []byte {
	out := []byte{noticeMagic, noticeVersion}
	out = binary.AppendUvarint(out, uint64(reason))
	out = binary.AppendUvarint(out, uint64(from))
	return binary.AppendUvarint(out, uint64(start))
}

// IsCatchupNotice 只看包头判断是否为恢复失败通知（广播与追帧页以 0xC7 开头，快照分块以 0xCA 开头）。
func IsCatchupNotice(packet []byte) bool { return len(packet) > 0 && packet[0] == noticeMagic }

// DecodeCatchupUnservable 严格解码 C9 v2 恢复失败通知；坏包（含 v1、未知原因）返回 ErrFrameCorrupt。
func DecodeCatchupUnservable(packet []byte) (*CatchupUnservableError, error) {
	if len(packet) < 5 || packet[0] != noticeMagic || packet[1] != noticeVersion {
		return nil, fmt.Errorf("%w: bad catch-up notice header", ErrFrameCorrupt)
	}
	reason, rest, err := readUvarint(packet[2:])
	if err != nil {
		return nil, err
	}
	from, rest, err := readUvarint(rest)
	if err != nil {
		return nil, err
	}
	start, rest, err := readUvarint(rest)
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 || reason < uint64(RecoveryHistoryTrimmed) || reason > uint64(RecoveryExpired) ||
		from == 0 || from > uint64(^FrameID(0)) || start > uint64(^FrameID(0)) ||
		(RecoveryFailureReason(reason) == RecoveryHistoryTrimmed && from >= start) {
		return nil, fmt.Errorf("%w: catch-up notice reason %d from %d start %d", ErrFrameCorrupt, reason, from, start)
	}
	return &CatchupUnservableError{Reason: RecoveryFailureReason(reason), From: FrameID(from), Start: FrameID(start)}, nil
}

func readUvarint(data []byte) (uint64, []byte, error) {
	value, read := binary.Uvarint(data)
	if read <= 0 {
		return 0, nil, fmt.Errorf("%w: truncated varint", ErrFrameCorrupt)
	}
	return value, data[read:], nil
}
