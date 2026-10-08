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
// 且不带第 1 行的哨兵（第 3～5 行排在它之后，命中第 2 行即可）。
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
			options := []nest.NestOption{nest.NestOptionWithGetter(f.access), nest.NestOptionWithWorkerNumAndMsgCap(1, 16)}
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
			if errors.Is(err, nest.ErrRemotePartRejected) {
				// RR-20260928-03：结果未知不是明确拒绝，两者互斥。
				t.Fatalf("reply err=%v: an unknown Remote outcome must not claim the Remote part was rejected", err)
			}
		})
	}
}

// RR-20260927-32：Remote 批次在 FinalizeLocked（WAL 准入之前）被明确拒绝——这里用 RR-20260927-09 的 sid 作用域拒绝——时，回复之前原样
// 返回 ErrRemoteManagedServerScopedDAO，判别表一行都不命中。在真实 Nest + 正式 Remote Manager 上断言回复现在按“第一个命中的行”
// 落在 ErrCommitRejected 那一行（未提交、已回滚），原因仍可 errors.Is，本地 committer 没收到记录。
// RR-20260928-03 在判别表第 4 行插入 ErrRemotePartRejected，这一行由第 11 行顺延为第 12 行（原用例名 …Row11）。
func TestRemoteFinalizeRejectionReplyHitsDecisionTableRow12(t *testing.T) {
	f, live := newReloadFixture(t, 1996)
	live.mu.Lock()
	live.scope = uint8(entity.DatabaseServer)
	live.mu.Unlock()
	committer := &countingLocalCommitter{}
	engine := nest.NewEngine(nest.NestOptionWithGetter(f.access), nest.NestOptionWithWorkerNumAndMsgCap(1, 16),
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
	if row != 12 {
		t.Fatalf("reply err=%v hits decision-table row %d (sentinels %s), want row 12 (ErrCommitRejected)", err, row, hits)
	}
	if n := committer.calls.Load(); n != 0 {
		t.Fatalf("local committer received %d record(s) although FinalizeLocked refused the batch", n)
	}
}

// RR-20260928-03：带 Remote 批次的消息，本地事务已提交而 Remote 部分被明确拒绝（RR-20260926-58：只丢弃被拒绝的 Remote 部分，
// 本地部分照常生效）。之前回复是 “nest: finish remote write batch: …write rejected before commit…”，判别表一行都不命中，
// 调用方看不出本地已提交，可能整笔重试、重复执行本地部分。承诺：回复满足 errors.Is(nest.ErrRemotePartRejected)，按“第一个命中的行”
// 落在第 4 行（部分已提交、不得整笔重试），原因（entity.ErrRemoteRejected / ErrRemoteVersionConflict）仍可 errors.Is；不带第 2 行
// （结果未知）与第 3 行（Remote 也已确认）的哨兵；本地 committer 确实收到了这笔记录。
// strict：WAL 投影器（这里由 beforeWait 模拟）把 Remote 写交给权威、被版本冲突拒绝，strict 等待得到 Rejected 结论（探针原形）；
// memory：Durability 0 由 Commit 直接写权威、被版本冲突拒绝。
func TestRemoteRejectedAfterLocalCommitReplyHitsDecisionTableRow4(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rawID      int64
		durability nest.DurabilityPolicy
		rollback   nest.RollbackPolicy
		beforeWait func(f reloadFixture, commits []entity.RemoteCommit)
		cause      error
	}{
		{name: "strict_projector_rejected", rawID: 1997, durability: nest.DurabilityStrict, rollback: nest.RollbackUndo,
			beforeWait: func(f reloadFixture, commits []entity.RemoteCommit) {
				f.storage.reject.Store(true)
				go func() { _, _ = f.mgr.ApplyRemoteCommits(context.Background(), commits[0].TransactionID, commits) }()
			}, cause: entity.ErrRemoteRejected},
		{name: "memory_rejected", rawID: 1998, durability: nest.DurabilityMemory, rollback: nest.RollbackNone,
			beforeWait: func(f reloadFixture, _ []entity.RemoteCommit) { f.storage.reject.Store(true) },
			cause:      entity.ErrRemoteVersionConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, live := newReloadFixture(t, tc.rawID)
			committer := &countingLocalCommitter{}
			manager := boundedConfirmManager{Manager: f.mgr, fixture: f, confirm: 3 * time.Second, beforeWait: tc.beforeWait}
			options := []nest.NestOption{nest.NestOptionWithGetter(f.access), nest.NestOptionWithWorkerNumAndMsgCap(1, 16),
				nest.NestOptionWithRemoteEntityManager(manager)}
			if tc.durability != nest.DurabilityMemory {
				options = append(options, nest.NestOptionWithTransactionCommitter(committer))
			}
			engine := nest.NewEngine(options...)
			name := nest.NewHandlerName("reply_sentinels_remote_part_rejected_" + tc.name)
			engine.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...nest.HandlerOption) (any, error) {
				live.set("rejected", "")
				return "ok", nil
			}, nest.HandlerMeta{Rollback: tc.rollback, Durability: tc.durability})
			if err := engine.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			reply, err := engine.Request(ctx, name, live.ID(), nil)
			cancel()
			row, hits := firstTableRow(err)
			t.Logf("reply=%v err=%v first-row=%d sentinels=[%s]", reply, err, row, hits)
			if !errors.Is(err, tc.cause) {
				t.Fatalf("fixture: reply err=%v, want the Remote rejection errors.Is(%v) on the chain", err, tc.cause)
			}
			if errors.Is(err, nest.ErrNestCanceled) || errors.Is(err, nest.ErrNestTimeout) {
				t.Fatalf("premise: the caller's own wait ended before the reply: %v", err)
			}
			if tc.durability != nest.DurabilityMemory {
				if n := committer.calls.Load(); n != 1 {
					t.Fatalf("premise: local committer received %d record(s), want 1 (the local transaction commits first)", n)
				}
			}
			if !errors.Is(err, nest.ErrRemotePartRejected) {
				t.Fatalf("reply err=%v (sentinels [%s]) after the local transaction committed and the Remote part was rejected; want errors.Is nest.ErrRemotePartRejected", err, hits)
			}
			if errors.Is(err, entity.ErrRemotePersistenceIndeterminate) || errors.Is(err, nest.ErrAfterCommitFailed) {
				t.Fatalf("reply err=%v: an explicit Remote rejection must not also claim an unknown outcome or a confirmed Remote commit", err)
			}
			if row != 4 {
				t.Fatalf("reply err=%v hits decision-table row %d (sentinels [%s]), want row 4", err, row, hits)
			}
		})
	}
}

