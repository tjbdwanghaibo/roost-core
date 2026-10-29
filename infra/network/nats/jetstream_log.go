package nats

import (
	"context"
	"errors"
	"time"
)

// JetStreamLog 是把 JetStream 流当作权威事件日志所需的原语（Saga 协调器用，设计见
// docs/maintenance/DESIGN-2026-10-28-saga-jetstream-authority.md §3.2）。实现只在 driver 包里，接口不带 nats.go 的类型。
//
// 前提：服务端 nats-server ≥ MinLogServerVersion，流为 R3 文件存储；收到 PubAck 即视为不丢（整集群同时断电不在范围）。
//
// 写入的隔离靠“通配期望序号”：PublishFenced 的首条消息带 Nats-Expected-Last-Subject-Sequence-Subject = fence.Filter 与
// Nats-Expected-Last-Subject-Sequence = fence.LastSeq，服务端按“流里匹配 Filter 的最后一条消息的序号”比较，不等就整次拒绝。
// 同一个 Filter 只有一个写者时，别的写者（崩溃重启的旧进程）写进去的任何一条都会让旧写者下一次发布被拒。
//
// 结果分三类，调用方据此决定重发还是停下：
//   - 返回 nil：已持久。单条可能是 Duplicate（同一 MsgID 在去重窗口内已存在，即之前那次已经落库）。
//   - errors.Is(err, ErrLogOutcomeUnknown)：没收到服务端答复（超时、断连、选主），可能已经落库。用同样的消息（同 MsgID）
//     与同一个 fence 重发即可判定：成功 / Duplicate = 已提交；ErrLogDuplicateBatch = 批之前已提交；
//     ErrLogWrongLastSequence 时读 LastMsg(fence.Filter)，MsgID 是自己这次的最后一条 = 已提交，否则 = 被别的写者隔离。
//   - 其余错误：这一次调用确定没有写入（服务端明确拒绝，或发送前就失败）。
type JetStreamLog interface {
	// EnsureLogStream 建流或把已有流更新成 config，再核对服务端实际生效的配置：文件存储、AllowAtomicPublish、
	// AllowDirect、Discard=New、MaxAge=0（权威日志不能按时间或容量静默删旧，满了拒新写）、副本数。
	// 服务端版本低于 MinLogServerVersion 或生效配置不符时返回 ErrLogUnsupportedServer。
	EnsureLogStream(ctx context.Context, config LogStreamConfig) (LogStreamInfo, error)

	// PublishFenced 一次提交 msgs：1 条用普通发布，多条用原子批量（全部写入或全部不写）。fence.Filter 非空时首条带
	// 通配期望序号。每条消息必须有 MsgID（重发判定依赖它）；条数超过 LogBatchMaxMessages 返回 ErrLogBatchTooLarge、
	// 不发送。批号由驱动每次调用新生成，同一组消息重发不会撞上服务端未清理的半批。结果分类见接口注释。
	PublishFenced(ctx context.Context, msgs []LogMsg, fence LogFence) (LogPublishAck, error)

	// LastMsg 读流里匹配 filter（可带通配）的最后一条消息，没有返回 ErrLogNotFound。读的是流 leader（不走 direct get：
	// 落后的副本可能回答旧消息），用于重放终点与结果未知的判定。
	LastMsg(ctx context.Context, stream, filter string) (LogMsg, error)

	// ReadRange 按流序号从 from 起顺序读匹配 filters 的消息，逐条交给 fn（同一 goroutine、序号严格递增），读到序号 ≥ to
	// 的那条后返回。to == 0 表示读到“追上”为止：某条交付时它之后已没有待交付的匹配消息（读消费者所在副本的视角，
	// 可能落后 leader）。需要确定终点时传 to = LastMsg(...).Seq，调用方保证这条消息存在且匹配 filters，否则等到 ctx 结束。
	// fn 返回错误即停止并原样返回它；ctx 结束返回 ctx 错误。
	ReadRange(ctx context.Context, stream string, filters []string, from, to uint64, fn func(LogMsg) error) error

	// Follow 用有序消费者从 from 起（from == 0 表示只要之后的新消息）持续读匹配 filters 的消息，在一个 goroutine 里
	// 逐条调用 fn（序号严格递增、不重复）。ctx 只约束建立；消费持续到 LogFollow.Stop。消费者断线、心跳丢失、选主后
	// 由驱动从最后交付序号之后重建，不跳号。fn 阻塞会拖住后续交付，应尽快返回。
	Follow(ctx context.Context, stream string, filters []string, from uint64, fn func(LogMsg)) (LogFollow, error)

	// Purge 删除流里匹配 filter（空表示整条流）且序号小于 below 的消息；below ≤ 1 什么也不做。
	Purge(ctx context.Context, stream, filter string, below uint64) error

	// DurableAckFloors 列出流上每个持久消费者（有 Durable 名）的有效 ack floor：流序号，不超过它的、匹配该消费者过滤的
	// 消息都已确认。floor 只用消费者 Raft 状态（服务端 AckFloor）和流 leader 的回答，不用消费者 leader 按本机流副本报的
	// 待投递数（那个副本可能落后）：流 leader 上过滤匹配、序号大于 AckFloor 的第一条是 n 时取 n-1，没有这样的消息时取
	// “先读到的流末序号”（空闲的过滤消费者不挡清理）。没回答的消费者（服务端列表的 missing）状态未知，floor 记 0。
	// 有序 / 临时消费者在回答了的时候不列出。依据与剩下的前提见驱动实现的注释。
	DurableAckFloors(ctx context.Context, stream string) (map[string]uint64, error)

	// KeyValue 建或更新一个 KV 桶并返回它的句柄。
	KeyValue(ctx context.Context, config LogKVConfig) (LogKV, error)

	// Objects 建或更新一个对象存储桶并返回它的句柄。
	Objects(ctx context.Context, config LogObjectConfig) (LogObjects, error)

	// RequestMany 在核心 NATS 上发一个请求，把每条回复交给 more，直到 more 返回 false（返回 nil）或 ctx 结束
	// （返回 ctx 错误，可 errors.Is）。一个回复都没有且服务端报告没有订阅者时返回 ErrNoResponders。用于向全部分区所有者
	// 散播查询再收集。
	RequestMany(ctx context.Context, subject string, data []byte, more func([]byte) bool) error

	// ServerVersion 是当前连接的服务端版本（如 "2.14.5"）；未连接或已关闭时为空。
	ServerVersion() string
}

