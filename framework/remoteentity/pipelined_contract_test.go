package remoteentity

import (
	"context"
	"errors"
	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPipelinedRemoteBatchLocalRecordIsFsyncedBeforeVisible(t *testing.T) {
	policies := []nest.DurabilityPolicy{nest.DurabilityStrict, nest.DurabilityPipelined}
	outcomes := []string{"committed", "rejected"}
	for pi, durability := range policies {
		for oi, outcome := range outcomes {
			t.Run(durability.String()+"/"+outcome, func(t *testing.T) {
				f, live := newReloadFixture(t, 2031+int64(pi*len(outcomes)+oi))
				opts := nestwal.DefaultOptions(t.TempDir())
				opts.GroupCommitInterval = time.Hour
				wal, err := nestwal.Open(opts)
				if err != nil {
					t.Fatal(err)
				}
				projector, err := engine.NewProjector(wal, discardingProjectionStore{}, engine.ProjectorOptions{CloseWAL: true, ManualReplay: true})
				if err != nil {
					_ = wal.Close(context.Background())
					t.Fatal(err)
				}
				t.Cleanup(func() {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					_ = projector.Close(ctx)
				})
				base := wal.Stats().Syncs
				syncs := func() uint64 { return wal.Stats().Syncs - base }

				type confirmStart struct {
					syncs    uint64
					lockFree bool
				}
				started := make(chan confirmStart, 1)
				beforeWait := func(f reloadFixture, commits []entity.RemoteCommit) {
					// Remote 确认在慢阶段、本地锁释放之后开始：记下此刻本地记录是否已 fsync、实体锁是否已释放。
					mu := live.GetMutex()
					free := mu.TryLock()
					if free {
						mu.Unlock()
					}
					started <- confirmStart{syncs: syncs(), lockFree: free}
					if outcome == "rejected" {
						f.storage.reject.Store(true)
					}
					_, _ = f.mgr.ApplyRemoteCommits(context.Background(), commits[0].TransactionID, commits)
				}
				manager := boundedConfirmManager{Manager: f.mgr, fixture: f, confirm: 3 * time.Second, beforeWait: beforeWait}
				engineMgr := nest.NewEngine(nest.NestOptionWithGetter(f.access), nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 16}, nest.WorkerPoolConfig{}),
					nest.NestOptionWithRemoteEntityManager(manager), nest.NestOptionWithTransactionCommitter(projector))
				name := nest.NewHandlerName("rr11_remote_local_fsync_" + durability.String() + "_" + outcome)
				afterCommit := make(chan uint64, 2)
				engineMgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...nest.HandlerOption) (any, error) {
					live.set("rr11", "")
					nest.AfterCommit(func() { afterCommit <- syncs() })
					return "ok", nil
				}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: durability})
				if err = engineMgr.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = engineMgr.Shutdown(context.Background()) })

				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				reply, err := engineMgr.Request(ctx, name, live.ID(), nil)
				cancel()
				replySyncs := syncs()
				row, hits := firstTableRow(err)
				var start confirmStart
				select {
				case start = <-started:
				default:
					t.Fatalf("premise: the Remote confirmation never started (reply=%v err=%v)", reply, err)
				}
				t.Logf("reply=%v err=%v first-row=%d sentinels=[%s] confirm-start syncs=+%d lock-free=%v reply syncs=+%d",
					reply, err, row, hits, start.syncs, start.lockFree, replySyncs)
				if !start.lockFree {
					t.Fatal("premise: the local entity lock was still held when the Remote confirmation started")
				}
				switch outcome {
				case "committed":
					if err != nil || reply != "ok" {
						t.Fatalf("reply=%v err=%v, want success", reply, err)
					}
				case "rejected":
					if row != 4 || !errors.Is(err, entity.ErrRemoteRejected) {
						t.Fatalf("reply err=%v hits row %d (sentinels [%s]), want row 4 (local committed, Remote rejected)", err, row, hits)
					}
				}
				if start.syncs < 1 || replySyncs < 1 {
					t.Fatalf("%s handler with a Remote batch: the local WAL record was not fsynced before it became visible: lock released and Remote confirmation started at syncs=+%d, reply (%s) at syncs=+%d",
						durability, start.syncs, outcome, replySyncs)
				}
				if outcome == "committed" {
					select {
					case n := <-afterCommit:
						if n < 1 {
							t.Fatalf("AfterCommit ran at syncs=+%d, before the local record was fsynced", n)
						}
					case <-time.After(3 * time.Second):
						t.Fatal("AfterCommit never ran")
					}
				} else {
					// 拒绝后按 RR-20260928-09 回滚、隔离、仅内存卸载；等收尾完成再结束，避免 finalizer 与关闭交错。
					f.assertReloadedFromAuthority(t, live)
				}
			})
		}
	}
}

type discardingProjectionStore struct{}

func (discardingProjectionStore) Project(context.Context, coredata.CommitRecord) error { return nil }

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
