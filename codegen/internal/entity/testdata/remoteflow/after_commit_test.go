package remoteflow

// B27 第 2 批：Remote 事务提交后的 hook / Close 失败与慢阶段失败的收尾，在正式装配（ManagerAccess 作 Remote loader、entitysync、
// 真实 Mongo / Redis / NATS）上的端到端。
//   - RR-20260926-32 的未验证项“没有用真实 Remote（Mongo ownership/fence + Redis）做‘提交后 hook panic’的端到端验证”与
//     “本回归未直接观察 Sync 帧”：strict 持久提交后业务 release hook panic，Remote 批次仍 Commit（Mongo 有这一笔、没有 pending
//     记录）、内存保留、Sync 门放行（订阅者收到这一笔），回复带 ErrAfterCommitFailed 与 hook 原因。
//   - RR-20260926-46 的未验证项“没有在正式生成工程里构造‘提交后 Close 失败’”：提交后释放 Redis 锁失败（注入点在 Redis 适配器，
//     其余全部真实），回复带 ErrAfterCommitFailed 且 errors.Is(ErrRemoteReleaseIncomplete)，Mongo 与内存都是已提交的值；
//     未提交路径（业务失败）上同样的失败不带哨兵。
//   - RR-20260926-44 的未验证项“未用真实 Nest 慢池 + Remote 争用负载测量快池续行数下降”：Prepare 在慢阶段失败（实体在权威里
//     不存在，冷加载失败）时 Abort 就地收尾，不占快池续行；用阶段指标 nest.stage.duration{stage=logic_queue} 计数。

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	"github.com/tjbdwanghaibo/roost-core/nest"
)

var (
	errRemoteReleaseHook = errors.New("remoteflow: release hook failed after the Remote commit")
	errRedisUnlock       = errors.New("remoteflow: injected Redis unlock failure")
)

// afterCommitMode 是 remote_after_commit 的参数：armHook 让本次释放时 hook panic；failRedis 让本次 Close 释放锁时 Redis Eval 失败；
// fail 让 handler 以业务错误结束（未提交路径）。
type afterCommitMode struct {
	armHook, failRedis, fail bool
}