// MinLogServerVersion 是 JetStreamLog 要求的最低 nats-server 版本：2.12.0 引入原子批量，但拒绝批内的 Nats-Msg-Id
// （10177 unsupported header）；2.12.1 起批内允许 MsgID，重发已落库的批返回 10201。依据：nats-server v2.12.0 / v2.12.1
// 的 server/stream.go（processJetStreamBatchMsg）与 server/jetstream_errors_generated.go。
const MinLogServerVersion = "2.12.1"

// LogBatchMaxMessages 是一次原子批量的最大条数：服务端缺省 jetstream.limits.max_batch_size = 1000（nats-server
// 2.12.x / 2.14.x 的 streamDefaultMaxBatchSize / streamDefaultMaxAtomicBatchSize，超出返回 10199）。服务端调小了这个限制时
// PublishFenced 收到 10199，同样返回 ErrLogBatchTooLarge。批没有总字节上限，单条受 max_payload（缺省 1 MiB）约束。
const LogBatchMaxMessages = 1000

var (
	// ErrLogWrongLastSequence：期望序号对不上（JetStream 10071，R>1 并发提案时为 10164），这次没有写入。
	ErrLogWrongLastSequence = errors.New("nats log: wrong last sequence")
	// ErrLogDuplicateBatch：原子批里的 MsgID 已在去重窗口内（10201）——之前某次发送已经整批落库，这次没有写入。
	ErrLogDuplicateBatch = errors.New("nats log: atomic batch contains a duplicate message id")
	// ErrLogBatchIncomplete：服务端丢弃了这个批（10176：缺号、批超时，或流 / 服务端同时打开的批数超过上限
	// max_batch_inflight_per_stream 缺省 50、max_batch_inflight_total 缺省 1000），这次没有写入，可以重发。
	ErrLogBatchIncomplete = errors.New("nats log: atomic batch incomplete")
	// ErrLogBatchTooLarge：条数超过 LogBatchMaxMessages（发送前拒绝）或服务端的 max_batch_size（10199）。调用方把事件
	// 切成几次顺序的 fenced 发布。
	ErrLogBatchTooLarge = errors.New("nats log: atomic batch too large")
	// ErrLogRejected：服务端明确拒绝了这次发布（上面几种之外的 API 错误，例如消息超过流的大小限制、流满且 Discard=New），
	// 这次没有写入。错误文本带服务端的错误码与描述。
	ErrLogRejected = errors.New("nats log: publish rejected by server")
	// ErrLogOutcomeUnknown：没收到服务端的答复，这次发布可能已经落库。按接口注释重发判定。
	ErrLogOutcomeUnknown = errors.New("nats log: publish outcome unknown")
	// ErrLogNotFound：LastMsg 没有匹配的消息、KV 键不存在（含已删除）、对象不存在。
	ErrLogNotFound = errors.New("nats log: not found")
	// ErrLogKVConflict：KV Create 时键已存在，或 Update 时修订号已变。
	ErrLogKVConflict = errors.New("nats log: key value revision conflict")
	// ErrLogUnsupportedServer：服务端版本低于 MinLogServerVersion，或流的生效配置缺少日志需要的设置。
	ErrLogUnsupportedServer = errors.New("nats log: server does not support the log stream settings")
)

