package wire

import (
	"encoding/binary"
	"fmt"
	"io"
)

// relay 帧（docs/framework/RELAY.md §2）：与 Gate 同一个 16 字节包头，区别只在两处。
//
//   - flags 的 bits3..4 是 relay 通道：01 游戏通道、10 房间通道、11 控制帧。00 表示不是 relay 帧，
//     只有连接上的第一帧（令牌，message_id=0、flags=0）这样写。bits0..2（推送位、载荷类型）在数据帧上由游戏自用，
//     relay 原样带过；控制帧上必须为零。bits5..7 仍是保留位。
//   - Seq 字段在 relay 帧上是槽位：客户端发出的帧写目标槽位（RelayAll=0 表示全体），relay 转出的帧改写为发送者的槽位；
//     控制帧上是控制对象的槽位。message_id 在数据帧上由游戏自用（可以为 0），在控制帧上是操作码（非 0）。
//
// Gate 的 Validate / Read / Write 不认通道位，relay 帧发到 Gate 当非法包拒绝；relay 连接用下面的 *Relay 系列读写。
const (
	FlagRelayGame    byte = 1 << 3
	FlagRelayRoom    byte = 2 << 3
	FlagRelayControl byte = 3 << 3
	relayChannelMask byte = 3 << 3
)

// RelayChannel 是 relay 帧的通道（flags 的 bits3..4）。
type RelayChannel byte

const (
	RelayNone    RelayChannel = iota // 不是 relay 帧：只有令牌首帧
	RelayGame                        // 游戏通道：成员的帧只到房主，房主的帧按目标转出
	RelayRoom                        // 房间通道：聊天、表情、语音，按目标直达，房主也是收件人
	RelayControl                     // 控制帧：relay 与客户端之间的信令，message_id 是操作码
)

// RelayAll 是目标槽位 0：发给全体（不含发送者自己）。
const RelayAll uint32 = 0

func (h Header) RelayChannel() RelayChannel { return RelayChannel((h.Flags & relayChannelMask) >> 3) }
func (p *Packet) RelayChannel() RelayChannel {
	return RelayChannel((p.Flags & relayChannelMask) >> 3)
}

// ValidateRelay 是 relay 连接上的校验：在 Validate 之上放开通道位；控制帧要求 message_id 非 0、flags 只有通道位。
func (h Header) ValidateRelay(maxPayload int) error { return h.validate(maxPayload, true) }

// ParseRelayHeader 与 ParseHeader 相同，按 ValidateRelay 校验。
func ParseRelayHeader(data []byte, maxPayload int) (Header, error) {
	return parseHeader(data, maxPayload, true)
}

// ReadRelay / WriteRelay / EncodeRelay / DecodeRelay 是 Read / Write / Encode / Decode 的 relay 版本。
func ReadRelay(reader io.Reader, maxPayload int) (*Packet, error) {
	return read(reader, maxPayload, true)
}
func EncodeRelay(packets []*Packet, maxPayload int) ([]byte, error) {
	return encode(packets, maxPayload, true)
}
func DecodeRelay(data []byte, maxPayload int) ([]*Packet, error) {
	return decode(data, maxPayload, true)
}
func WriteRelay(writer io.Writer, packets []*Packet, maxPayload int) error {
	data, err := EncodeRelay(packets, maxPayload)
	if err != nil {
		return err
	}
	return writeAll(writer, data)
}

// 控制帧操作码（控制帧的 message_id）。1～15 由 relay 发出，16～31 由房主发出，32～ 任何一端都可以发；
// 64～127 留给换 host（FEATURE-2026-11-16-lobby-relay §4.6）。不认识的操作码 relay 丢弃，不断开。
const (
	// RelayOpWelcome：relay → 刚接入的连接。Seq=自己的槽位；载荷见 RelayWelcome。
	RelayOpWelcome uint32 = 1
	// RelayOpMemberJoined：relay → 房主，成员上线。Seq=成员槽位；载荷见 RelayMember（Reason 为 0 是新加入，
	// RelayReasonReplaced 是同一账号重连顶掉了旧连接，槽位不变）。
	RelayOpMemberJoined uint32 = 2
	// RelayOpMemberLeft：relay → 房主，成员下线。Seq=成员槽位；载荷见 RelayMember。
	RelayOpMemberLeft uint32 = 3
	// RelayOpClosed：relay → 某条连接，随后 relay 断开它。Seq=0；载荷 1 字节原因（RelayReason）。
	// 发送队列已满被断开的慢连接收不到这一帧。
	RelayOpClosed uint32 = 4
	// RelayOpMuted：relay → 被禁言 / 解除禁言的成员。Seq=自己的槽位；载荷 1 字节：1 禁言、0 解除。
	RelayOpMuted uint32 = 5

	// RelayOpKick：房主 → relay，踢出槽位 Seq。该账号在这个房间里之后的连接一律拒绝（RelayReasonKicked）。
	RelayOpKick uint32 = 16
	// RelayOpMute：房主 → relay，禁言槽位 Seq 的房间通道。载荷 1 字节：1 禁言、0 解除。
	RelayOpMute uint32 = 17

	// RelayOpPing：客户端 → relay，心跳（重置空闲计时）。载荷不透明，不超过 RelayMaxPingBytes。
	RelayOpPing uint32 = 32
	// RelayOpPong：relay → 客户端，原样带回 Ping 的载荷，客户端可以据此测往返时延。
	RelayOpPong uint32 = 33
)

