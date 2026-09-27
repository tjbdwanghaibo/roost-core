package remoteentity

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/nest"
)

// OPEN-ITEMS B23：RR-20260926-77 判别表第 2 行按 RR-37 记录写入、没有核对过真实回复链。这里在真实 Nest + 正式 Remote Manager
// （MongoCommitter 权威、ManagerAccess loader，RR-39 的 newReloadFixture）上跑 RR-37 的四种“本地已提交、Remote 结果未知”，
// 打印回复链上的哨兵组合，并断言按判别表“从上到下第一个命中的行”恰是第 2 行（可能已提交、不得重试），
// 且不带第 1、3、4 行的哨兵。
//
// 核对结果（2026-09-27）：Durability 0 回复丢失 / 从未到达、strict 等待中投影器报告未知，回复带 ErrRemotePersistenceIndeterminate；
// strict 等待 Remote 确认到截止时回复是 ErrRemoteCommitTimeout + context.DeadlineExceeded，**不带** ErrRemotePersistenceIndeterminate
// （transaction_tracking.go waitRemoteTransaction 的截止分支）。判别表第 2 行原先只列前者，已补上后者。
//
// RR-20260927-24：RR-37 承诺“Remote 结果未知 → ErrRemotePersistenceIndeterminate”，按这个哨兵判“可能已提交、不得重试”的调用方
// 在 strict 截止场景漏判。截止分支改为同时带上它（ErrRemoteCommitTimeout 与 ctx 错误保留），这里断言四种场景都满足它，
// strict 截止另外断言原有的两个哨兵仍在（只增加 errors.Is 命中，不删除）。
func TestRemoteUnknownOutcomeRepliesHitDecisionTableRow2(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rawID      int64
		durability nest.DurabilityPolicy
		setup      func(f reloadFixture)
		// strict：Remote 确认等待的上限；beforeWait 在等待开始前调用（模拟 WAL 投影器）。
		confirm    time.Duration
		beforeWait func(f reloadFixture, commits []entity.RemoteCommit)
		// also：除 ErrRemotePersistenceIndeterminate 外必须仍然满足的哨兵。
		also []error
	}{
		{name: "memory_lost_reply", rawID: 1991, durability: nest.DurabilityMemory,
			setup: func(f reloadFixture) { f.storage.loseReply.Store(true) }},
		{name: "memory_never_reached", rawID: 1992, durability: nest.DurabilityMemory,
			setup: func(f reloadFixture) { f.storage.unreachable.Store(true) }},
		{name: "strict_confirm_deadline", rawID: 1993, durability: nest.DurabilityStrict,
			confirm: 100 * time.Millisecond, also: []error{entity.ErrRemoteCommitTimeout, context.DeadlineExceeded}},
		{name: "strict_projector_reports_unknown", rawID: 1994, durability: nest.DurabilityStrict,
			confirm: 3 * time.Second, beforeWait: func(f reloadFixture, commits []entity.RemoteCommit) {
				// 投影器的发布没拿到 Remote 回复：tracker Indeterminate，strict 等待随之结束。
				f.storage.unreachable.Store(true)
				go func() { _, _ = f.mgr.ApplyRemoteCommits(context.Background(), commits[0].TransactionID, commits) }()
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, live := newReloadFixture(t, tc.rawID)
			if tc.setup != nil {
				tc.setup(f)
			}
			var manager entity.IRemoteEntityManager = f.mgr
			options := []nest.NestOption{nest.NestOptionWithGetter(f.access), nest.NestOptionWithWorkerNumAndMsgCap(1, 1, 16)}
			rollback := nest.RollbackNone
			if tc.durability != nest.DurabilityMemory {
				manager = boundedConfirmManager{Manager: f.mgr, fixture: f, confirm: tc.confirm, beforeWait: tc.beforeWait}
				options = append(options, nest.NestOptionWithTransactionCommitter(localDurableCommitter{}))
				// 持久 handler 必须可回滚；undo 模式不要求实体快照，这里也不登记 undo 动作。
				rollback = nest.RollbackUndo
			}
			options = append(options, nest.NestOptionWithRemoteEntityManager(manager))
			engine := nest.NewEngine(options...)
			name := nest.NewHandlerName("reply_sentinels_" + tc.name)
			engine.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...nest.HandlerOption) (any, error) {
				live.set("unknown", "")
				return "ok", nil
			}, nest.HandlerMeta{Rollback: rollback, Durability: tc.durability})
			if err := engine.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			reply, err := engine.Request(ctx, name, live.ID(), nil)
			cancel()
			row, hits := firstTableRow(err)
			t.Logf("reply=%v first-row=%d sentinels=[%s]", reply, row, hits)
			for _, want := range append([]error{entity.ErrRemotePersistenceIndeterminate}, tc.also...) {
				if !errors.Is(err, want) {
					t.Fatalf("reply err=%v (sentinels %s), want errors.Is(%v)", err, hits, want)
				}
			}
			if errors.Is(err, nest.ErrNestCanceled) || errors.Is(err, nest.ErrNestTimeout) {
				t.Fatalf("premise: the caller's own wait ended before the reply: %v", err)
			}
			if row != 2 {
				t.Fatalf("reply err=%v hits decision-table row %d (sentinels %s), want row 2", err, row, hits)
			}
		})
	}
}

