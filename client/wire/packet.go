// Package wire 定义客户端公共包头；TCP、WebSocket 和客户端 SDK 使用同一格式。
// 它不解释 PB/Sync/Lockstep 业务载荷，不负责鉴权、请求排序或服务器权威状态。
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	HeaderSize      = 16
	Version    byte = 2
	FlagPush   byte = 1 << 0
	FlagSync   byte = 1 << 1
	// bits1..2为载荷类型：00=PB、01=Sync、10=Lockstep。
	FlagLockstep      byte = 1 << 2
	DefaultMaxPayload      = 1 << 20
)

var (
	ErrInvalidPacket = errors.New("client wire: invalid packet")
	ErrPacketTooBig  = errors.New("client wire: packet too big")
)

// PayloadKind 是业务载荷类型，与推送位独立。
type PayloadKind byte

const (
	PayloadProtobuf PayloadKind = iota
	PayloadSync
	PayloadLockstep
)

func (h Header) Kind() PayloadKind { return PayloadKind((h.Flags >> 1) & 3) }

// Header 使用网络大端：RS、版本、flags、消息ID、序列号、载荷长度。
// flags 的其余位必须为零（bits3..4 只在 relay 连接上是通道位，见 relay.go）；消息ID为零的控制握手不得设置业务或推送标志。
type Header struct {
	Flags       byte
	MsgID       uint32
	Seq         uint32
	PayloadSize uint32
}

// Packet 独占 Payload；解包复制数据，不将接收缓存暴露给业务层。
type Packet struct {
	Flags   byte
	MsgID   uint32
	Seq     uint32
	Payload []byte
}

// Validate 是 Gate / 内嵌 TCP 的校验：relay 通道位（bits3..4）在这里仍是保留位，Gate 收到 relay 帧当非法包拒绝。
func (h Header) Validate(maxPayload int) error { return h.validate(maxPayload, false) }

func (h Header) validate(maxPayload int, relay bool) error {
	allowed := FlagPush | FlagSync | FlagLockstep
	if relay {
		allowed |= relayChannelMask
	}
	if h.Flags&^allowed != 0 || h.Flags&(FlagSync|FlagLockstep) == (FlagSync|FlagLockstep) {
		return ErrInvalidPacket
	}
	switch h.RelayChannel() {
	case RelayNone:
		if h.MsgID == 0 && h.Flags != 0 {
			return ErrInvalidPacket
		}
	case RelayControl:
		// 控制帧只认操作码，不带推送位与载荷类型。
		if h.MsgID == 0 || h.Flags != FlagRelayControl {
			return ErrInvalidPacket
		}
	}
	if maxPayload <= 0 {
		maxPayload = DefaultMaxPayload
	}
	if uint64(h.PayloadSize) > uint64(maxPayload) {
		return ErrPacketTooBig
	}
	return nil
}

// ParseHeader 在申请载荷内存之前检查版本、保留位及长度上限。
func ParseHeader(data []byte, maxPayload int) (Header, error) {
	return parseHeader(data, maxPayload, false)
}

func parseHeader(data []byte, maxPayload int, relay bool) (Header, error) {
	if len(data) != HeaderSize || data[0] != 'R' || data[1] != 'S' || data[2] != Version {
		return Header{}, ErrInvalidPacket
	}
	h := Header{Flags: data[3], MsgID: binary.BigEndian.Uint32(data[4:8]), Seq: binary.BigEndian.Uint32(data[8:12]), PayloadSize: binary.BigEndian.Uint32(data[12:16])}
	return h, h.validate(maxPayload, relay)
}

func (h Header) Encode(maxPayload int) ([HeaderSize]byte, error) { return h.encode(maxPayload, false) }

func (h Header) encode(maxPayload int, relay bool) ([HeaderSize]byte, error) {
	var data [HeaderSize]byte
	if err := h.validate(maxPayload, relay); err != nil {
		return data, err
	}
	data[0], data[1], data[2], data[3] = 'R', 'S', Version, h.Flags
	binary.BigEndian.PutUint32(data[4:8], h.MsgID)
	binary.BigEndian.PutUint32(data[8:12], h.Seq)
	binary.BigEndian.PutUint32(data[12:16], h.PayloadSize)
	return data, nil
}

func Encode(packets []*Packet, maxPayload int) ([]byte, error) {
	return encode(packets, maxPayload, false)
}

func encode(packets []*Packet, maxPayload int, relay bool) ([]byte, error) {
	var out []byte
	for _, p := range packets {
		if p == nil {
			continue
		}
		if uint64(len(p.Payload)) > uint64(^uint32(0)) {
			return nil, ErrPacketTooBig
		}
		h, err := (Header{Flags: p.Flags, MsgID: p.MsgID, Seq: p.Seq, PayloadSize: uint32(len(p.Payload))}).encode(maxPayload, relay)
		if err != nil {
			return nil, err
		}
		out = append(out, h[:]...)
		out = append(out, p.Payload...)
	}
	return out, nil
}

func Decode(data []byte, maxPayload int) ([]*Packet, error) { return decode(data, maxPayload, false) }

func decode(data []byte, maxPayload int, relay bool) ([]*Packet, error) {
	var packets []*Packet
	for len(data) > 0 {
		if len(data) < HeaderSize {
			return nil, ErrInvalidPacket
		}
		h, err := parseHeader(data[:HeaderSize], maxPayload, relay)
		if err != nil {
			return nil, err
		}
		// 先比较实际缓冲长度，避免32位平台的整型加法溢出。
		if uint64(h.PayloadSize) > uint64(len(data)-HeaderSize) {
			return nil, fmt.Errorf("%w: incomplete payload", ErrInvalidPacket)
		}
		end := HeaderSize + int(h.PayloadSize)
		packets = append(packets, &Packet{Flags: h.Flags, MsgID: h.MsgID, Seq: h.Seq, Payload: append([]byte(nil), data[HeaderSize:end]...)})
		data = data[end:]
	}
	return packets, nil
}

func Read(reader io.Reader, maxPayload int) (*Packet, error) { return read(reader, maxPayload, false) }

func read(reader io.Reader, maxPayload int, relay bool) (*Packet, error) {
	var data [HeaderSize]byte
	if _, err := io.ReadFull(reader, data[:]); err != nil {
		return nil, err
	}
	h, err := parseHeader(data[:], maxPayload, relay)
	if err != nil {
		return nil, err
	}
	p := &Packet{Flags: h.Flags, MsgID: h.MsgID, Seq: h.Seq, Payload: make([]byte, int(h.PayloadSize))}
	_, err = io.ReadFull(reader, p.Payload)
	return p, err
}

func Write(writer io.Writer, packets []*Packet, maxPayload int) error {
	data, err := Encode(packets, maxPayload)
	if err != nil {
		return err
	}
	return writeAll(writer, data)
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
