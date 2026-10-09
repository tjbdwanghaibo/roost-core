package bus

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/vmihailenco/msgpack/v5"
)

const (
	messagePackHeader = "RM\x01"
	MaxCodecBytes     = 16 << 20
	maxCodecDepth     = 64
)

var ErrCodecFormat = errors.New("bus: unsupported or malformed MessagePack format")

// MessagePackCodec 是 Bus 和 Gate 共用的当前内部编码。普通结构体按导出字段名编码，
// 不借用 JSON tag、不猜测旧格式。每次调用独占 encoder/decoder，返回字节归调用方所有。
type MessagePackCodec struct{}

type codecWriter struct{ bytes.Buffer }

func (writer *codecWriter) Write(data []byte) (int, error) {
	if len(data) > MaxCodecBytes-writer.Len() {
		return 0, fmt.Errorf("%w: encoded size exceeds %d", ErrCodecFormat, MaxCodecBytes)
	}
	return writer.Buffer.Write(data)
}

// msgpack 会直接使用 io.ByteWriter / StringWriter；三个入口必须共享同一字节上限。
func (writer *codecWriter) WriteByte(value byte) error {
	if writer.Len() >= MaxCodecBytes {
		return ErrCodecFormat
	}
	return writer.Buffer.WriteByte(value)
}

func (writer *codecWriter) WriteString(value string) (int, error) {
	if len(value) > MaxCodecBytes-writer.Len() {
		return 0, ErrCodecFormat
	}
	return writer.Buffer.WriteString(value)
}

func (MessagePackCodec) Marshal(value any) ([]byte, error) {
	var buffer codecWriter
	buffer.WriteString(messagePackHeader)
	encoder := msgpack.NewEncoder(&buffer)
	encoder.UseCompactInts(true)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	if buffer.Len() > MaxCodecBytes {
		return nil, fmt.Errorf("%w: encoded size exceeds %d", ErrCodecFormat, MaxCodecBytes)
	}
	return buffer.Bytes(), nil
}

func (MessagePackCodec) Unmarshal(data []byte, value any) error {
	if len(data) > MaxCodecBytes || !bytes.HasPrefix(data, []byte(messagePackHeader)) {
		return ErrCodecFormat
	}
	body := data[len(messagePackHeader):]
	// 先验证完整值的长度与深度。恶意 array/map 声明不能在读取实际数据前诱发巨量分配；
	// 也不能忽略尾部第二个值，让同一信封在不同调用方得到不同解释。
	end, err := messagePackValueEnd(body, 0, 0)
	if err != nil || end != len(body) {
		return ErrCodecFormat
	}
	decoder := msgpack.NewDecoder(bytes.NewReader(body))
	return decoder.Decode(value)
}

func messagePackValueEnd(data []byte, offset, depth int) (int, error) {
	if offset >= len(data) || depth >= maxCodecDepth {
		return 0, ErrCodecFormat
	}
	code := data[offset]
	offset++
	var count, payload int
	var lengthBytes int
	var container, extension bool
	switch {
	case code <= 0x7f || code >= 0xe0 || code == 0xc0 || code == 0xc2 || code == 0xc3:
		return offset, nil
	case code >= 0xa0 && code <= 0xbf:
		payload = int(code & 0x1f)
	case code >= 0x90 && code <= 0x9f:
		container, count = true, int(code&0x0f)
	case code >= 0x80 && code <= 0x8f:
		container, count = true, 2*int(code&0x0f)
	default:
		switch code {
		case 0xc4, 0xd9:
			lengthBytes = 1
		case 0xc5, 0xda:
			lengthBytes = 2
		case 0xc6, 0xdb:
			lengthBytes = 4
		case 0xc7:
			lengthBytes, extension = 1, true
		case 0xc8:
			lengthBytes, extension = 2, true
		case 0xc9:
			lengthBytes, extension = 4, true
		case 0xcc, 0xd0:
			payload = 1
		case 0xcd, 0xd1:
			payload = 2
		case 0xce, 0xd2, 0xca:
			payload = 4
		case 0xcf, 0xd3, 0xcb:
			payload = 8
		case 0xd4:
			payload = 2
		case 0xd5:
			payload = 3
		case 0xd6:
			payload = 5
		case 0xd7:
			payload = 9
		case 0xd8:
			payload = 17
		case 0xdc, 0xde:
			lengthBytes, container = 2, true
		case 0xdd, 0xdf:
			lengthBytes, container = 4, true
		default:
			return 0, ErrCodecFormat
		}
	}
	if lengthBytes != 0 {
		if lengthBytes > len(data)-offset {
			return 0, ErrCodecFormat
		}
		var length uint64
		switch lengthBytes {
		case 1:
			length = uint64(data[offset])
		case 2:
			length = uint64(binary.BigEndian.Uint16(data[offset:]))
		case 4:
			length = uint64(binary.BigEndian.Uint32(data[offset:]))
		}
		offset += lengthBytes
		if code == 0xde || code == 0xdf {
			length *= 2
		}
		if extension {
			length++
		}
		if length > uint64(len(data)-offset) {
			return 0, ErrCodecFormat
		}
		if container {
			count = int(length)
		} else {
			payload = int(length)
		}
	}
	if !container {
		if payload > len(data)-offset {
			return 0, ErrCodecFormat
		}
		return offset + payload, nil
	}
	if count > len(data)-offset {
		return 0, ErrCodecFormat
	}
	for range count {
		var err error
		offset, err = messagePackValueEnd(data, offset, depth+1)
		if err != nil {
			return 0, err
		}
	}
	return offset, nil
}