// RR-20260927-32：Remote 批次在 FinalizeLocked（WAL 准入之前）被明确拒绝——这里用 RR-20260927-09 的 sid 作用域拒绝——时，回复之前原样
// 返回 ErrRemoteManagedServerScopedDAO，判别表一行都不命中。在真实 Nest + 正式 Remote Manager 上断言回复现在按“第一个命中的行”
// 落在第 11 行（ErrCommitRejected：未提交、已回滚），原因仍可 errors.Is，本地 committer 没收到记录。
func TestRemoteFinalizeRejectionReplyHitsDecisionTableRow11(t *testing.T) {
	f, live := newReloadFixture(t, 1996)
	live.mu.Lock()
	live.scope = uint8(entity.DatabaseServer)
	live.mu.Unlock()
	committer := &countingLocalCommitter{}
	engine := nest.NewEngine(nest.NestOptionWithGetter(f.access), nest.NestOptionWithWorkerNumAndMsgCap(1, 1, 16),
		nest.NestOptionWithTransactionCommitter(committer), nest.NestOptionWithRemoteEntityManager(f.mgr))
	name := nest.NewHandlerName("reply_sentinels_finalize_rejected")
	engine.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...nest.HandlerOption) (any, error) {
		live.set("sid", "")
		return "ok", nil
	}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityStrict})
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	reply, err := engine.Request(ctx, name, live.ID(), nil)
	cancel()
	row, hits := firstTableRow(err)
	t.Logf("reply=%v first-row=%d sentinels=[%s]", reply, row, hits)
	if !errors.Is(err, entity.ErrRemoteManagedServerScopedDAO) {
		t.Fatalf("reply err=%v, want errors.Is ErrRemoteManagedServerScopedDAO (fixture: FinalizeLocked must refuse dbscope=sid)", err)
	}
	if row != 11 {
		t.Fatalf("reply err=%v hits decision-table row %d (sentinels %s), want row 11 (ErrCommitRejected)", err, row, hits)
	}
	if n := committer.calls.Load(); n != 0 {
		t.Fatalf("local committer received %d record(s) although FinalizeLocked refused the batch", n)
	}
}

// countingLocalCommitter 与 localDurableCommitter 相同（立即持久），另计 Commit 次数。
type countingLocalCommitter struct{ calls atomic.Int32 }

func (c *countingLocalCommitter) Commit(context.Context, coredata.CommitRecord) error {
	c.calls.Add(1)
	return nil
}