// LogStreamConfig 是日志流可调的部分；存储（文件）、AllowAtomicPublish、AllowDirect、Discard=New、MaxAge=0 由
// EnsureLogStream 固定，不开放。
type LogStreamConfig struct {
	Name     string
	Subjects []string
	Replicas int
	// MaxBytes 是流的容量上限（0 不限）；满了拒新写（Discard=New），不删旧。
	MaxBytes int64
	// Duplicates 是 MsgID 去重窗口，必须长于调用方对“结果未知”的重发时长。
	Duplicates time.Duration
}

// LogStreamInfo 是 EnsureLogStream 之后服务端报告的流状态。
type LogStreamInfo struct {
	Name     string
	Subjects []string
	Replicas int
	Msgs     uint64
	Bytes    uint64
	FirstSeq uint64
	LastSeq  uint64
	// Leader 是流 leader 的 server_name（R1 或单机时也有值）。
	Leader string
}

// LogMsg 是日志里的一条消息。发布时用 Subject / Data / MsgID / Headers；读回时另有 Seq。
type LogMsg struct {
	Subject string
	Data    []byte
	// MsgID 写入 Nats-Msg-Id：服务端按它在去重窗口内去重，结果未知时的判定也靠它。PublishFenced 要求非空。
	MsgID string
	// Headers 是业务头（单值）。以 "Nats-" 开头的键由驱动与服务端保留，发布时拒绝，读回时不放进这里。
	Headers map[string]string
	// Seq 是读回时的流序号，发布时忽略。
	Seq uint64
}

// LogFence 是 PublishFenced 的期望序号：Filter 为空表示不带期望序号——只用于多写者追加、去重只靠 MsgID 的消息（saga 分区的
// 输入：启动 / 结果 / 运维意图）与测试变异；单写者的权威事件必须带。LastSeq 为 0 表示“Filter 上还没有任何消息”。
type LogFence struct {
	Filter  string
	LastSeq uint64
}