// RR-20260928-08（第七轮审计 N1，由审计探针改写）：strict 下本地已提交，WAL 投影器（这里由 beforeWait 模拟）已把 Remote 写进权威、
// tracker 转 Committed 并关闭；strict 等待开始之前，另一笔 Remote 事务准入时按容量淘汰了这条已关闭的 tracker、占住唯一名额（pending 不淘汰）。
// 等待重新登记 tracker 得到 entity.ErrRemoteOverloaded。批次对 strict 的任何 Commit 错误都按结果未知交给 finalizer（之后按已提交收尾），
// 而这个错误不带 entity.ErrRemotePersistenceIndeterminate，nest 按判别表第 4 行的判据把回复标成 ErrRemotePartRejected（Remote 没写入），
// 调用方照做会把已经提交的 Remote 部分再写一次。承诺：等待投影器结论的 Commit 凡不是明确拒绝（entity.ErrRemoteRejected）的错误都带
// ErrRemotePersistenceIndeterminate，回复按“第一个命中的行”落在第 2 行、不带 ErrRemotePartRejected，原因 ErrRemoteOverloaded 仍可 errors.Is。
// pipelined handler 带 Remote 批次时同样走 strict 提交路径、同样等待投影器结论（outcome.Durability=3），一并钉住。
func TestRemoteStrictTrackerEvictedAfterCommitReplyHitsDecisionTableRow2(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rawID      int64
		durability nest.DurabilityPolicy
		committer  interface {
			nest.TransactionCommitter
			commits() int32
		}
	}{
		{name: "strict", rawID: 1999, durability: nest.DurabilityStrict, committer: &countingLocalCommitter{}},
		{name: "pipelined", rawID: 1988, durability: nest.DurabilityPipelined, committer: &pipelinedLocalCommitter{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, live := newReloadFixture(t, tc.rawID)
			f.mgr.remote.txMu.Lock()
			f.mgr.remote.txCapacity = 1
			f.mgr.remote.txMu.Unlock()
			var txID entity.RemoteTransactionID
			beforeWait := func(f reloadFixture, commits []entity.RemoteCommit) {
				txID = commits[0].TransactionID
				// 投影器把 Remote 写交给权威并成功：tracker Committed、关闭。
				if _, err := f.mgr.ApplyRemoteCommits(context.Background(), txID, commits); err != nil {
					t.Errorf("projector apply: %v", err)
				}
				if err := f.mgr.FlushRemoteTransaction(context.Background(), txID); err != nil {
					t.Fatal(err)
				}
				// 另一笔 Remote 事务准入：容量满，淘汰最旧的已关闭 tracker（本事务），占住唯一名额。
				if err := f.mgr.trackRemoteTransaction(remoteTestTxID(251)); err != nil {
					t.Errorf("second admission: %v", err)
				}
			}
			manager := boundedConfirmManager{Manager: f.mgr, fixture: f, confirm: 3 * time.Second, beforeWait: beforeWait}
			engine := nest.NewEngine(nest.NestOptionWithGetter(f.access), nest.NestOptionWithWorkerNumAndMsgCap(1, 16),
				nest.NestOptionWithRemoteEntityManager(manager), nest.NestOptionWithTransactionCommitter(tc.committer))
			name := nest.NewHandlerName("reply_sentinels_tracker_evicted_" + tc.name)
			engine.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...nest.HandlerOption) (any, error) {
				live.set("committed-remote", "")
				return "ok", nil
			}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: tc.durability})
			if err := engine.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			reply, err := engine.Request(ctx, name, live.ID(), nil)
			cancel()
			row, hits := firstTableRow(err)
			t.Logf("reply=%v err=%v first-row=%d sentinels=[%s]", reply, err, row, hits)
			status, statusErr := f.storage.CommitStatus(context.Background(), txID)
			if statusErr != nil || status.State != entity.RemoteCommitCommitted {
				t.Fatalf("premise: authority CommitStatus(tx)=%v err=%v, want the Remote write committed", status.State, statusErr)
			}
			if n := tc.committer.commits(); n != 1 {
				t.Fatalf("premise: local committer received %d record(s), want 1 (the local transaction commits first)", n)
			}
			if !errors.Is(err, entity.ErrRemoteOverloaded) {
				t.Fatalf("premise: reply err=%v, want the wait to fail re-admitting the evicted tracker (errors.Is ErrRemoteOverloaded)", err)
			}
			if errors.Is(err, nest.ErrNestCanceled) || errors.Is(err, nest.ErrNestTimeout) {
				t.Fatalf("premise: the caller's own wait ended before the reply: %v", err)
			}
			if errors.Is(err, nest.ErrRemotePartRejected) {
				t.Fatalf("reply err=%v claims the Remote part was rejected (row 4), but the authority committed the Remote write", err)
			}
			if !errors.Is(err, entity.ErrRemotePersistenceIndeterminate) {
				t.Fatalf("reply err=%v (sentinels [%s]): a Commit error that is not an explicit rejection must carry ErrRemotePersistenceIndeterminate", err, hits)
			}
			if row != 2 {
				t.Fatalf("reply err=%v hits decision-table row %d (sentinels [%s]), want row 2", err, row, hits)
			}
		})
	}
}