// RelayMaxPingBytes 是 Ping 载荷的上限；超出的 Ping 当协议错误断开。
const RelayMaxPingBytes = 16

// RelayReason 是 RelayOpClosed 与 RelayOpMemberLeft 的原因码（1 字节）。
type RelayReason byte

const (
	RelayReasonNone          RelayReason = 0
	RelayReasonDisconnected  RelayReason = 1  // 连接断开（读写出错、客户端关闭）
	RelayReasonKicked        RelayReason = 2  // 被房主踢出
	RelayReasonReplaced      RelayReason = 3  // 同一账号的新连接顶掉了这一条
	RelayReasonRoomClosed    RelayReason = 4  // 房间关闭（这一批：房主断开）
	RelayReasonFrameTooBig   RelayReason = 5  // 单帧超过上限
	RelayReasonRateLimited   RelayReason = 6  // 每秒帧数超过上限
	RelayReasonSlowConsumer  RelayReason = 7  // 发送队列满（只出现在 MemberLeft 与 NATS 报告里）
	RelayReasonIdleTimeout   RelayReason = 8  // 空闲超时（连接上没有任何帧，或房间里只剩房主太久）
	RelayReasonProtocol      RelayReason = 9  // 帧头非法、首帧不是令牌、控制帧格式错误
	RelayReasonTokenRejected RelayReason = 10 // 令牌验签失败、过期或不是发给本实例的
	RelayReasonRoomNotFound  RelayReason = 11 // 成员令牌对应的房间不存在（房主还没到，或房间已关闭）
	RelayReasonRoomFull      RelayReason = 12 // 在线人数已达令牌里的上限
	RelayReasonShutdown      RelayReason = 13 // relay 进程停机
)

var relayReasonNames = [...]string{"none", "disconnected", "kicked", "replaced", "room_closed", "frame_too_big", "rate_limited",
	"slow_consumer", "idle_timeout", "protocol", "token_rejected", "room_not_found", "room_full", "shutdown"}

// String 是原因码的小写名字（NATS 报告、日志用）。
func (r RelayReason) String() string {
	if int(r) < len(relayReasonNames) {
		return relayReasonNames[r]
	}
	return fmt.Sprintf("reason_%d", byte(r))
}

// RelayWelcome 是 RelayOpWelcome 的载荷：[角色 1 字节：1 房主、2 成员][房主槽位 4 字节大端]。
type RelayWelcome struct {
	Host     bool
	HostSlot uint32
}

func EncodeRelayWelcome(welcome RelayWelcome) []byte {
	payload := make([]byte, 5)
	payload[0] = 2
	if welcome.Host {
		payload[0] = 1
	}
	binary.BigEndian.PutUint32(payload[1:], welcome.HostSlot)
	return payload
}

func DecodeRelayWelcome(payload []byte) (RelayWelcome, error) {
	if len(payload) != 5 || (payload[0] != 1 && payload[0] != 2) {
		return RelayWelcome{}, fmt.Errorf("%w: relay welcome", ErrInvalidPacket)
	}
	return RelayWelcome{Host: payload[0] == 1, HostSlot: binary.BigEndian.Uint32(payload[1:])}, nil
}

// RelayMember 是 RelayOpMemberJoined / RelayOpMemberLeft 的载荷：[原因 1 字节][账号 UTF-8]。
type RelayMember struct {
	Reason  RelayReason
	Account string
}

func EncodeRelayMember(member RelayMember) []byte {
	return append([]byte{byte(member.Reason)}, member.Account...)
}

func DecodeRelayMember(payload []byte) (RelayMember, error) {
	if len(payload) < 2 {
		return RelayMember{}, fmt.Errorf("%w: relay member", ErrInvalidPacket)
	}
	return RelayMember{Reason: RelayReason(payload[0]), Account: string(payload[1:])}, nil
}
