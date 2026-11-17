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
// 64～127 是换 host（FEATURE-2026-11-16-lobby-relay §4.6，见本文件末尾）。不认识的操作码 relay 丢弃，不断开。
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
	RelayReasonRoomClosed    RelayReason = 4  // 房间关闭（房主走了且选不出新房主）
	RelayReasonFrameTooBig   RelayReason = 5  // 单帧超过上限
	RelayReasonRateLimited   RelayReason = 6  // 每秒帧数超过上限
	RelayReasonSlowConsumer  RelayReason = 7  // 发送队列满（只出现在 MemberLeft 与 NATS 报告里）
	RelayReasonIdleTimeout   RelayReason = 8  // 空闲超时（连接上没有任何帧，或房间里只剩房主太久）
	RelayReasonProtocol      RelayReason = 9  // 帧头非法、首帧不是令牌、控制帧格式错误
	RelayReasonTokenRejected RelayReason = 10 // 令牌验签失败、过期或不是发给本实例的
	RelayReasonRoomNotFound  RelayReason = 11 // 成员令牌对应的房间不存在（房主还没到，或房间已关闭）
	RelayReasonRoomFull      RelayReason = 12 // 在线人数已达令牌里的上限
	RelayReasonShutdown      RelayReason = 13 // relay 进程停机
	RelayReasonLeft          RelayReason = 14 // 玩家经 lobby 的 leave（或 RelayOpLeave）离开
	// RelayReasonLobbyRejected：令牌验签通过，但 lobby 验权不让进（房间不在、不分在这个 relay、不是成员、被踢、
	// 建房的不是房主）。客户端重新向 lobby join 才可能进来。
	RelayReasonLobbyRejected RelayReason = 15
	// RelayReasonLobbyUnavailable：向 lobby 验权失败或超时（lobby 不可用）。失败即关闭，客户端稍后重试。
	RelayReasonLobbyUnavailable RelayReason = 16
)

var relayReasonNames = [...]string{"none", "disconnected", "kicked", "replaced", "room_closed", "frame_too_big", "rate_limited",
	"slow_consumer", "idle_timeout", "protocol", "token_rejected", "room_not_found", "room_full", "shutdown", "left",
	"lobby_rejected", "lobby_unavailable"}

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

// 换 host 的控制帧（FEATURE-2026-11-16-lobby-relay §4.6；docs/framework/RELAY.md §2.3）。64～79 由 relay 发出，
// 80～95 由客户端发出。epoch 都是 8 字节大端的 host_epoch：建房时为 0，每次换 host 加 1。
const (
	// RelayOpYouAreMember：relay → 不再是（或没能成为）房主的连接。Seq=自己的槽位；载荷 [epoch 8B]。
	// 主动转交后的旧房主、没在时限内回 host_ready 的候选人收到它；之后它发的游戏帧按成员帧处理。
	RelayOpYouAreMember uint32 = 64
	// RelayOpYouAreHost：relay → 被选中的新房主候选。Seq=自己的槽位；载荷见 RelayYouAreHost。
	// 候选要在 host_ready_timeout 内回 RelayOpHostReady，否则 relay 换下一个人。
	RelayOpYouAreHost uint32 = 65
	// RelayOpHostChanging：relay → 成员。房主不在（宽限期或正在选新房主），这个成员发给房主的游戏帧被丢弃了；
	// 每次换 host 每个成员最多收到一次。Seq=0；载荷 [epoch 8B]（宽限期内是当前的 epoch，选人阶段是新的 epoch）。
	RelayOpHostChanging uint32 = 66
	// RelayOpHostChanged：relay → 房间里在线的每个人（含新房主），新房主已就绪，游戏帧恢复转发。
	// Seq=新房主的槽位；载荷见 RelayHostChanged。
	RelayOpHostChanged uint32 = 67
	// RelayOpHostResumed：relay → 成员，房主在宽限期内重连回来了，仍是原来的房主。Seq=房主槽位；载荷 [epoch 8B]。
	RelayOpHostResumed uint32 = 68
	// RelayOpRejected：relay → 房主，它发的快照或转交被拒（连接保留）。Seq=0；载荷见 RelayRejected。
	RelayOpRejected uint32 = 69

	// RelayOpTransfer：房主 → relay，主动把房主转交给槽位 Seq 上的在线成员。载荷是交接数据（不透明，
	// 不超过 snapshot_max_bytes），relay 原样放进新房主的 RelayOpYouAreHost。
	RelayOpTransfer uint32 = 80
	// RelayOpLeave：任何人 → relay，离开房间（等同于经 lobby 的 leave）。Seq=0、无载荷。房主离开不等宽限期，
	// 立即用 relay 存的快照换 host。relay 回 Closed{left} 后断开。
	RelayOpLeave uint32 = 81
	// RelayOpSuccessors：房主 → relay，声明候选顺序。Seq=0；载荷是槽位列表，每个 4 字节大端，至多 RelayMaxSuccessors 个。
	// relay 只记最新一份，不判断合不合理。
	RelayOpSuccessors uint32 = 82
	// RelayOpSnapshot：房主 → relay，最新快照。Seq=0；载荷见 RelaySnapshot。relay 只存最新一份，不解析。
	RelayOpSnapshot uint32 = 83
	// RelayOpHostReady：新房主候选 → relay，已接手。Seq=0；载荷 [epoch 8B]，必须等于 RelayOpYouAreHost 里的 epoch。
	RelayOpHostReady uint32 = 84
)

