package wire

import (
	"encoding/binary"
	"fmt"
)

// 鉴权首包（连接上的第一帧，message_id=0、flags=0）的载荷有两种写法：
//
//   - 票据原文：没有连接代号（缺省，老客户端与不关心跨 Gate 重连的客户端）。
//   - 代号信封：0x00、'G'、8 字节大端连接代号（非 0）、票据原文。
//
// 连接代号是客户端给这条连接的单调编号，同一客户端越晚发起的连接代号越大；服务端只在同一 SessionID 的两条连接
// 落在不同 Gate 实例、无法比较接受先后时用它判新旧（GATEWAY §3）。服务端在交给 Authenticator 之前剥掉信封，
// 业务鉴权只看到票据原文。票据原文因此不能以 0x00 开头。
const (
	authEnvelopeMarker   byte = 0x00
	authEnvelopeTag      byte = 'G'
	authEnvelopeOverhead      = 2 + 8
)

// EncodeAuth 写出鉴权首包的载荷：generation 为 0 时就是票据原文，否则是带连接代号的信封。
func EncodeAuth(ticket string, generation uint64) ([]byte, error) {
	if ticket == "" || ticket[0] == authEnvelopeMarker {
		return nil, fmt.Errorf("%w: auth ticket is empty or starts with 0x00", ErrInvalidPacket)
	}
	if generation == 0 {
		return []byte(ticket), nil
	}
	payload := make([]byte, authEnvelopeOverhead, authEnvelopeOverhead+len(ticket))
	payload[0], payload[1] = authEnvelopeMarker, authEnvelopeTag
	binary.BigEndian.PutUint64(payload[2:authEnvelopeOverhead], generation)
	return append(payload, ticket...), nil
}

// DecodeAuth 拆开鉴权首包的载荷，返回票据原文（与 payload 共用底层内存）与连接代号（没有信封时为 0）。
// 以 0x00 开头却不是完整信封（标记不对、代号为 0、票据为空）时拒绝，不把它当票据原文。
func DecodeAuth(payload []byte) (ticket []byte, generation uint64, err error) {
	if len(payload) == 0 {
		return nil, 0, fmt.Errorf("%w: empty auth payload", ErrInvalidPacket)
	}
	if payload[0] != authEnvelopeMarker {
		return payload, 0, nil
	}
	if len(payload) <= authEnvelopeOverhead || payload[1] != authEnvelopeTag {
		return nil, 0, fmt.Errorf("%w: malformed auth envelope", ErrInvalidPacket)
	}
	generation = binary.BigEndian.Uint64(payload[2:authEnvelopeOverhead])
	ticket = payload[authEnvelopeOverhead:]
	if generation == 0 || ticket[0] == authEnvelopeMarker {
		return nil, 0, fmt.Errorf("%w: malformed auth envelope", ErrInvalidPacket)
	}
	return ticket, generation, nil
}