// LogPublishAck 是一次 PublishFenced 的成功结果。
type LogPublishAck struct {
	Stream string
	// Seq 是最后一条消息的流序号（单条即这条）；下一次发布的 fence.LastSeq 取它。
	Seq uint64
	// Count 是这次写入的条数。
	Count int
	// Duplicate 只对单条有意义：同一 MsgID 已在去重窗口内，Seq 是原来那条的序号。
	Duplicate bool
}

// LogFollow 是 Follow / Watch 的消费句柄。
type LogFollow interface {
	// Stop 停止消费并在 ctx 内等正在执行的回调返回；ctx 先结束返回 ctx 错误，可以再调用继续等。返回 nil 之后不会
	// 再有回调。重复调用返回 nil。
	Stop(ctx context.Context) error
	// Done 在消费结束且回调已返回后关闭。
	Done() <-chan struct{}
}

// LogKVConfig 是 KeyValue 桶的配置（文件存储）。
type LogKVConfig struct {
	Bucket   string
	Replicas int
	// History 是每个键保留的修订数（0 按 1）。
	History uint8
	// MaxAge 是值的存活时间（0 不过期）。
	MaxAge time.Duration
	// LimitMarkerTTL 非 0 时，键因 MaxAge 过期被删除时服务端写一条删除标记（保留这么久），Watch 能看到这次删除；
	// 为 0 时过期是静默的，Watch 看不到。需要 nats-server ≥ 2.11。
	LimitMarkerTTL time.Duration
	// MaxValueSize 是单个值的上限（0 按服务端 max_payload）。
	MaxValueSize int32
	MaxBytes     int64
}

// LogKVEntry 是 KV 里一个键的值。Deleted 只在 Watch 里出现：键被删除或过期清除。
type LogKVEntry struct {
	Key      string
	Value    []byte
	Revision uint64
	Deleted  bool
}

// LogKV 是一个 KV 桶的句柄。
type LogKV interface {
	// Get 读键的当前值；不存在或已删除返回 ErrLogNotFound。
	Get(ctx context.Context, key string) (LogKVEntry, error)
	// Create 只在键不存在（或已删除）时写入，返回修订号；已存在返回 ErrLogKVConflict。
	Create(ctx context.Context, key string, value []byte) (uint64, error)
	// Update 只在键的当前修订号等于 revision 时写入；不等返回 ErrLogKVConflict。
	Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error)
	// Put 无条件写入，返回修订号。
	Put(ctx context.Context, key string, value []byte) (uint64, error)
	// Delete 删除键（写删除标记）；不存在也返回 nil。
	Delete(ctx context.Context, key string) error
	// Watch 先把匹配 pattern（可带通配，空表示全部）的现有值逐个交给 onEntry，交完调用一次 onReady，之后持续交付变化
	// （含删除）。两个回调在同一个 goroutine 里顺序执行。ctx 只约束建立。
	Watch(ctx context.Context, pattern string, onEntry func(LogKVEntry), onReady func()) (LogFollow, error)
}

// LogObjectConfig 是对象存储桶的配置（文件存储）。
type LogObjectConfig struct {
	Bucket   string
	Replicas int
	MaxBytes int64
}

// LogObjectInfo 是写入对象之后的元数据。Digest 是 SHA-256（"SHA-256=<base64url>"），Get 读回时由驱动校验。
type LogObjectInfo struct {
	Name   string
	Size   uint64
	Digest string
}

// LogObjects 是一个对象存储桶的句柄。大对象由服务端分块存放，不受单条消息 max_payload 的限制。
type LogObjects interface {
	Put(ctx context.Context, name string, data []byte) (LogObjectInfo, error)
	// Get 读整个对象并校验摘要；不存在返回 ErrLogNotFound，摘要不符返回错误。
	Get(ctx context.Context, name string) ([]byte, error)
	// Delete 删除对象；不存在返回 ErrLogNotFound。
	Delete(ctx context.Context, name string) error
}