// pipelinedLocalCommitter 让 pipelined handler 通过注册检查；带 Remote 批次的消息不走 Enqueue（Remote 批次保留自己的两阶段协议，
// 留在 strict 路径，nest/execution.go），本地记录经 Commit 立即持久。Enqueue 被调用说明 Remote 消息离开了 strict 路径，直接报错。
type pipelinedLocalCommitter struct{ countingLocalCommitter }

func (*pipelinedLocalCommitter) Enqueue(context.Context, coredata.CommitRecord) (nest.CommitTicket, error) {
	return nil, errors.New("test: a message with a Remote write batch must stay on the strict commit path")
}

func (*pipelinedLocalCommitter) DurableLSN() uint64 { return 0 }

// RR-20260928-08（第七轮审计 N2）：判别表第 14 行——调用方自己的等待先结束，Nest.Request* 返回 ErrNestCanceled（与 ctx 错误并存）
// 或 ErrNestTimeout，结果未知。在真实 Nest + 正式 Remote Manager 上构造：strict handler 本地已提交，Remote 确认还在等（没有投影器），
// 调用方的 ctx 截止 / 引擎同步等待超时先到。断言回复按“第一个命中的行”落在第 14 行（不命中第 1～13 行，也不落到兜底第 15 行），
// 且本地事务确实已提交——这正是第 14 行“不说明结果、不得据此重试”的原因。
func TestCallerWaitEndedReplyHitsDecisionTableRow14(t *testing.T) {
	for _, tc := range []struct {
		name      string
		rawID     int64
		ctxWait   time.Duration
		syncWait  time.Duration
		sentinels []error
	}{
		{name: "caller_ctx_deadline", rawID: 1989, ctxWait: 200 * time.Millisecond, syncWait: 5 * time.Second,
			sentinels: []error{nest.ErrNestCanceled, context.DeadlineExceeded}},
		{name: "engine_sync_timeout", rawID: 1990, ctxWait: 5 * time.Second, syncWait: 200 * time.Millisecond,
			sentinels: []error{nest.ErrNestTimeout}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, live := newReloadFixture(t, tc.rawID)
			committer := &countingLocalCommitter{}
			// Remote 确认等待比调用方的等待长：调用方先截止，回复送达时批次仍在等 Remote 结论。
			manager := boundedConfirmManager{Manager: f.mgr, fixture: f, confirm: time.Second}
			engine := nest.NewEngine(nest.NestOptionWithGetter(f.access), nest.NestOptionWithWorkerNumAndMsgCap(1, 16),
				nest.NestOptionWithRemoteEntityManager(manager), nest.NestOptionWithTransactionCommitter(committer),
				nest.NestOptionWithSyncTimeout(tc.syncWait))
			name := nest.NewHandlerName("reply_sentinels_row14_" + tc.name)
			engine.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...nest.HandlerOption) (any, error) {
				live.set("row14", "")
				return "ok", nil
			}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityStrict})
			if err := engine.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })

			ctx, cancel := context.WithTimeout(context.Background(), tc.ctxWait)
			reply, err := engine.Request(ctx, name, live.ID(), nil)
			cancel()
			row, hits := firstTableRow(err)
			t.Logf("reply=%v err=%v first-row=%d sentinels=[%s]", reply, err, row, hits)
			for _, want := range tc.sentinels {
				if !errors.Is(err, want) {
					t.Fatalf("premise: reply err=%v (sentinels [%s]), want errors.Is(%v): the caller's own wait must end first", err, hits, want)
				}
			}
			if row != 14 {
				t.Fatalf("reply err=%v hits decision-table row %d (sentinels [%s]), want row 14 (caller's wait ended: outcome unknown)", err, row, hits)
			}
			// 本地事务在 Remote 确认等待之前已提交：回复不说明结果，重试会重复执行。
			deadline := time.Now().Add(3 * time.Second)
			for committer.calls.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if n := committer.calls.Load(); n != 1 {
				t.Fatalf("local committer received %d record(s), want 1: the row-14 reply hides a committed local transaction", n)
			}
		})
	}
}