// replySentinels 是 USER_GUIDE §4“回复错误判别”表按行的哨兵（第 7 行是第 6 行加 ErrLockTimeout 的组合，这里按第 6、8 行分别列出）。
var replySentinels = []struct {
	row  int
	name string
	err  error
}{
	{1, "nest.ErrCommitIndeterminate", nest.ErrCommitIndeterminate},
	{2, "entity.ErrRemotePersistenceIndeterminate", entity.ErrRemotePersistenceIndeterminate},
	{2, "entity.ErrRemoteCommitTimeout", entity.ErrRemoteCommitTimeout},
	{3, "nest.ErrAfterCommitFailed", nest.ErrAfterCommitFailed},
	{4, "nest.ErrNestedTransactionCommitted", nest.ErrNestedTransactionCommitted},
	{5, "nest.ErrNonRollbackNotRequeued", nest.ErrNonRollbackNotRequeued},
	{6, "nest.ErrCreatedEntityLockConflict", nest.ErrCreatedEntityLockConflict},
	{8, "nest.ErrLockTimeout", nest.ErrLockTimeout},
	{8, "nest.ErrEntityLockGroupChanged", nest.ErrEntityLockGroupChanged},
	{8, "nest.ErrEntityGroupTransitionPending", nest.ErrEntityGroupTransitionPending},
	{9, "nest.ErrNestedTransactionRollbackConflict", nest.ErrNestedTransactionRollbackConflict},
	{10, "nest.ErrNestedTransactionInRemoteMessage", nest.ErrNestedTransactionInRemoteMessage},
	{11, "nest.ErrCommitRejected", nest.ErrCommitRejected},
	{0, "nest.ErrNestCanceled", nest.ErrNestCanceled},
	{0, "nest.ErrNestTimeout", nest.ErrNestTimeout},
	{0, "context.DeadlineExceeded", context.DeadlineExceeded},
}

// firstTableRow 返回判别表“从上到下第一个命中的行”（0 = 没有命中任何行），以及命中的全部哨兵名。
func firstTableRow(err error) (int, string) {
	first := 0
	var hits []string
	for _, s := range replySentinels {
		if !errors.Is(err, s.err) {
			continue
		}
		hits = append(hits, s.name)
		if s.row != 0 && (first == 0 || s.row < first) {
			first = s.row
		}
	}
	return first, strings.Join(hits, " + ")
}

// localDurableCommitter 是 strict 的本地提交器替身：本地事务立即持久（Commit 返回 nil）；这里没有 WAL 投影器，
// Remote 确认只能由用例的 beforeWait 给出，或等到 strict 等待截止。
type localDurableCommitter struct{}

func (localDurableCommitter) Commit(context.Context, coredata.CommitRecord) error { return nil }

// boundedConfirmManager 只把 strict 的 Remote 确认等待截短到 confirm（其余全部是正式 Manager），让“确认截止”先于调用方截止发生，
// 回复才能送达调用方；生产里二者常是同一个请求截止，调用方先看到自己的 ErrNestCanceled。
type boundedConfirmManager struct {
	*Manager
	fixture    reloadFixture
	confirm    time.Duration
	beforeWait func(reloadFixture, []entity.RemoteCommit)
}

func (m boundedConfirmManager) PrepareRemoteWriteBatch(ctx context.Context, ids []int64) (entity.RemoteWriteBatch, error) {
	batch, err := m.Manager.PrepareRemoteWriteBatch(ctx, ids)
	if err != nil {
		return nil, err
	}
	return boundedConfirmBatch{RemoteWriteBatch: batch, manager: m}, nil
}

type boundedConfirmBatch struct {
	entity.RemoteWriteBatch
	manager boundedConfirmManager
}

func (b boundedConfirmBatch) Commit(ctx context.Context) ([]entity.RemoteCommitReceipt, error) {
	if b.manager.beforeWait != nil {
		b.manager.beforeWait(b.manager.fixture, b.Commits())
	}
	bounded, cancel := context.WithTimeout(ctx, b.manager.confirm)
	defer cancel()
	return b.RemoteWriteBatch.Commit(bounded)
}

func (b boundedConfirmBatch) DeferUntilDurableOutcome(fn func(bool)) bool {
	deferrer, ok := b.RemoteWriteBatch.(entity.RemoteOutcomeDeferrer)
	return ok && deferrer.DeferUntilDurableOutcome(fn)
}
