package lockstep

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
)

// 快照分块：服务端经可靠通道、与 C7 / C9 同一路由下发；服务端 Match 与 Go 客户端共用这份编解码。
//
//	chunk  = magic(0xCA) version(1) transfer(uvarint, 非 0) offset(uvarint) [header，仅当 offset = 0] data
//	header = frame(uvarint) total(uvarint, 1..上限) flags(1 字节) [stateHash(uvarint64)，当 flags bit0] digest(32 字节)
//	data   = 至少 1 字节，offset + len(data) <= total
//	flags  : bit0 = 携带 StateHash；其余位必须为 0
//	digest : SHA-256(完整快照字节)
//
// transfer 是 Match 内单调递增的传输编号：同一会话重新发起快照（重取、窗口越过、换连接）用新编号。
// 客户端遇到新编号且 offset = 0 时丢弃未完成的旧传输；新编号但 offset != 0 是坏包。同一编号下
// offset 小于已收字节数是重发（服务端把超时但其实已写出的分块再发了一次），丢弃不报错；大于则是坏包。
// 首块之外不重复头部：可靠通道有序，重发只发生在已确认偏移处。
const (
	snapshotMagic         = 0xCA
	snapshotVersion       = 1
	snapshotFlagStateHash = 0x01
	// SnapshotDigestBytes 是首块携带的 SHA-256 摘要长度。
	SnapshotDigestBytes = sha256.Size
	// MaxSnapshotChunkHeaderBytes 是一个分块除数据外的最大字节数（首块最长），用于核对可靠通道的单条上限。
	MaxSnapshotChunkHeaderBytes = 2 + 2*binary.MaxVarintLen64 + binary.MaxVarintLen32 + binary.MaxVarintLen64 + 1 + binary.MaxVarintLen64 + SnapshotDigestBytes
)

// SnapshotChunk 是一个解码后的 CA 01 分块。First 为 true（Offset = 0）时头部字段有效。
type SnapshotChunk struct {
	Transfer uint64
	Offset   int
	First    bool

	Frame        FrameID
	Total        int
	StateHash    uint64
	HasStateHash bool
	Digest       [SnapshotDigestBytes]byte

	// Data 引用解码时传入的包，调用方需要保留时自行复制。
	Data []byte
}

// ReceivedSnapshot 是客户端重组完成、字节摘要已核对的快照；Data 是调用方独占的副本。
type ReceivedSnapshot struct {
	Frame        FrameID
	Data         []byte
	StateHash    uint64
	HasStateHash bool
}

// IsSnapshotChunk 只看包头判断是否为快照分块。
func IsSnapshotChunk(packet []byte) bool { return len(packet) > 0 && packet[0] == snapshotMagic }

// EncodeSnapshotChunk 校验后编码一个分块；First 由 Offset 决定（Offset = 0 写头部）。
func EncodeSnapshotChunk(chunk SnapshotChunk) ([]byte, error) {
	switch {
	case chunk.Transfer == 0:
		return nil, fmt.Errorf("%w: snapshot transfer 0", ErrFrameCorrupt)
	case chunk.Offset < 0 || len(chunk.Data) == 0:
		return nil, fmt.Errorf("%w: snapshot chunk offset %d, %d data bytes", ErrFrameCorrupt, chunk.Offset, len(chunk.Data))
	case chunk.Offset == 0 && (chunk.Total < len(chunk.Data) || chunk.Total > MaxSnapshotBytes):
		return nil, fmt.Errorf("%w: snapshot total %d", ErrFrameCorrupt, chunk.Total)
	}
	return appendSnapshotChunk(nil, chunk), nil
}

func appendSnapshotChunk(out []byte, chunk SnapshotChunk) []byte {
	out = append(out, snapshotMagic, snapshotVersion)
	out = binary.AppendUvarint(out, chunk.Transfer)
	out = binary.AppendUvarint(out, uint64(chunk.Offset))
	if chunk.Offset == 0 {
		out = binary.AppendUvarint(out, uint64(chunk.Frame))
		out = binary.AppendUvarint(out, uint64(chunk.Total))
		if chunk.HasStateHash {
			out = append(out, snapshotFlagStateHash)
			out = binary.AppendUvarint(out, chunk.StateHash)
		} else {
			out = append(out, 0)
		}
		out = append(out, chunk.Digest[:]...)
	}
	return append(out, chunk.Data...)
}

