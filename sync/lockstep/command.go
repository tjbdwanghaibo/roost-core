package lockstep

import (
	"encoding/binary"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/sync/nettransport"
)

type Operation byte

const (
	OpInput   Operation = 1
	OpHash    Operation = 2
	OpCatchup Operation = 3
)

// Command 不含客户端自报座位；比赛单所有者通过当前会话绑定决定权限。
// C8 01 operation frame(uvarint)，Input带length+bytes，Hash带uvarint64。
type Command struct {
	Operation Operation
	Frame     FrameID
	Payload   []byte
	Hash      uint64
}

func (c Command) Validate() error {
	if c.Frame == 0 {
		return fmt.Errorf("%w: command frame is zero", ErrFrameCorrupt)
	}
	switch c.Operation {
	case OpInput:
		if len(c.Payload) > MaxInputPayloadBytes {
			return ErrPayloadTooBig
		}
		if c.Hash != 0 {
			return fmt.Errorf("%w: input carries hash", ErrFrameCorrupt)
		}
	case OpHash:
		if len(c.Payload) != 0 {
			return fmt.Errorf("%w: hash carries payload", ErrFrameCorrupt)
		}
	case OpCatchup:
		if len(c.Payload) != 0 || c.Hash != 0 {
			return fmt.Errorf("%w: catchup carries extra fields", ErrFrameCorrupt)
		}
	default:
		return fmt.Errorf("%w: unknown command %d", ErrFrameCorrupt, c.Operation)
	}
	return nil
}

func EncodeCommand(c Command) ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	out := binary.AppendUvarint([]byte{0xC8, 1, byte(c.Operation)}, uint64(c.Frame))
	switch c.Operation {
	case OpInput:
		out = binary.AppendUvarint(out, uint64(len(c.Payload)))
		out = append(out, c.Payload...)
	case OpHash:
		out = binary.AppendUvarint(out, c.Hash)
	}
	return out, nil
}

// DecodeCommand 拒绝坏包并复制输入；调用者可以立即释放接收池缓存。
func DecodeCommand(data []byte) (Command, error) {
	var c Command
	if len(data) < 4 || data[0] != 0xC8 || data[1] != 1 {
		return c, fmt.Errorf("%w: bad command header", ErrFrameCorrupt)
	}
	c.Operation = Operation(data[2])
	frame, rest, err := readUvarint(data[3:])
	if err != nil {
		return c, err
	}
	if frame == 0 || frame > uint64(^FrameID(0)) {
		return c, fmt.Errorf("%w: command frame overflow/zero", ErrFrameCorrupt)
	}
	c.Frame = FrameID(frame)
	switch c.Operation {
	case OpInput:
		var size uint64
		size, rest, err = readUvarint(rest)
		if err != nil {
			return c, err
		}
		if size > MaxInputPayloadBytes || size > uint64(len(rest)) {
			return c, fmt.Errorf("%w: command input length", ErrFrameCorrupt)
		}
		c.Payload = append([]byte(nil), rest[:size]...)
		rest = rest[size:]
	case OpHash:
		c.Hash, rest, err = readUvarint(rest)
		if err != nil {
			return c, err
		}
	case OpCatchup:
	default:
		return c, fmt.Errorf("%w: unknown command", ErrFrameCorrupt)
	}
	if len(rest) != 0 {
		return c, fmt.Errorf("%w: command trailing bytes", ErrFrameCorrupt)
	}
	return c, c.Validate()
}

// HandleCommand 必须在Room的串行handler内调用；传入由鉴权层映射的会话。
// 重绑后旧会话无法提交；旁观者只有追帧权限。没有发送成功即业务ACK的约定。
func (r *Room) HandleCommand(session nettransport.SessionID, c Command) error {
	if r.closed {
		return ErrRoomClosed
	}
	if err := c.Validate(); err != nil {
		return err
	}
	player, attached := r.sessionOwners[session]
	if !attached {
		return ErrPlayerDetached
	}
	if c.Operation == OpCatchup {
		if player == ownerSpectator {
			return r.SpectatorCatchup(session, c.Frame)
		}
		return r.StartCatchup(player, c.Frame)
	}
	if player == ownerSpectator {
		return ErrPlayerUnknown
	}
	if c.Operation == OpHash {
		return r.ReportHash(player, c.Frame, c.Hash)
	}
	_, err := r.SubmitInput(player, c.Frame, c.Payload)
	return err
}