// countingLocalCommitter 与 localDurableCommitter 相同（立即持久），另计 Commit 次数。
type countingLocalCommitter struct{ calls atomic.Int32 }

func (c *countingLocalCommitter) Commit(context.Context, coredata.CommitRecord) error {
	c.calls.Add(1)
	return nil
}

func (c *countingLocalCommitter) commits() int32 { return c.calls.Load() }

// replySentinels 是 USER_GUIDE §4“回复错误判别”表第 1～14 行的判据，按行号与表对齐；第 15 行是兜底（不命中以上任何一行），
// firstTableRow 对它返回 15。第 7、8 行共用 ErrCreatedEntityLockConflict，按是否同时带 ErrLockTimeout 区分（without / with）；
// 第 13 行 entity.ErrEntityRemoved 只对“handler 内新建本 handler 较早撤销的同 ID”成立，errors.Is 看不出来源，用例自己保证来源。
// row 0 的项不是任何一行的判据，只在命中时打印（context.DeadlineExceeded 与第 2、14 行并存）。
// RR-20260928-08：之前停在 RR-20260928-03 改号之前（兜底记为第 14 行、ErrNestCanceled / ErrNestTimeout 记为 0、第 8 行被当成第 7 行）。
var replySentinels = []struct {
	row     int
	name    string
	err     error
	with    error // 非 nil：还必须同时满足它
	without error // 非 nil：同时满足它时不算这一行
}{
	{row: 1, name: "nest.ErrCommitIndeterminate", err: nest.ErrCommitIndeterminate},
	{row: 2, name: "entity.ErrRemotePersistenceIndeterminate", err: entity.ErrRemotePersistenceIndeterminate},
	{row: 2, name: "entity.ErrRemoteCommitTimeout", err: entity.ErrRemoteCommitTimeout},
	{row: 3, name: "nest.ErrAfterCommitFailed", err: nest.ErrAfterCommitFailed},
	{row: 4, name: "nest.ErrRemotePartRejected", err: nest.ErrRemotePartRejected},
	{row: 5, name: "nest.ErrNestedTransactionCommitted", err: nest.ErrNestedTransactionCommitted},
	{row: 6, name: "nest.ErrNonRollbackNotRequeued", err: nest.ErrNonRollbackNotRequeued},
	{row: 7, name: "nest.ErrCreatedEntityLockConflict(without ErrLockTimeout)", err: nest.ErrCreatedEntityLockConflict, without: nest.ErrLockTimeout},
	{row: 8, name: "nest.ErrCreatedEntityLockConflict+ErrLockTimeout", err: nest.ErrCreatedEntityLockConflict, with: nest.ErrLockTimeout},
	{row: 9, name: "nest.ErrLockTimeout", err: nest.ErrLockTimeout},
	{row: 9, name: "nest.ErrEntityLockGroupChanged", err: nest.ErrEntityLockGroupChanged},
	{row: 9, name: "nest.ErrEntityGroupTransitionPending", err: nest.ErrEntityGroupTransitionPending},
	{row: 10, name: "nest.ErrNestedTransactionRollbackConflict", err: nest.ErrNestedTransactionRollbackConflict},
	{row: 11, name: "nest.ErrNestedTransactionInRemoteMessage", err: nest.ErrNestedTransactionInRemoteMessage},
	{row: 12, name: "nest.ErrCommitRejected", err: nest.ErrCommitRejected},
	{row: 13, name: "entity.ErrEntityRemoved", err: entity.ErrEntityRemoved},
	{row: 14, name: "nest.ErrNestCanceled", err: nest.ErrNestCanceled},
	{row: 14, name: "nest.ErrNestTimeout", err: nest.ErrNestTimeout},
	{row: 0, name: "context.DeadlineExceeded", err: context.DeadlineExceeded},
}

// decisionTableFallbackRow 是判别表的兜底行“不命中以上任何一行：未提交”。
const decisionTableFallbackRow = 15

// firstTableRow 返回判别表“从上到下第一个命中的行”（err 为 nil 时返回 0；不命中第 1～14 行时返回兜底第 15 行），以及命中的全部哨兵名。
func firstTableRow(err error) (int, string) {
	if err == nil {
		return 0, ""
	}
	first := 0
	var hits []string
	for _, s := range replySentinels {
		if !errors.Is(err, s.err) || (s.with != nil && !errors.Is(err, s.with)) || (s.without != nil && errors.Is(err, s.without)) {
			continue
		}
		hits = append(hits, s.name)
		if s.row != 0 && (first == 0 || s.row < first) {
			first = s.row
		}
	}
	if first == 0 {
		first = decisionTableFallbackRow
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
