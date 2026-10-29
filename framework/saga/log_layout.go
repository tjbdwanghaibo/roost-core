package saga

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
)

// log_layout.go 是 JetStream 协调器的分区布局：saga ID 怎么来、落在哪个分区、分区在哪条流、各类消息的 subject 与 KV / 对象桶名
// （设计 §4.1）。布局只由 Layout 的四个字段决定，全部进程必须一致：第一个启动的所有者把它登记进快照桶的 layout 键，
// 之后的进程比对，不一致拒绝启动（ErrLayoutMismatch）。改分区数 / 流数只能停全部协调器、删流与桶后重建，不支持在线改。

// maxPartitionsPerStream 是一条流上的分区数上限：每个分区同时最多一次原子批量在途，服务端缺省一条流同时打开的原子批
// 不超过 50（jetstream.limits.max_batch_inflight_per_stream，超出的批被丢弃、返回 10176）。所以
// ceil(Partitions / Groups) ≤ 50，否则高负载下同流的批会互相挤掉（确定没写入、重发可恢复，但会空转）。第 1 批核实，
// 见设计 §16。
const maxPartitionsPerStream = 50

// Layout 是 JetStream 协调器的分区布局。
type Layout struct {
	// Prefix 是全部 subject 的前缀（如 roost.saga），可以带点。
	Prefix string
	// Stream 是流名前缀（如 ROOST_SAGA）；第 g 条流名为 <Stream>_G<g>，KV / 对象桶名也由它派生。
	Stream string
	// Partitions 是分区数 P（1～1024）。
	Partitions int
	// Groups 是流数 G（1～8，≤ P）：分区 k 在第 k mod G 条流上；一条流选主时只停它上面的分区。
	Groups int
}

// Validate 校验布局：前缀是合法 subject、流名只含 [A-Za-z0-9_-]、P / G 的范围，以及每条流的分区数不超过同时在途的
// 原子批上限（maxPartitionsPerStream）。
func (l Layout) Validate() error {
	if !validSubject(l.Prefix) || strings.Contains(l.Prefix, ".q.") || strings.HasSuffix(l.Prefix, ".q") {
		return fmt.Errorf("%w: layout prefix %q", ErrInvalidDefinition, l.Prefix)
	}
	if !validStreamName(l.Stream) || len(l.Stream) > 200 {
		return fmt.Errorf("%w: layout stream %q must use only letters, digits, '_' and '-'", ErrInvalidDefinition, l.Stream)
	}
	if l.Partitions < 1 || l.Partitions > 1024 {
		return fmt.Errorf("%w: layout partitions %d must be in [1, 1024]", ErrInvalidDefinition, l.Partitions)
	}
	if l.Groups < 1 || l.Groups > 8 || l.Groups > l.Partitions {
		return fmt.Errorf("%w: layout stream groups %d must be in [1, 8] and <= partitions %d", ErrInvalidDefinition, l.Groups, l.Partitions)
	}
	if perStream := (l.Partitions + l.Groups - 1) / l.Groups; perStream > maxPartitionsPerStream {
		return fmt.Errorf("%w: layout puts %d partitions on one stream (ceil(%d/%d)); at most %d atomic batches may be open per stream, so it must be <= %d",
			ErrInvalidDefinition, perStream, l.Partitions, l.Groups, maxPartitionsPerStream, maxPartitionsPerStream)
	}
	return nil
}

func validStreamName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// IDOf 是一笔 saga 的 ID：由 (Type, BusinessKey) 决定（选项 ID-A），所以按业务键查就是按 ID 查，唯一性与推进在同一个
// 分区单写者里判定。128 位十六进制，满足 subject token 约束。
func IDOf(sagaType, businessKey string) string {
	sum := sha256.Sum256([]byte(sagaType + "\x00" + businessKey))
	return hex.EncodeToString(sum[:16])
}

// PartitionOf 是 saga ID 所在的分区：FNV-1a(id) mod P（与原型相同）。
func (l Layout) PartitionOf(id string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return int(h.Sum32() % uint32(l.Partitions))
}

// GroupOf 是分区 k 所在的流号。
func (l Layout) GroupOf(k int) int { return k % l.Groups }

// StreamOf 是分区 k 所在的流名。
func (l Layout) StreamOf(k int) string { return l.streamName(l.GroupOf(k)) }

func (l Layout) streamName(g int) string { return l.Stream + "_G" + strconv.Itoa(g) }

// Streams 是全部流名（G 条）。
func (l Layout) Streams() []string {
	names := make([]string, l.Groups)
	for g := range names {
		names[g] = l.streamName(g)
	}
	return names
}