// RelayMaxSuccessors 是 RelayOpSuccessors 里槽位的个数上限。
const RelayMaxSuccessors = 256

// EncodeRelayEpoch / DecodeRelayEpoch 是只带 epoch 的载荷（YouAreMember、HostChanging、HostResumed、HostReady）。
func EncodeRelayEpoch(epoch uint64) []byte { return binary.BigEndian.AppendUint64(nil, epoch) }

func DecodeRelayEpoch(payload []byte) (uint64, error) {
	if len(payload) != 8 {
		return 0, fmt.Errorf("%w: relay epoch", ErrInvalidPacket)
	}
	return binary.BigEndian.Uint64(payload), nil
}

// RelayHostChanged 是 RelayOpHostChanged 的载荷：[epoch 8B][新房主账号 UTF-8]。新房主槽位在 Seq。
type RelayHostChanged struct {
	Epoch   uint64
	Account string
}

func EncodeRelayHostChanged(changed RelayHostChanged) []byte {
	return append(binary.BigEndian.AppendUint64(nil, changed.Epoch), changed.Account...)
}

func DecodeRelayHostChanged(payload []byte) (RelayHostChanged, error) {
	if len(payload) < 9 {
		return RelayHostChanged{}, fmt.Errorf("%w: relay host changed", ErrInvalidPacket)
	}
	return RelayHostChanged{Epoch: binary.BigEndian.Uint64(payload), Account: string(payload[8:])}, nil
}

// RelayStateSource 说明 RelayYouAreHost.State 从哪来。
type RelayStateSource byte

const (
	RelayStateNone     RelayStateSource = 0 // 没有可交的状态：新房主用自己本地的状态，或由游戏决定重开
	RelayStateHandoff  RelayStateSource = 1 // 主动转交时旧房主随 transfer 带来的交接数据
	RelayStateSnapshot RelayStateSource = 2 // relay 存的最新快照（房主离开或断线）
)

// RelaySeat 是 RelayYouAreHost 里的一个成员：房间存续期间进过房、没被踢的账号，含离线的。
type RelaySeat struct {
	Slot    uint32
	Account string
	Online  bool
}

// RelayYouAreHost 是 RelayOpYouAreHost 的载荷：
//
//	[epoch 8B][来源 1B][快照 seq 8B][快照年龄毫秒 4B][成员数 2B]
//	成员数 × { [槽位 4B][在线 1B：1 在线 / 0 离线][账号长度 2B][账号 UTF-8] }
//	[状态字节：剩下的全部]
//
// 来源为 RelayStateSnapshot 时 SnapshotSeq / SnapshotAgeMillis 是那份快照的 seq 与 relay 收到它到现在的毫秒数
// （新房主据此知道快照落后了多少）；其余来源这两项为 0。整数都是大端。
type RelayYouAreHost struct {
	Epoch             uint64
	Source            RelayStateSource
	SnapshotSeq       uint64
	SnapshotAgeMillis uint32
	Members           []RelaySeat
	State             []byte
}

func EncodeRelayYouAreHost(host RelayYouAreHost) []byte {
	size := 23 + len(host.State)
	for _, seat := range host.Members {
		size += 7 + len(seat.Account)
	}
	out := make([]byte, 0, size)
	out = binary.BigEndian.AppendUint64(out, host.Epoch)
	out = append(out, byte(host.Source))
	out = binary.BigEndian.AppendUint64(out, host.SnapshotSeq)
	out = binary.BigEndian.AppendUint32(out, host.SnapshotAgeMillis)
	out = binary.BigEndian.AppendUint16(out, uint16(len(host.Members)))
	for _, seat := range host.Members {
		out = binary.BigEndian.AppendUint32(out, seat.Slot)
		online := byte(0)
		if seat.Online {
			online = 1
		}
		out = append(out, online)
		out = binary.BigEndian.AppendUint16(out, uint16(len(seat.Account)))
		out = append(out, seat.Account...)
	}
	return append(out, host.State...)
}

