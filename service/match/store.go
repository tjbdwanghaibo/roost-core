package match

import (
	"context"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

// Store is matchmaking persistence.
//
// The shape that matters: Commit is one call that takes tickets off the queue,
// creates the match and resolves those tickets **atomically**. There is no way
// to pop the queue without committing, and no way to write a match without
// consuming its tickets. In the implementation this replaces those were three
// separate writes with the queue already truncated, so a failure between them
// lost the players — removed from the queue, still marked waiting, with
// nothing to put them back.
type Store interface {
	// Enqueue adds a waiting ticket. It refuses a subject that already holds
	// a live ticket (ErrAlreadyQueued), and is idempotent per requestID: a
	// repeat returns the existing ticket rather than queueing twice.
	//
	// 请求记录与它的票同寿：票结束满 Config.Retention 被移除后，同一 requestID 的重试是一次新的排队。
	Enqueue(ctx context.Context, queue Queue, subject Subject, requestID string) (Ticket, error)

	// Cancel withdraws a waiting ticket. subject must own it.
	Cancel(ctx context.Context, queue Queue, ticketID string, subject Subject) (Ticket, error)

	// Ticket reads one ticket. subject must own it, so a guessable id does not
	// expose another subject's score or payload. 已结束的票保留 Config.Retention，
	// 移除后读到 found=false。
	Ticket(ctx context.Context, queue Queue, ticketID string, subject Subject) (Ticket, bool, error)

	// Candidates returns up to limit waiting, unexpired tickets in queue
	// order, oldest first. It is bounded: the implementation this replaces
	// read every queued ticket on every enqueue.
	Candidates(ctx context.Context, queue Queue, limit int) ([]Ticket, error)

	// Commit 原子地把票集合变成一场比赛：检查与写入是同一次比较并设置，失败不留副作用。
	// 票集合就是提交身份：完整的同一票集合已匹配到同一局时幂等返回原 Match（票序可不同），
	// 不创建第二局；与已匹配票只部分重叠，或含已取消 / 过期 / 不存在的票时拒绝（ErrConflict /
	// ErrTicketMissing）。并发调用可以都成功回复，但只指向同一场比赛，不能按成功回复次数计开局数。
	//
	// 重放只在保留期内返回原结果（Config.Retention，缺省 DefaultRetention，维护者 2026-10-19 方案 A）：
	// 比赛与它的票从成局起至少保留 Retention，之后在下一次写入或 Sweep 时一起移除；超期后同一票集合的
	// Commit 报 ErrTicketMissing——仍不会成第二局（被移除的票 ID 不会回到等待状态），只是拿不回原 Match。
	// 匹配器要在保留期内完成自己的重试。见 TestCommitReplayReturnsOriginalMatchForSameTicketSet、
	// TestCommitReplayInsideRetentionReturnsTheOriginalMatch、TestCommitReplayAfterRetentionReportsTheTicketsMissing、
	// TestConcurrentCommitsProduceExactlyOneMatch、TestFailedCommitLeavesTheQueueUntouched。
	Commit(ctx context.Context, queue Queue, ticketIDs []string) (Match, error)

	// CommitAcross 是跨分区的 Commit：tickets 来自同一逻辑队列（Mode + GroupSize 相同、只差 Partition）的
	// 若干分区，张数等于 GroupSize。它在一次多键比较并设置里读全部涉及的分区，校验每张票仍在自己的分区里等待、
	// 主体不重复，然后在每个涉及的分区写入同一场比赛、把票标为已匹配——要么所有分区同时生效，要么一个分区也
	// 不改（某张票已被取消 / 过期 / 被别的 Commit 拿走时报 ErrConflict 或 ErrTicketMissing）。
	//
	// 语义与 Commit 一致：票集合就是提交身份，保留期内同一票集合（票序可不同）的重放返回原 Match，超期报
	// ErrTicketMissing，不会成第二局。全部票在同一个分区时等同于 Commit。
	//
	// 比赛的存放：每个涉及的分区各存一份完整的 Match（Match.TicketPartitions 记每张票的分区），所以用其中
	// 任何一个分区调 Match、或用票自己的分区调 Ticket 再按 MatchID 调 Match，都能读到它。每个分区按自己的
	// 保留期规则把这份比赛与本分区的票一起移除（pruneResolvedLocked）；各分区移除的时点都是“成局时刻 +
	// Retention”之后的第一次写入或 Sweep，所以保留期内所有分区都还在，超期后不保证各分区同时消失。
	//
	// 路由与代价：键名的 hash tag 是逻辑队列（Queue.Key），同一逻辑队列的分区在 Redis Cluster 的同一个 slot，
	// 多键脚本才能执行；一次 CommitAcross 读写的是涉及的几个分区的整个状态，并与这些分区上的单分区操作竞争。
	// 跨分区成局应是放宽匹配的补充路径，常规成局仍走单分区 Commit。
	CommitAcross(ctx context.Context, tickets TicketRefs) (Match, error)

	// Match reads a committed match. 比赛从成局起保留 Config.Retention，移除后读到 found=false。
	Match(ctx context.Context, queue Queue, matchID string) (Match, bool, error)

	// Sweep resolves tickets whose deadline has passed, up to limit, and
	// returns how many it resolved. Nothing in the implementation this
	// replaces ever read the deadline it wrote.
	//
	// 同一次比较并设置里还移除结束已满 Config.Retention 的票、比赛与请求记录（不受 limit 约束，
	// 不计入返回值），让没人再写的队列状态也回落；任何写入也会顺带做同样的移除。
	Sweep(ctx context.Context, queue Queue, limit int) (int, error)

	// QueueLength reports how many waiting tickets a queue holds.
	QueueLength(ctx context.Context, queue Queue) (int, error)
}

func validateEnqueue(queue Queue, subject Subject) error {
	if err := queue.Validate(); err != nil {
		return err
	}
	return subject.Validate()
}

func validateOwnership(ticket Ticket, subject Subject) error {
	if ticket.Subject.Key() != subject.Key() {
		// Deliberately does not say who does own it: the caller has already
		// shown it does not, and naming the owner would turn a guessable id
		// into an enumeration oracle.
		return fmt.Errorf("%w: ticket %s", ErrNotPermitted, ticket.ID)
	}
	return nil
}

func validateLimit(limit int) error {
	if limit <= 0 {
		return fmt.Errorf("%w: limit must be positive, got %d", ErrQueueInvalid, limit)
	}
	if limit > MaxPageSize {
		return fmt.Errorf("%w: limit %d exceeds %d", ErrQueueInvalid, limit, MaxPageSize)
	}
	return nil
}

// NewMemoryStore is NewStore over an in-process versionstore: what a test or a
// single-process tool uses when there is no Redis to share the queue with.
// The queue state type is this package's own, so callers outside it cannot
// name the versionstore themselves; this is the constructor they use instead.
func NewMemoryStore(cfg Config) (Store, error) {
	return NewStore(versionstore.NewMemoryStore[string, queueState](), cfg)
}