// DecodeSnapshotChunk 严格解码一个分块：坏头、零 transfer、帧号溢出、total 为 0 或超过 maxTotal、
// flags 保留位、首块数据超过 total、空数据都返回 ErrFrameCorrupt。maxTotal <= 0 取 DefaultMaxSnapshotBytes，
// 超过 MaxSnapshotBytes 按 MaxSnapshotBytes。非首块的越界（offset + len > total）由 SnapshotReceiver 判断。
func DecodeSnapshotChunk(packet []byte, maxTotal int) (SnapshotChunk, error) {
	maxTotal = snapshotLimit(maxTotal)
	var chunk SnapshotChunk
	if len(packet) < 5 || packet[0] != snapshotMagic || packet[1] != snapshotVersion {
		return chunk, fmt.Errorf("%w: bad snapshot chunk header", ErrFrameCorrupt)
	}
	transfer, rest, err := readUvarint(packet[2:])
	if err != nil {
		return chunk, err
	}
	offset, rest, err := readUvarint(rest)
	if err != nil {
		return chunk, err
	}
	if transfer == 0 || offset >= uint64(maxTotal) {
		return chunk, fmt.Errorf("%w: snapshot transfer %d offset %d", ErrFrameCorrupt, transfer, offset)
	}
	chunk.Transfer, chunk.Offset, chunk.First = transfer, int(offset), offset == 0
	if chunk.First {
		var frame, total uint64
		if frame, rest, err = readUvarint(rest); err != nil {
			return chunk, err
		}
		if total, rest, err = readUvarint(rest); err != nil {
			return chunk, err
		}
		if frame > uint64(^FrameID(0)) || total == 0 || total > uint64(maxTotal) || len(rest) < 1 {
			return chunk, fmt.Errorf("%w: snapshot frame %d total %d (limit %d)", ErrFrameCorrupt, frame, total, maxTotal)
		}
		flags := rest[0]
		rest = rest[1:]
		if flags&^snapshotFlagStateHash != 0 {
			return chunk, fmt.Errorf("%w: snapshot flags %#x", ErrFrameCorrupt, flags)
		}
		if flags&snapshotFlagStateHash != 0 {
			if chunk.StateHash, rest, err = readUvarint(rest); err != nil {
				return chunk, err
			}
			chunk.HasStateHash = true
		}
		if len(rest) < SnapshotDigestBytes {
			return chunk, fmt.Errorf("%w: truncated snapshot digest", ErrFrameCorrupt)
		}
		copy(chunk.Digest[:], rest[:SnapshotDigestBytes])
		rest = rest[SnapshotDigestBytes:]
		chunk.Frame, chunk.Total = FrameID(frame), int(total)
		if len(rest) > chunk.Total {
			return chunk, fmt.Errorf("%w: first snapshot chunk carries %d bytes of %d", ErrFrameCorrupt, len(rest), chunk.Total)
		}
	}
	if len(rest) == 0 {
		return chunk, fmt.Errorf("%w: empty snapshot chunk", ErrFrameCorrupt)
	}
	chunk.Data = rest
	return chunk, nil
}

func snapshotLimit(limit int) int {
	if limit <= 0 {
		return DefaultMaxSnapshotBytes
	}
	return min(limit, MaxSnapshotBytes)
}

// SnapshotReceiver 把 CA 分块重组成完整快照，并在交出前核对 SHA-256 摘要。
// 客户端单所有者状态（与 FrameAssembler 相同）：一个 goroutine 驱动。
type SnapshotReceiver struct {
	maxBytes int
	transfer uint64 // 最近一次见过的传输编号（完成后仍记着，用来丢弃它的重发分块）
	active   bool
	header   SnapshotChunk
	data     []byte
}

// NewSnapshotReceiver 创建重组器；maxBytes 是客户端愿意接受的快照上限（<= 0 取 DefaultMaxSnapshotBytes，
// 超过 MaxSnapshotBytes 按 MaxSnapshotBytes），首块声明的 total 超过它即坏包，不分配内存。
func NewSnapshotReceiver(maxBytes int) *SnapshotReceiver {
	return &SnapshotReceiver{maxBytes: snapshotLimit(maxBytes)}
}

// Active 报告是否有一次传输已开始但未完成。
func (r *SnapshotReceiver) Active() bool { return r.active }

// Received 返回当前传输已收到的字节数与总长（没有进行中的传输时为 0, 0）。
func (r *SnapshotReceiver) Received() (int, int) {
	if !r.active {
		return 0, 0
	}
	return len(r.data), r.header.Total
}

// Reset 丢弃进行中的传输（例如换连接）。之后同一编号的分块按新传输处理。
func (r *SnapshotReceiver) Reset() {
	r.active, r.data, r.transfer, r.header = false, nil, 0, SnapshotChunk{}
}

// Accept 处理一个分块包。未齐返回 (nil, nil)；重发的旧分块丢弃并返回 (nil, nil)；最后一块到齐且摘要
// 相符时返回完整快照。坏包返回 ErrFrameCorrupt，摘要不符返回 ErrSnapshotCorrupt；两者都丢弃进行中的传输。
func (r *SnapshotReceiver) Accept(packet []byte) (*ReceivedSnapshot, error) {
	chunk, err := DecodeSnapshotChunk(packet, r.maxBytes)
	if err != nil {
		r.Reset()
		return nil, err
	}
	if chunk.Transfer != r.transfer {
		if !chunk.First {
			r.Reset()
			return nil, fmt.Errorf("%w: snapshot transfer %d starts at offset %d", ErrFrameCorrupt, chunk.Transfer, chunk.Offset)
		}
		// 新传输取代未完成的旧传输。
		r.transfer, r.active, r.header = chunk.Transfer, true, chunk
		r.header.Data = nil
		r.data = make([]byte, 0, chunk.Total)
	} else {
		received := len(r.data)
		// 进行中的传输至少已收首块的 1 字节，所以同编号的首块重发也落在这一支。
		if !r.active || chunk.Offset < received {
			return nil, nil // 已完成传输或已确认偏移之前的重发
		}
		if chunk.Offset > received {
			r.Reset()
			return nil, fmt.Errorf("%w: snapshot chunk offset %d skips past %d", ErrFrameCorrupt, chunk.Offset, received)
		}
	}
	if len(r.data)+len(chunk.Data) > r.header.Total {
		r.Reset()
		return nil, fmt.Errorf("%w: snapshot chunk overruns total %d", ErrFrameCorrupt, r.header.Total)
	}
	r.data = append(r.data, chunk.Data...)
	if len(r.data) < r.header.Total {
		return nil, nil
	}
	sum := sha256.Sum256(r.data)
	header, data := r.header, r.data
	r.active, r.data = false, nil // 编号仍记着：之后到达的同编号重发分块被丢弃
	if !bytes.Equal(sum[:], header.Digest[:]) {
		return nil, fmt.Errorf("%w: frame %d, %d bytes", ErrSnapshotCorrupt, header.Frame, len(data))
	}
	return &ReceivedSnapshot{Frame: header.Frame, Data: data, StateHash: header.StateHash, HasStateHash: header.HasStateHash}, nil
}