func DecodeRelayYouAreHost(payload []byte) (RelayYouAreHost, error) {
	bad := fmt.Errorf("%w: relay you_are_host", ErrInvalidPacket)
	if len(payload) < 23 || payload[8] > byte(RelayStateSnapshot) {
		return RelayYouAreHost{}, bad
	}
	host := RelayYouAreHost{Epoch: binary.BigEndian.Uint64(payload), Source: RelayStateSource(payload[8]),
		SnapshotSeq: binary.BigEndian.Uint64(payload[9:]), SnapshotAgeMillis: binary.BigEndian.Uint32(payload[17:])}
	count := int(binary.BigEndian.Uint16(payload[21:]))
	rest := payload[23:]
	host.Members = make([]RelaySeat, 0, count)
	for range count {
		if len(rest) < 7 || rest[4] > 1 {
			return RelayYouAreHost{}, bad
		}
		n := int(binary.BigEndian.Uint16(rest[5:]))
		if len(rest) < 7+n {
			return RelayYouAreHost{}, bad
		}
		host.Members = append(host.Members, RelaySeat{Slot: binary.BigEndian.Uint32(rest), Online: rest[4] == 1, Account: string(rest[7 : 7+n])})
		rest = rest[7+n:]
	}
	host.State = append([]byte(nil), rest...)
	return host, nil
}

// RelaySnapshot 是 RelayOpSnapshot 的载荷：[seq 8B][快照字节]。seq 在房间内必须递增（跨房主：新房主从
// RelayYouAreHost.SnapshotSeq 往上接）；快照字节由房主客户端压缩，relay 不压缩、不解压、不解析。
type RelaySnapshot struct {
	Seq  uint64
	Data []byte
}

func EncodeRelaySnapshot(snapshot RelaySnapshot) []byte {
	return append(binary.BigEndian.AppendUint64(make([]byte, 0, 8+len(snapshot.Data)), snapshot.Seq), snapshot.Data...)
}

func DecodeRelaySnapshot(payload []byte) (RelaySnapshot, error) {
	if len(payload) < 8 {
		return RelaySnapshot{}, fmt.Errorf("%w: relay snapshot", ErrInvalidPacket)
	}
	return RelaySnapshot{Seq: binary.BigEndian.Uint64(payload), Data: payload[8:]}, nil
}

// EncodeRelaySuccessors / DecodeRelaySuccessors 是 RelayOpSuccessors 的载荷：槽位列表，每个 4 字节大端。
func EncodeRelaySuccessors(slots []uint32) []byte {
	out := make([]byte, 0, 4*len(slots))
	for _, slot := range slots {
		out = binary.BigEndian.AppendUint32(out, slot)
	}
	return out
}

func DecodeRelaySuccessors(payload []byte) ([]uint32, error) {
	if len(payload)%4 != 0 || len(payload)/4 > RelayMaxSuccessors {
		return nil, fmt.Errorf("%w: relay successors", ErrInvalidPacket)
	}
	slots := make([]uint32, len(payload)/4)
	for i := range slots {
		slots[i] = binary.BigEndian.Uint32(payload[4*i:])
	}
	return slots, nil
}

// RelayRejectCode 是 RelayOpRejected 的原因。
type RelayRejectCode byte

const (
	RelayRejectTooBig    RelayRejectCode = 1 // 快照或交接数据超过 snapshot_max_bytes
	RelayRejectTooSoon   RelayRejectCode = 2 // 离上一份快照不到 snapshot_min_interval
	RelayRejectStaleSeq  RelayRejectCode = 3 // 快照 seq 不大于 relay 存着的那一份
	RelayRejectBadTarget RelayRejectCode = 4 // 转交的目标不在线，或是房主自己
	RelayRejectNotHost   RelayRejectCode = 5 // 发送者此刻不是房主（已被换下，或正在换 host）
)

// RelayRejected 是 RelayOpRejected 的载荷：[被拒的操作码 1B][原因 1B][seq 8B]。seq 是被拒快照的 seq，转交为 0。
type RelayRejected struct {
	Op   uint32
	Code RelayRejectCode
	Seq  uint64
}

func EncodeRelayRejected(rejected RelayRejected) []byte {
	return binary.BigEndian.AppendUint64([]byte{byte(rejected.Op), byte(rejected.Code)}, rejected.Seq)
}

func DecodeRelayRejected(payload []byte) (RelayRejected, error) {
	if len(payload) != 10 {
		return RelayRejected{}, fmt.Errorf("%w: relay rejected", ErrInvalidPacket)
	}
	return RelayRejected{Op: uint32(payload[0]), Code: RelayRejectCode(payload[1]), Seq: binary.BigEndian.Uint64(payload[2:])}, nil
}