// newAfterCommitRig 装配 strict 的正式链路，并登记 remote_after_commit handler 与 release hook。
func newAfterCommitRig(t *testing.T, extra ...nest.NestOption) (*rejectRig, nest.HandlerName) {
	t.Helper()
	ctx, mongo, redis, database := rejectTestEnv(t)
	name := nest.NewHandlerName("remote_after_commit")
	var (
		hookArmed  int64
		hookArmedM sync.Mutex
	)
	rig := newRejectRig(t, ctx, mongo, redis, database, nest.DurabilityStrict, nil, func(rig *rejectRig) {
		rig.scheduler.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
			mode := params[0].(afterCommitMode)
			vault := es[0].(*SyncedVault)
			vault.balance.SetValue(vault.balance.GetValue() + 1)
			vault.items.SetValue(vault.items.GetValue() + 1)
			if mode.armHook {
				hookArmedM.Lock()
				hookArmed = vault.ID()
				hookArmedM.Unlock()
			}
			if mode.failRedis {
				// 锁已在慢阶段取得；从这里起 Redis 不可用，提交后的 Close 释放锁失败。
				fault := errRedisUnlock
				rig.redis.evalFault.Store(&fault)
			}
			if mode.fail {
				return nil, errRemoteBusiness
			}
			return nil, nil
		}, nest.HandlerMeta{Rollback: nest.RollbackState, Durability: nest.DurabilityStrict})
		unhook, err := rig.access.RegisterOnEntityRelease(func(e entity.IThreadSafeEntity) {
			hookArmedM.Lock()
			armed := hookArmed
			if armed != 0 && e.ID() == armed {
				hookArmed = 0
			}
			hookArmedM.Unlock()
			if armed != 0 && e.ID() == armed {
				panic(errRemoteReleaseHook)
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(unhook)
	}, extra...)
	return rig, name
}

var errRemoteBusiness = errors.New("remoteflow: business failed after the Remote write")

func TestGeneratedRemoteReleaseHookPanicAfterDurableCommitStillCommits(t *testing.T) {
	rig, name := newAfterCommitRig(t)
	id := rig.seed(t, 9700)
	if err := rig.request(rig.ctx, id); err != nil {
		t.Fatal(err)
	}
	rig.assertNoPending(t)
	rig.flushUntil(t, id, "the first committed write")

	// 提交后 release hook panic：Remote 批次仍 Commit。
	_, err := rig.scheduler.Request(rig.ctx, name, id, nest.Params{afterCommitMode{armHook: true}})
	if !errors.Is(err, nest.ErrAfterCommitFailed) || !errors.Is(err, errRemoteReleaseHook) {
		t.Fatalf("strict Remote write with a release hook panic after the durable commit replied %v, want ErrAfterCommitFailed with the hook cause", err)
	}
	t.Logf("committed reply: %v", err)
	rig.assertNoPending(t)
	if version, balance, items, _ := rig.stored(t, id); version != 2 || balance != 2 || items != 2 {
		t.Fatalf("Mongo version=%d balance=%d items=%d, want the committed write 2/2/2 (the hook panic must not Abort a committed batch)", version, balance, items)
	}
	if balance, items := rig.memory(t, id); balance != 2 || items != 2 {
		t.Fatalf("memory balance=%d items=%d, want 2/2", balance, items)
	}
	// Sync 门随 Commit 放行：订阅者收到这一笔。
	if got := rig.flushUntil(t, id, "the Sync release after the commit with the hook panic"); got[len(got)-1].balance != 2 {
		t.Fatalf("frames after the commit=%v, want balance=2", got)
	}
	select {
	case err := <-rig.fatal:
		t.Fatalf("runtime fatal: %v", err)
	default:
	}

	// 未提交路径保持性：业务失败 + 同样的 hook panic，不带哨兵，Mongo / 内存不变（回复是否保留业务错误见下一个用例）。
	_, err = rig.scheduler.Request(rig.ctx, name, id, nest.Params{afterCommitMode{armHook: true, fail: true}})
	if err == nil || errors.Is(err, nest.ErrAfterCommitFailed) {
		t.Fatalf("rolled-back Remote write with a release hook panic replied %v, want an error without ErrAfterCommitFailed", err)
	}
	t.Logf("rolled-back reply: %v", err)
	rig.assertNoPending(t)
	if version, balance, items, _ := rig.stored(t, id); version != 2 || balance != 2 || items != 2 {
		t.Fatalf("Mongo after the rolled-back write version=%d balance=%d items=%d, want 2/2/2", version, balance, items)
	}
	if balance, items := rig.memory(t, id); balance != 2 || items != 2 {
		t.Fatalf("memory after the rolled-back write balance=%d items=%d, want 2/2", balance, items)
	}
	rig.flushNone(t, id, "after the rolled-back write")

	// 之后照常可写。
	if err := rig.request(rig.ctx, id); err != nil {
		t.Fatalf("write after the hook failures: %v", err)
	}
	rig.assertNoPending(t)
	if version, balance, items, _ := rig.stored(t, id); version != 3 || balance != 3 || items != 3 {
		t.Fatalf("Mongo after the next write version=%d balance=%d items=%d, want 3/3/3", version, balance, items)
	}
}

// 未提交路径上 release hook panic 时，回复仍应带业务错误。2026-09-30 实跑（B27 第 2 批）：strict Remote 消息业务失败后 hook panic，
// 回复只有 `remoteflow: release hook failed after the Remote commit`，业务错误丢失（dataengine fixture 的本地消息 async / strict /
// pipelined 同样）。hook 的 panic 从 dispatchLoadedEntities 的 `defer release()`（nest/nest_dispatch.go:404-406）经 Guard.ReleaseEntity
// 传到 runNestLogic 的 recover（nest/nest_dispatch.go:203-209 `err = recoveredErr`），在途的 handler 错误被整个替换。
func TestGeneratedRemoteRolledBackReleaseHookPanicKeepsBusinessError(t *testing.T) {
	rig, name := newAfterCommitRig(t)
	id := rig.seed(t, 9705)
	if err := rig.request(rig.ctx, id); err != nil {
		t.Fatal(err)
	}
	rig.assertNoPending(t)
	_, err := rig.scheduler.Request(rig.ctx, name, id, nest.Params{afterCommitMode{armHook: true, fail: true}})
	if !errors.Is(err, errRemoteBusiness) || errors.Is(err, nest.ErrAfterCommitFailed) {
		t.Fatalf("rolled-back Remote write with a release hook panic replied %v, want the business error kept", err)
	}
}

func TestGeneratedRemoteCloseFailureAfterCommitCarriesSentinel(t *testing.T) {
	rig, name := newAfterCommitRig(t)
	// 释放失败会让该实体的 Redis 锁留在本进程的“已取得”状态（见 TestGeneratedRemoteEntityWritableAfterUnlockFailure），
	// 已提交路径与未提交路径各用一个实体。
	committedID, rolledBackID := rig.seed(t, 9710), rig.seed(t, 9711)
	for _, id := range []int64{committedID, rolledBackID} {
		if err := rig.request(rig.ctx, id); err != nil {
			t.Fatal(err)
		}
		rig.assertNoPending(t)
		rig.flushUntil(t, id, "the first committed write")
	}

	// 提交后 Close 释放 Redis 锁失败（UnlockWithRetry 用尽重试）：已提交，回复带哨兵与 ErrRemoteReleaseIncomplete。
	_, err := rig.scheduler.Request(rig.ctx, name, committedID, nest.Params{afterCommitMode{failRedis: true}})
	rig.redis.evalFault.Store(nil)
	if !errors.Is(err, nest.ErrAfterCommitFailed) || !errors.Is(err, entity.ErrRemoteReleaseIncomplete) || !errors.Is(err, errRedisUnlock) {
		t.Fatalf("committed Remote write whose Close failed replied %v, want ErrAfterCommitFailed wrapping ErrRemoteReleaseIncomplete and the Redis cause", err)
	}
	t.Logf("committed reply: %v", err)
	rig.assertNoPending(t)
	if version, balance, items, _ := rig.stored(t, committedID); version != 2 || balance != 2 || items != 2 {
		t.Fatalf("Mongo version=%d balance=%d items=%d, want the committed write 2/2/2", version, balance, items)
	}
	if balance, items := rig.memory(t, committedID); balance != 2 || items != 2 {
		t.Fatalf("memory balance=%d items=%d, want 2/2", balance, items)
	}
	if got := rig.flushUntil(t, committedID, "the Sync release after the commit whose Close failed"); got[len(got)-1].balance != 2 {
		t.Fatalf("frames after the commit=%v, want balance=2", got)
	}
	select {
	case err := <-rig.fatal:
		t.Fatalf("runtime fatal: %v", err)
	default:
	}

	// 未提交路径保持性：业务失败后同样的释放失败（Abort 路径），不带哨兵，保留 ErrRemoteReleaseIncomplete。
	_, err = rig.scheduler.Request(rig.ctx, name, rolledBackID, nest.Params{afterCommitMode{failRedis: true, fail: true}})
	rig.redis.evalFault.Store(nil)
	if !errors.Is(err, errRemoteBusiness) || errors.Is(err, nest.ErrAfterCommitFailed) {
		t.Fatalf("rolled-back Remote write whose Close failed replied %v, want the business error without ErrAfterCommitFailed", err)
	}
	if !errors.Is(err, entity.ErrRemoteReleaseIncomplete) {
		t.Fatalf("rolled-back Remote write whose Close failed replied %v, want ErrRemoteReleaseIncomplete kept in the cause chain", err)
	}
	t.Logf("rolled-back reply: %v", err)
	rig.assertNoPending(t)
	if version, balance, items, _ := rig.stored(t, rolledBackID); version != 1 || balance != 1 || items != 1 {
		t.Fatalf("Mongo after the rolled-back write version=%d balance=%d items=%d, want 1/1/1", version, balance, items)
	}
	if balance, items := rig.memory(t, rolledBackID); balance != 1 || items != 1 {
		t.Fatalf("memory after the rolled-back write balance=%d items=%d, want 1/1", balance, items)
	}
}

// 提交后释放 Redis 锁失败（回复已带 ErrAfterCommitFailed + ErrRemoteReleaseIncomplete）之后，同一实体在 Redis 租期（LockTTL 3s）
// 到期后应当重新可写：释放失败只是资源没交还，不是实体从此不可写。
func TestGeneratedRemoteEntityWritableAfterUnlockFailure(t *testing.T) {
	// 2026-09-30 修前实跑（B27 第 2 批，RR-20260930-21）：10s 内 50 次重试全部 `remote_entity: shared lock <id>: versioned lock already acquired`。
	// UnlockWithRetry 用尽重试后本地 acquired 仍为 true，下一次 beginWrite 的 rMu.Lock → TryLock 看本地状态就拒绝，不再问 Redis。
	// 修后释放失败让锁进入"持有状态未知"，TryLock 以 Redis 为准：租约仍是上一代 token 就在同一条脚本里换成新 token 重新取得。
	rig, name := newAfterCommitRig(t)
	id := rig.seed(t, 9715)
	if err := rig.request(rig.ctx, id); err != nil {
		t.Fatal(err)
	}
	rig.assertNoPending(t)
	_, err := rig.scheduler.Request(rig.ctx, name, id, nest.Params{afterCommitMode{failRedis: true}})
	rig.redis.evalFault.Store(nil)
	if !errors.Is(err, nest.ErrAfterCommitFailed) || !errors.Is(err, entity.ErrRemoteReleaseIncomplete) {
		t.Fatalf("premise: committed Remote write whose Close failed replied %v", err)
	}
	// Redis 已恢复；租期到期前拿不到锁是正常的，到期后（LockTTL 3s，留到 10s）必须能写。
	deadline := time.Now().Add(10 * time.Second)
	var last error
	attempts := 0
	for time.Now().Before(deadline) {
		attempts++
		last = rig.request(rig.ctx, id)
		if last == nil {
			break
		}
		if errors.Is(last, nest.ErrAfterCommitFailed) || errors.Is(last, entity.ErrRemotePersistenceIndeterminate) {
			t.Fatalf("write after the unlock failure: %v", last)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if last != nil {
		t.Fatalf("the entity stayed unwritable for 10s after a failed unlock (LockTTL 3s, Redis healthy again, %d attempts): last=%v", attempts, last)
	}
	rig.assertNoPending(t)
	if version, balance, items, _ := rig.stored(t, id); version != 3 || balance != 3 || items != 3 {
		t.Fatalf("Mongo after the next write version=%d balance=%d items=%d, want 3/3/3", version, balance, items)
	}
}

// logicQueueObservations 是 handler 被投递到快池续行（remoteLogicCall）的次数：慢阶段准备完成后回快池执行 handler、
// Abort / RunLocal 经本地执行器回快池都记一次 nest.stage.duration{stage=logic_queue}。
func logicQueueObservations(handler string) int64 {
	var total int64
	for _, metric := range metrics.Snapshot() {
		if metric.Name == "nest.stage.duration" && metric.Labels["stage"] == "logic_queue" && metric.Labels["handler"] == handler {
			total += metric.Count
		}
	}
	return total
}

func TestGeneratedRemotePrepareFailureAbortsWithoutFastContinuation(t *testing.T) {
	rig, name := newAfterCommitRig(t, nest.NestOptionWithStageMetrics(true))
	id := rig.seed(t, 9720)
	handler := name.String()
	// 正对照：一笔成功的 strict 写占用的快池续行数（慢阶段准备后回快池执行 handler，提交后经本地执行器回快池的收尾各记一次）；
	// 以它为基线，失败的 Prepare 不得再增加。
	before := logicQueueObservations(handler)
	if _, err := rig.scheduler.Request(rig.ctx, name, id, nest.Params{afterCommitMode{}}); err != nil {
		t.Fatal(err)
	}
	perCommit := logicQueueObservations(handler) - before
	if perCommit < 1 {
		t.Fatalf("premise: a committed Remote write recorded %d logic_queue continuation(s), want at least the handler's", perCommit)
	}
	t.Logf("logic_queue continuations per committed write=%d", perCommit)
	rig.assertNoPending(t)

	// 慢阶段 Prepare 失败：目标在权威里不存在（ManagerAccess 冷加载真实 Mongo 得不到信封），Abort 就地收尾。
	missing, err := entity.BuildEntityID(9721, EntityKindSyncedVault)
	if err != nil {
		t.Fatal(err)
	}
	before = logicQueueObservations(handler)
	_, soloErr := rig.scheduler.Request(rig.ctx, name, missing, nest.Params{afterCommitMode{}})
	if soloErr == nil || errors.Is(soloErr, nest.ErrAfterCommitFailed) || errors.Is(soloErr, entity.ErrRemotePersistenceIndeterminate) {
		t.Fatalf("write to the missing entity replied %v, want a definite Prepare failure", soloErr)
	}
	if got := logicQueueObservations(handler) - before; got != 0 {
		t.Fatalf("a Prepare that failed on the slow pool recorded %d logic_queue continuation(s), want 0: the unfinalized batch's Abort must stay in place", got)
	}
	t.Logf("Prepare failure: %v", soloErr)

	// 争用：32 条并发请求争用同一个缺失实体，另有 8 条对已有实体的写同时进行（真实慢池 + Remote 争用）。
	const failing, succeeding = 32, 8
	before = logicQueueObservations(handler)
	var (
		group     sync.WaitGroup
		mu        sync.Mutex
		failures  []error
		successes int
	)
	for range failing {
		group.Go(func() {
			_, err := rig.scheduler.Request(rig.ctx, name, missing, nest.Params{afterCommitMode{}})
			mu.Lock()
			failures = append(failures, err)
			mu.Unlock()
		})
	}
	for range succeeding {
		group.Go(func() {
			if _, err := rig.scheduler.Request(rig.ctx, name, id, nest.Params{afterCommitMode{}}); err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			} else {
				mu.Lock()
				failures = append(failures, err)
				mu.Unlock()
			}
		})
	}
	group.Wait()
	if len(failures) != failing {
		t.Fatalf("%d request(s) failed, want exactly the %d writes to the missing entity: %v", len(failures), failing, failures)
	}
	for _, err := range failures {
		if err == nil || errors.Is(err, nest.ErrAfterCommitFailed) || errors.Is(err, entity.ErrRemotePersistenceIndeterminate) {
			t.Fatalf("write to the missing entity replied %v, want a definite Prepare failure", err)
		}
	}
	if got := logicQueueObservations(handler) - before; got != succeeding*perCommit {
		t.Fatalf("logic_queue continuations=%d for %d failed Prepares + %d committed writes, want %d (%d per committed write): an unfinalized batch's Abort must not hop to the fast pool", got, failing, succeeding, succeeding*perCommit, perCommit)
	}
	if inFlight := rig.manager.Stats().WritesInFlight; inFlight != 0 {
		t.Fatalf("Remote writes in flight after the failed Prepares=%d", inFlight)
	}
	rig.assertNoPending(t)
	if version, balance, items, _ := rig.stored(t, id); version != 1+succeeding || balance != 1+succeeding {
		t.Fatalf("Mongo version=%d balance=%d items=%d, want %d", version, balance, items, 1+succeeding)
	}
	if _, _, _, found := rig.stored(t, missing); found {
		t.Fatal("the missing entity appeared in Mongo")
	}
	if rig.access.Manager().Get(missing) != nil {
		t.Fatal("the missing entity was published in memory")
	}
}