// PartitionsOfGroup 是第 g 条流上的分区。
func (l Layout) PartitionsOfGroup(g int) []int {
	var parts []int
	for k := g; k < l.Partitions; k += l.Groups {
		parts = append(parts, k)
	}
	return parts
}

// StreamSubjects 是第 g 条流捕获的 subject：本流各分区的 <prefix>.<k>.>（G=1 也逐个列出，查询 subject <prefix>.q.* 不能被流捕获）。
func (l Layout) StreamSubjects(g int) []string {
	parts := l.PartitionsOfGroup(g)
	subjects := make([]string, len(parts))
	for i, k := range parts {
		subjects[i] = l.PartitionFilter(k)
	}
	return subjects
}

func (l Layout) partitionPrefix(k int) string { return l.Prefix + "." + strconv.Itoa(k) }

// PartitionFilter 是分区 k 的全部消息（事件与输入）：重放与清理用。
func (l Layout) PartitionFilter(k int) string { return l.partitionPrefix(k) + ".>" }

// 事件 subject 的去向：带命令的事件按步骤主题路由（参与方按主题过滤），终态单独一个（WaitTerminal、冷副本），其余变化一个。
const (
	eventDestCommand = "cmd"
	eventDestDone    = "done"
	eventDestNone    = "none"
)

// CommandEventSubject 是分区 k 派发主题 topic 的命令事件。
func (l Layout) CommandEventSubject(k int, topic string) string {
	return l.partitionPrefix(k) + ".ev." + eventDestCommand + "." + topic
}

// DoneEventSubject / NoneEventSubject 是分区 k 的终态事件与无命令变化事件。
func (l Layout) DoneEventSubject(k int) string { return l.partitionPrefix(k) + ".ev." + eventDestDone }
func (l Layout) NoneEventSubject(k int) string { return l.partitionPrefix(k) + ".ev." + eventDestNone }

// EventFilter 是分区 k 的全部事件：期望序号的过滤（单写者隔离）就是它。
func (l Layout) EventFilter(k int) string { return l.partitionPrefix(k) + ".ev.>" }

// InputFilter 是分区 k 的全部输入（启动、结果、运维）。
func (l Layout) InputFilter(k int) string { return l.partitionPrefix(k) + ".in.>" }

// StartSubject / ResultSubject / OpSubject 是分区 k 的三类输入。
func (l Layout) StartSubject(k int) string  { return l.partitionPrefix(k) + ".in.start" }
func (l Layout) ResultSubject(k int) string { return l.partitionPrefix(k) + ".in.res" }
func (l Layout) OpSubject(k int) string     { return l.partitionPrefix(k) + ".in.op" }

// CommandFilter 是全部分区派发主题 topic 的命令事件（参与方的 durable 用）。
func (l Layout) CommandFilter(topic string) string {
	return l.Prefix + ".*.ev." + eventDestCommand + "." + topic
}

// DoneFilter 是全部分区的终态事件。
func (l Layout) DoneFilter() string { return l.Prefix + ".*.ev." + eventDestDone }

// QuerySubject 是向分区 k 的所有者问记录的核心 NATS subject（不进流）；ListSubject 是向全部所有者散播的列表查询。
func (l Layout) QuerySubject(k int) string { return l.Prefix + ".q." + strconv.Itoa(k) }
func (l Layout) ListSubject() string       { return l.Prefix + ".q.list" }

// SnapshotBucket 是快照清单的 KV 桶（键 p<k> 是分区 k 的清单，键 layout 是布局登记）；ObjectBucket 是快照正文的对象桶；
// LoadBucket 是准入信号（part.<参与方>）与准入提示（admit）的 KV 桶。
func (l Layout) SnapshotBucket() string { return l.Stream + "_SNAP" }
func (l Layout) ObjectBucket() string   { return l.Stream + "_SNAPOBJ" }
func (l Layout) LoadBucket() string     { return l.Stream + "_LOAD" }

// snapshotKey 是分区 k 的快照清单键。
func snapshotKey(k int) string { return "p" + strconv.Itoa(k) }

// layoutKey 是快照桶里登记布局的键。
const layoutKey = "layout"

// isEventSubject / inputKindOf 按 subject 区分分区里的消息。
func (l Layout) isEventSubject(k int, subject string) bool {
	return strings.HasPrefix(subject, l.partitionPrefix(k)+".ev.")
}

func (l Layout) inputKindOf(k int, subject string) string {
	rest, ok := strings.CutPrefix(subject, l.partitionPrefix(k)+".in.")
	if !ok {
		return ""
	}
	return rest
}
