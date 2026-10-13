package nest

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
)

// DurabilityPolicy 与回滚策略相互独立：回滚策略管提交点之前的失败，持久级别管提交点何时被确认。
//
// 它是 handler 声明用的类型，零值表示“未声明”：
// 手写 HandlerMeta 时 Rollback 不为 none 就必须显式写 Durability，注册时校验；
// Rollback 为 none 时未声明按 memory 处理，HandlerMeta{} 仍走 memory 快路径。
// 事务记录（WAL、投影、远端 outcome）上的级别是 dataengine.Durability，用 Record 换算。
type DurabilityPolicy uint8

const (
	durabilityUnset DurabilityPolicy = iota
	DurabilityMemory
	DurabilityAsync
	DurabilityStrict
	// DurabilityPipelined 拆分准入：锁内 Enqueue 是唯一拒绝点，fsync 在锁外进行，提交票据确认持久后才对外
	// 可见（回复、AfterCommit）。完整契约见 NEST_PIPELINED_COMMIT.md。
	DurabilityPipelined
)

// durabilityChoices 列出 HandlerMeta.Durability 的可选值，用于注册期报错。
const durabilityChoices = "nest.DurabilityMemory | nest.DurabilityAsync | nest.DurabilityStrict | nest.DurabilityPipelined"

// Record 换算成事务记录上的持久化级别；未声明按 memory。
func (p DurabilityPolicy) Record() dataengine.Durability {
	if p == durabilityUnset {
		return dataengine.DurabilityMemory
	}
	return dataengine.Durability(p - 1)
}

// String 返回策略名；未声明为 "unset"，越界为 "invalid"。
func (p DurabilityPolicy) String() string {
	switch {
	case p == durabilityUnset:
		return "unset"
	case p > DurabilityPipelined:
		return "invalid"
	default:
		return p.Record().String()
	}
}

// ParseDurabilityPolicy 解析 //roost:nest 与配置中的持久级别名，空串视为 memory。
func ParseDurabilityPolicy(value string) (DurabilityPolicy, error) {
	switch value {
	case "", "memory":
		return DurabilityMemory, nil
	case "async":
		return DurabilityAsync, nil
	case "strict":
		return DurabilityStrict, nil
	case "pipelined":
		return DurabilityPipelined, nil
	default:
		return DurabilityMemory, fmt.Errorf("nest: unsupported durability policy %q", value)
	}
}

// TransactionID 由进程随机前缀与本地序号组成，可排序；热路径上不必每笔事务都调用 crypto/rand。
type TransactionID = dataengine.TransactionID

var transactionIDState struct {
	prefix [8]byte
	seq    atomic.Uint64
}

func init() {
	if _, err := rand.Read(transactionIDState.prefix[:]); err != nil {
		binary.BigEndian.PutUint64(transactionIDState.prefix[:], uint64(time.Now().UnixNano()))
	}
}

// newTransactionID 生成本进程内唯一的事务 ID：随机前缀 + 单调递增序号。
func newTransactionID() TransactionID {
	var id TransactionID
	copy(id[:8], transactionIDState.prefix[:])
	binary.BigEndian.PutUint64(id[8:], transactionIDState.seq.Add(1))
	return id
}

// Nest 与生成代码使用的持久模型别名；类型由 dataengine 包定义并拥有。
type EntityMutation = dataengine.Mutation
type Effect = dataengine.Effect
type CommitRecord = dataengine.CommitRecord

// CommitFence 标识一条 WAL 记录（段号 + 帧结束偏移），用于确认。
type CommitFence struct {
	TransactionID TransactionID
	Segment       uint64
	Offset        int64
}

// CommitWAL 由基础设施包实现。Append 是提交点；Replay 必须按原追加顺序返回未确认的记录。
type CommitWAL interface {
	Append(ctx context.Context, record CommitRecord) (CommitFence, error)
	Ack(ctx context.Context, fence CommitFence) error
	Replay(ctx context.Context, consume func(CommitFence, CommitRecord) error) error
}

// TransactionCommitter 执行持久准入并安排回放 / outbox 投递。持久接受记录之后绝不能再报告失败；
// 提交后的投递失败在带外重试。
type TransactionCommitter interface {
	Commit(ctx context.Context, record CommitRecord) error
}

// TransactionReleaseNotifier 是 committer 的可选能力。Nest 在全部实体锁释放后调用它，让回放 worker 不与提交进程
// 正常的 checkpoint 路径竞争。
type TransactionReleaseNotifier interface {
	TransactionReleased(TransactionID)
}

// LocalExecutorBinder 是 committer 的可选能力。NewEngine 把引擎的 RunLocal 交给它，让持久化基础设施在业务池上执行
// 需要实体本地锁的步骤（例如驱逐投影时被跳过的 lease-fenced 事务留下的实体），而不是在自己的 goroutine 上取锁。
// Remote Entity Manager 与 Getter（entity.ManagerAccess：发布领头请求已离开的共享冷加载）以同样方式绑定。
type LocalExecutorBinder interface {
	BindLocalExecutor(run func(func()) error)
}

// CommitTicket 在已入队记录持久时完成。Err 成功时为 nil，fsync 结果未知时为 ErrCommitIndeterminate，不允许其他错误：
// 一切可拒绝的情形都必须由 Enqueue 在调用方仍持实体锁、仍能回滚时同步报告。
type CommitTicket interface {
	LSN() uint64
	Done() <-chan struct{}
	Err() error
}

// PipelinedTransactionCommitter 是支撑 DurabilityPipelined 的 committer 可选能力。Enqueue 在持实体锁时调用：必须同步
// 完成全部可拒绝的校验与缓冲准入（背压时拒绝而不是等待——调用方持锁）、分配 LSN 后返回，由组提交 worker 完成票据。
// DurableLSN 是单调水位：LSN 不大于它的记录都已持久（前缀持久）。见 NEST_PIPELINED_COMMIT.md。
type PipelinedTransactionCommitter interface {
	TransactionCommitter
	Enqueue(ctx context.Context, record CommitRecord) (CommitTicket, error)
	DurableLSN() uint64
}

// CommitParticipant 在 handler 成功结束时恰好一次地生成最终 after-image / delta。实现在准备记录时不得清除脏标记
// 或 patch 元数据。
type CommitParticipant interface {
	PrepareCommit(tx *RollbackTx) error
}

// validateCommitRecord 用 DataEngine 的规则校验提交记录。
func validateCommitRecord(record CommitRecord) error {
	return dataengine.ValidateCommitRecord(record)
}
