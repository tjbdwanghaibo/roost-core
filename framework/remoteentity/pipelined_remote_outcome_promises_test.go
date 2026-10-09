package remoteentity

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
)

// RR-20260928-09：pipelined handler 带 Remote 批次时留在 strict 提交路径（nest/execution.go），批次的 outcome.Durability 是 3，
// Commit 与 strict 一样只等 WAL 投影器的结论。Remote 被明确拒绝（tracker 终态 Rejected → entity.ErrRemoteRejected）时，
// 修前 batch.go 的结论处理只认 Durability == 2：批次既不交给 finalizer，也不按 Durability 0 同步回滚——
// DeferUntilDurableOutcome 返回 false、Close 直接释放 gate；实例只被 Commit 出错路径的通用隔离挡住，从不回滚、从不卸载，
// 下一写者一直得到裸 ErrRemoteFenced（不是可重试的 ErrRemoteEntityReloading），直到重启都不可写；
// 提交后工作没有结论可等（nest 计 post_commit_without_outcome_total），Sync 门永久冻结。
// 承诺：pipelined 与 strict 的 Remote 结论处理一致，在真实 Nest + 正式 Remote Manager（newReloadFixture）上逐项断言：
//   - rejected：回复第 4 行；批次接手提交后工作，finalizer 回滚（在 Nest 快 worker 上）、隔离、释放、仅内存卸载，
//     下一次访问从权威重载、被拒修改没有写出；结论 false 恰好一次，AfterCommit 不执行，Sync 门放行；
//   - indeterminate：Remote 确认等到截止（第 2 行），交给 finalizer；投影器随后提交，结论 true 恰好一次，AfterCommit 在快池执行一次，Sync 门放行；
//   - committed：投影器在等待前已提交，回复成功，批次不接手（没有结论要等），AfterCommit 一次，Sync 门放行。
func TestPipelinedRemoteOutcomeMatchesStrict(t *testing.T) {
	type policy struct {
		name       string
		durability nest.DurabilityPolicy
		committer  func() interface {
			nest.TransactionCommitter
			commits() int32
		}
	}
	policies := []policy{
		{"strict", nest.DurabilityStrict, func() interface {
			nest.TransactionCommitter
			commits() int32
		} {
			return &countingLocalCommitter{}
		}},
		{"pipelined", nest.DurabilityPipelined, func() interface {
			nest.TransactionCommitter
			commits() int32
		} {
			return &pipelinedLocalCommitter{}
		}},
	}
	outcomes := []string{"rejected", "indeterminate", "committed"}
	for pi, p := range policies {
		for oi, outcome := range outcomes {
			t.Run(p.name+"/"+outcome, func(t *testing.T) {
				f, live := newReloadFixture(t, 2011+int64(pi*len(outcomes)+oi))
				syncMgr, err := entitysync.NewManager(entitysync.ManagerConfig{Mode: entitysync.ModePeriodic, Interval: time.Hour,
					Transport: entitysync.TransportFunc(func(context.Context, entitysync.SessionID, []byte) error { return nil })})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = syncMgr.Close(context.Background()) })
				packed := func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
					return entity.CopyFrozenSyncPayload(1, []byte("remote")), nil
				}
				live.EnableSync(entity.EntitySyncCreateParam{Enabled: true, EntityID: live.ID(), Namespace: "test",
					Packer: entity.SubjectSyncPackFunc{Snapshot: packed, Delta: func(p entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) { return packed(p) }}})
				if err = syncMgr.Register(live.Sync()); err != nil {
					t.Fatal(err)
				}
				syncState := live.Sync()

				var commitsMu sync.Mutex
				var waited []entity.RemoteCommit
				beforeWait := func(f reloadFixture, commits []entity.RemoteCommit) {
					commitsMu.Lock()
					waited = commits
					commitsMu.Unlock()
					switch outcome {
					case "rejected":
						// WAL 投影器（这里同步模拟）把 Remote 写交给权威、被版本冲突拒绝：tracker 终态 Rejected。
						f.storage.reject.Store(true)
						_, _ = f.mgr.ApplyRemoteCommits(context.Background(), commits[0].TransactionID, commits)
					case "committed":
						if _, err := f.mgr.ApplyRemoteCommits(context.Background(), commits[0].TransactionID, commits); err != nil {
							t.Errorf("projector apply: %v", err)
						}
					}
					// indeterminate：没有投影器结论，等待到 confirm 截止。
				}
				confirm := 3 * time.Second
				if outcome == "indeterminate" {
					confirm = 100 * time.Millisecond
				}
				rec := &remoteOutcomeTap{}
				manager := outcomeTapManager{boundedConfirmManager: boundedConfirmManager{Manager: f.mgr, fixture: f, confirm: confirm, beforeWait: beforeWait}, rec: rec}
				committer := p.committer()
				engine := nest.NewEngine(nest.NestOptionWithGetter(f.access), nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 16}, nest.WorkerPoolConfig{}),
					nest.NestOptionWithRemoteEntityManager(manager), nest.NestOptionWithTransactionCommitter(committer), nest.NestOptionWithEntitySync(syncMgr))
				name := nest.NewHandlerName("pipelined_remote_outcome_" + p.name + "_" + outcome)
				afterCommit := make(chan bool, 4)
				engine.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...nest.HandlerOption) (any, error) {
					live.set("rejected", "")
					live.MarkSyncDirty(1)
					nest.AfterCommit(func() { afterCommit <- fctx.InFastWorker() })
					return "ok", nil
				}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: p.durability})
				if err = engine.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })

				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				reply, err := engine.Request(ctx, name, live.ID(), nil)
				cancel()
				row, hits := firstTableRow(err)
				t.Logf("reply=%v err=%v first-row=%d sentinels=[%s] deferred=%d", reply, err, row, hits, rec.took.Load())
				if errors.Is(err, nest.ErrNestCanceled) || errors.Is(err, nest.ErrNestTimeout) {
					t.Fatalf("premise: the caller's own wait ended before the reply: %v", err)
				}
				if n := committer.commits(); n != 1 {
					t.Fatalf("premise: local committer received %d record(s), want 1 (a message with a Remote batch commits locally on the strict path)", n)
				}

				switch outcome {
				case "rejected":
					if row != 4 || !errors.Is(err, entity.ErrRemoteRejected) {
						t.Fatalf("reply err=%v hits row %d (sentinels [%s]), want row 4 with the ErrRemoteRejected cause", err, row, hits)
					}
					if rec.took.Load() != 1 {
						// Errorf：继续断言隔离 / 卸载，修前红里一并看到实例没有被卸载。
						t.Errorf("explicitly rejected %s Remote batch did not take the post-commit work (deferred=%d): no finalizer settles it, the instance holding the rejected write is never rolled back or unloaded (stays quarantined until restart)",
							p.name, rec.took.Load())
					}
					f.assertReloadedFromAuthority(t, live) // 回滚、隔离、释放 gate、仅内存卸载，从权威重载，被拒修改没有写出
					if n, off := live.rollbacks.Load(), live.rollbacksOffFast.Load(); n != 1 || off != 0 {
						t.Fatalf("RollbackRemoteCommit ran %d time(s), %d off the Nest fast pool; want once on a fast worker", n, off)
					}
					rec.waitDelivered(t, false)
					select {
					case <-afterCommit:
						t.Fatal("AfterCommit ran although the Remote part was rejected")
					default:
					}
				case "indeterminate":
					if row != 2 || !errors.Is(err, entity.ErrRemoteCommitTimeout) {
						t.Fatalf("reply err=%v hits row %d (sentinels [%s]), want row 2 (Remote confirmation deadline)", err, row, hits)
					}
					if rec.took.Load() != 1 {
						t.Fatalf("unknown %s Remote outcome was not handed to the finalizer (deferred=%d)", p.name, rec.took.Load())
					}
					if syncState.SyncCommitReady() {
						t.Fatal("Sync released without a durable Remote conclusion")
					}
					select {
					case <-afterCommit:
						t.Fatal("AfterCommit ran without a durable Remote conclusion")
					default:
					}
					commitsMu.Lock()
					commits := waited
					commitsMu.Unlock()
					// WAL 投影器随后把 Remote 写交给权威：finalizer 在等投影器结论（RR-20260926-38），拿到后收尾并交付 true。
					if _, err := f.mgr.ApplyRemoteCommits(context.Background(), commits[0].TransactionID, commits); err != nil {
						t.Fatalf("projector apply: %v", err)
					}
					rec.waitDelivered(t, true)
					waitAfterCommitOnFast(t, afterCommit)
				case "committed":
					if err != nil || reply != "ok" {
						t.Fatalf("reply=%v err=%v, want success", reply, err)
					}
					if rec.took.Load() != 0 {
						t.Fatalf("committed batch took post-commit work (deferred=%d); Commit succeeded, nothing to wait for", rec.took.Load())
					}
					waitAfterCommitOnFast(t, afterCommit)
				}
				if !syncState.SyncCommitReady() {
					t.Fatalf("Sync gate still frozen after the durable Remote conclusion (%s)", outcome)
				}
				select {
				case <-afterCommit:
					t.Fatal("AfterCommit ran more than once")
				default:
				}
			})
		}
	}
}

