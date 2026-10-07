package remoteentity

import (
	"context"
	"errors"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
)

// RR-20260928-11：带 Remote 批次的 pipelined handler 留在 strict 提交路径（nest/execution.go），本地记录经 committer.Commit
// （kit 装配为 engine.Projector.Commit）进入 WAL.Append，记录的 Durability 是 3。修前 Append 只对 DurabilityStrict 等 fsync：
// 这条记录与 async 一样在 fsync 之前返回，于是本地锁释放、Remote 确认开始、回复（包括第 4 行“本地已提交、Remote 被拒绝”）、
// AfterCommit 都可能早于本地部分落盘。承诺：与 strict 相同，本地记录 fsync 完成之后这些才发生。
// 真实 WAL（GroupCommitInterval=1h 关掉后台刷盘，只有 requireSync 的批次 fsync）+ 正式 Projector（ManualReplay，不回放、不 Ack，
// 不产生额外 fsync）+ 真实 Nest + 正式 Remote Manager（newReloadFixture）；投影器的 Remote 结论由 beforeWait 同步给出。
// 用 WAL.Stats().Syncs 判定：每个观察点上它相对提交前的增量为 0，就是“对外可见早于本地 fsync”。
func TestPipelinedRemoteBatchLocalRecordIsFsyncedBeforeVisible(t *testing.T) {
	policies := []nest.DurabilityPolicy{nest.DurabilityStrict, nest.DurabilityPipelined}
	outcomes := []string{"committed", "rejected"}
	for pi, durability := range policies {
		for oi, outcome := range outcomes {
			t.Run(durability.String()+"/"+outcome, func(t *testing.T) {
				f, live := newReloadFixture(t, 2031+int64(pi*len(outcomes)+oi))
				opts := nestwal.DefaultOptions(t.TempDir())
				opts.WriterVersion = nestwal.WriterVersionV2
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
				engineMgr := nest.NewEngine(nest.NestOptionWithGetter(f.access), nest.NestOptionWithWorkerNumAndMsgCap(1, 16),
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