func waitAfterCommitOnFast(t *testing.T, afterCommit <-chan bool) {
	t.Helper()
	select {
	case onFast := <-afterCommit:
		if !onFast {
			t.Fatal("AfterCommit ran off the Nest fast pool")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("AfterCommit never ran after the durable commit")
	}
}

// remoteOutcomeTap 记录批次是否接手了 Nest 交来的提交后工作，以及交付的结论。
type remoteOutcomeTap struct {
	took      atomic.Int32
	delivered chan bool
	once      sync.Once
}

func (r *remoteOutcomeTap) channel() chan bool {
	r.once.Do(func() { r.delivered = make(chan bool, 4) })
	return r.delivered
}

func (r *remoteOutcomeTap) waitDelivered(t *testing.T, want bool) {
	t.Helper()
	select {
	case got := <-r.channel():
		if got != want {
			t.Fatalf("durable outcome delivered committed=%v, want %v", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("durable outcome (committed=%v) never delivered to the post-commit work", want)
	}
	select {
	case got := <-r.channel():
		t.Fatalf("durable outcome delivered twice (second committed=%v)", got)
	default:
	}
}

type outcomeTapManager struct {
	boundedConfirmManager
	rec *remoteOutcomeTap
}

func (m outcomeTapManager) PrepareRemoteWriteBatch(ctx context.Context, ids []int64) (entity.RemoteWriteBatch, error) {
	batch, err := m.boundedConfirmManager.PrepareRemoteWriteBatch(ctx, ids)
	if err != nil {
		return nil, err
	}
	return outcomeTapBatch{boundedConfirmBatch: batch.(boundedConfirmBatch), rec: m.rec}, nil
}

type outcomeTapBatch struct {
	boundedConfirmBatch
	rec *remoteOutcomeTap
}

func (b outcomeTapBatch) DeferUntilDurableOutcome(fn func(bool)) bool {
	delivered := b.rec.channel()
	ok := b.boundedConfirmBatch.DeferUntilDurableOutcome(func(committed bool) {
		fn(committed)
		delivered <- committed
	})
	if ok {
		b.rec.took.Add(1)
	}
	return ok
}
