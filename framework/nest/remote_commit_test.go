// Remote 批次随消息事务提交：拒绝 / 未知 / 已提交三种结论的回复与哨兵、提交后工作的移交、fence 后拒绝 Remote 直写。

package nest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/remoteentity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

const cachedAllowStaleSchema uint32 = 0x6e6f7374 // 只在本用例使用的 schema 号

type snapshotClientManager struct {
	entity.IRemoteEntityManager
	client *remoteentity.SnapshotClient
}

func (m snapshotClientManager) ReadRemoteSnapshot(ctx context.Context, key entity.RemoteSnapshotKey, consistency entity.RemoteReadConsistency, minVersion uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
	return m.client.ReadRemoteSnapshot(ctx, key, consistency, minVersion)
}

type cachedAllowStaleRequest struct{ access RemoteAccess }

func (r cachedAllowStaleRequest) RemoteAccess() []RemoteAccess { return []RemoteAccess{r.access} }

func TestCachedRemoteAccessWithAllowStaleStillAcceptsAnOlderSnapshot(t *testing.T) {
	ctx := context.Background()
	if err := entity.RegisterRemoteSnapshotDecoder(cachedAllowStaleSchema, func(data []byte) (any, error) { return string(data), nil }); err != nil &&
		!errors.Is(err, entity.ErrRemoteSnapshotDecoderDuplicate) {
		t.Fatal(err)
	}
	refID := mustBuildCastID(t, 7651, entity.EntityCategoryRemote, nestRemoteManagedKind)
	key := entity.RemoteSnapshotKey{EntityID: refID, Kind: nestRemoteManagedKind, Scope: 7}
	loads := 0
	client, err := remoteentity.NewSnapshotClient(nil, remoteentity.SnapshotClientDeps{
		ConsumerSID: 1,
		Loader: func(_ context.Context, requested entity.RemoteSnapshotKey, _ entity.RemoteReadConsistency, _ uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
			loads++
			data := []byte("guild-summary-v3")
			return entity.RemoteSnapshotEnvelope{
				Key: requested, StateVersion: 3, MarkerEpoch: 1, RouteEpoch: 1, Schema: cachedAllowStaleSchema, Codec: 1, Full: true,
				Checksum: entity.RemoteSnapshotChecksum(data), Payload: entity.CopyFrozenRemoteSnapshotPayload(data),
			}, true, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Stop(ctx) }()
	// L1 先持有版本 3（Monotonic 回源一次）。
	if got, found, err := client.ReadRemoteSnapshot(ctx, key, entity.RemoteReadMonotonic, 0); err != nil || !found || got.StateVersion != 3 {
		t.Fatalf("seed read: version=%d found=%v err=%v", got.StateVersion, found, err)
	}
	manager := snapshotClientManager{client: client}
	ref := entity.RemoteViewRef{EntityID: refID, Kind: nestRemoteManagedKind, RouteEpoch: 1}
	access := RemoteAccess{Alias: "guild", Ref: ref, Mode: RemoteAcquireCache, Scope: 7, MinVersion: 5, Required: true}

	stale := access
	stale.AllowStale = true
	msg := &Msg{Params: []any{cachedAllowStaleRequest{access: stale}}}
	if err := prepareRemoteSnapshots(msg, nil, manager); err != nil {
		t.Fatalf("cached access with allow_stale and min_version=5 over a cached version 3: %v; want the older snapshot accepted (RemoteSnapshot.Accepts honours AllowStale)", err)
	}

	msg = &Msg{Params: []any{cachedAllowStaleRequest{access: access}}}
	err = prepareRemoteSnapshots(msg, nil, manager)
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("cached access with min_version=5 and no allow_stale over a cached version 3: err=%v; want a stale refusal", err)
	}
	if loads != 1 {
		t.Fatalf("authority loads = %d, want 1 (a Cached access never loads)", loads)
	}
}

type remotePartOutcomeBatch struct {
	stagedRemoteBatch
	commitErr error
}

func (b *remotePartOutcomeBatch) Commit(ctx context.Context) ([]entity.RemoteCommitReceipt, error) {
	_, _ = b.stagedRemoteBatch.Commit(ctx)
	return nil, b.commitErr
}

func TestNestedCommitInRemotePrepareAlongsideCommittedLocalPartKeepsReplyText(t *testing.T) {
	for _, tc := range []struct {
		name      string
		unique    int64
		commitErr error
		sentinel  error
	}{
		{name: "remote_part_rejected", unique: 48620, commitErr: entity.ErrRemoteRejected, sentinel: ErrRemotePartRejected},
		{name: "remote_outcome_unknown", unique: 48622, commitErr: errors.Join(entity.ErrRemotePersistenceIndeterminate, errors.New("remote reply lost")),
			sentinel: entity.ErrRemotePersistenceIndeterminate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			localID, local := newAsyncPilotEntity(t, tc.unique, 1)
			remoteID := mustBuildCastID(t, tc.unique+1, entity.EntityCategoryRemote, nestRemoteManagedKind)
			getter := newMockGetter()
			getter.Add(newMockEntity(remoteID, entity.EntityCategoryRemote))
			getter.Add(local)
			iso := &recordsCommitter{}
			var isoErr error
			batch := &remotePartOutcomeBatch{commitErr: tc.commitErr}
			var commits atomic.Int32
			batch.commit = func() { commits.Add(1) }
			remote := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) {
				// 批次还没挂上：独立事务照常执行并提交，不认领消息（RR-20260926-84 复核残留，B19）。
				_, isoErr = RunIsolatedTransaction(context.Background(), iso, "rr08_iso_in_prepare", func() (any, error) {
					return nil, CurrentRollbackTx().AddMutation(EntityMutation{Key: dataengine.DocumentKey{ID: localID, Database: "test", Resource: "rr08_prepare"}, Kind: dataengine.MutationPut, ExpectedVersion: 0, NextVersion: 1, Data: []byte(`{}`)})
				})
				return batch, nil
			}}
			mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(remote), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
			name := NewHandlerName("rr08_nested_alongside_" + tc.name)
			mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				return "ok", nil
			}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = mgr.Shutdown(context.Background()) }()
			msg, ch := GenSyncMsg(MsgTypeMulti)
			msg.Name, msg.Tids, msg.Cost, msg.HasRemote = name.String(), []int64{remoteID, localID}, true, true
			if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
				t.Fatal(err)
			}
			var reply any
			select {
			case reply = <-ch:
			case <-time.After(10 * time.Second):
				t.Fatal("Remote message did not reply within 10s")
			}
			err, _ := reply.(error)
			t.Logf("reply=%v", reply)

			if isoErr != nil || iso.count() != 1 {
				t.Fatalf("premise: isolated transaction in PrepareRemoteWriteBatch err=%v commits=%d, want it committed once", isoErr, iso.count())
			}
			if n := commits.Load(); n != 1 || batch.aborted.Load() {
				t.Fatalf("premise: batch commits=%d aborted=%v, want the message's own local transaction committed and the batch committed once", n, batch.aborted.Load())
			}
			if !errors.Is(err, ErrNestedTransactionCommitted) || !errors.Is(err, tc.sentinel) || !errors.Is(err, tc.commitErr) {
				t.Fatalf("reply=%v: want errors.Is ErrNestedTransactionCommitted, %v and the Remote cause", reply, tc.sentinel)
			}
			if errors.Is(err, ErrAfterCommitFailed) {
				t.Fatalf("reply=%v: the Remote part was not confirmed, must not claim ErrAfterCommitFailed", reply)
			}
			if text := err.Error(); strings.Contains(text, "before the message failed") {
				t.Fatalf("reply text %q says the message failed although its own local transaction committed", text)
			}
		})
	}
}

func TestRemoteReleaseHookPanicAfterDurableCommitStillCommits(t *testing.T) {
	for _, tc := range []struct {
		name        string
		failHandler bool
	}{
		{name: "hook panic after durable commit"},
		{name: "handler failure before commit", failHandler: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unique := int64(9970)
			if tc.failHandler {
				unique = 9980
			}
			localID, e := newAsyncPilotEntity(t, unique, 10)
			owner := entity.NewEntityManager()
			if err := owner.TryAdd(e); err != nil {
				t.Fatal(err)
			}
			armed := false
			hookErr := errors.New("release hook failed")
			defer owner.RegisterOnEntityRelease(func(entity.IThreadSafeEntity) {
				if armed {
					armed = false
					panic(hookErr)
				}
			})()
			getter := newMockGetter()
			remoteID := mustBuildCastID(t, unique+1, entity.EntityCategoryRemote, nestRemoteManagedKind)
			getter.Add(newMockEntity(remoteID, entity.EntityCategoryRemote))
			getter.Add(e)
			batch := &stagedRemoteBatch{}
			committed := false
			batch.commit = func() { committed = true }
			manager := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) { return batch, nil }}
			committer := &recordingCommitter{}
			mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(manager), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
			name := NewHandlerName("remote_release_hook_panic")
			mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				old := e.dao.Value
				RecordUndo(e.dao, 1, func() error { e.dao.Value = old; return nil })
				e.dao.Value++
				if tc.failHandler {
					return nil, errors.New("business refused")
				}
				armed = true
				return "ok", MarkPersist(e.dao, 1)
			}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict})
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer mgr.Shutdown(context.Background())
			msg, ch := GenSyncMsg(MsgTypeMulti)
			msg.Name = name.String()
			msg.Tids = []int64{remoteID, localID}
			msg.Cost = true
			msg.HasRemote = true
			if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
				t.Fatal(err)
			}
			got := stagedWait(t, ch)
			replyErr, _ := got.(error)
			if tc.failHandler {
				if committed || !batch.aborted.Load() || len(committer.record.Mutations) != 0 || e.dao.Value != 10 {
					t.Fatalf("uncommitted failure: reply=%v Commit=%v Abort=%v mutations=%d value=%d", got, committed, batch.aborted.Load(), len(committer.record.Mutations), e.dao.Value)
				}
				return
			}
			if len(committer.record.Mutations) == 0 {
				t.Fatalf("transaction was not durably committed: reply=%v", got)
			}
			if !committed || batch.aborted.Load() {
				t.Fatalf("remote batch after durable commit + release hook panic: Commit=%v Abort=%v reply=%v", committed, batch.aborted.Load(), got)
			}
			if !errors.Is(replyErr, hookErr) || !errors.Is(replyErr, ErrAfterCommitFailed) {
				t.Fatalf("post-commit hook failure must be reported as ErrAfterCommitFailed with the hook cause, reply=%v", got)
			}
			if e.dao.Value != 11 {
				t.Fatalf("committed local value rolled back: %d", e.dao.Value)
			}
		})
	}
}

func TestRemoteManagedDispatchRejectsBroadcast(t *testing.T) {
	const kind entity.EntityKind = 195
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, err := entity.BuildEntityID(9202, kind)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareRemoteWriteBatch(nil, &Msg{Type: MsgTypeBroadcast, Tids: []int64{id}, HasRemote: true}); !errors.Is(err, ErrRemoteBroadcastUnsupported) {
		t.Fatalf("broadcast error=%v", err)
	}
}

type remoteBatchIntegrationFake struct {
	commit        entity.RemoteCommit
	aborted       bool
	indeterminate bool
	closed        bool
}

func (b *remoteBatchIntegrationFake) EntityIDs() []int64 { return []int64{b.commit.EntityID} }
func (b *remoteBatchIntegrationFake) FinalizeLocked(outcome entity.RemoteTransactionOutcome) error {
	b.commit.TransactionID = outcome.TransactionID
	return nil
}
func (b *remoteBatchIntegrationFake) Commits() []entity.RemoteCommit {
	return []entity.RemoteCommit{b.commit.Clone()}
}
func (b *remoteBatchIntegrationFake) Commit(context.Context) ([]entity.RemoteCommitReceipt, error) {
	return nil, nil
}
func (b *remoteBatchIntegrationFake) Abort(context.Context, error) error {
	b.aborted = true
	return nil
}
func (b *remoteBatchIntegrationFake) Indeterminate(context.Context, error) error {
	b.indeterminate = true
	return nil
}
func (b *remoteBatchIntegrationFake) Close(context.Context) error {
	b.closed = true
	return nil
}

func TestFinalizeRemoteWriteBatchProducesValidWALMutation(t *testing.T) {
	const kind entity.EntityKind = 194
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, err := entity.BuildEntityID(9201, kind)
	if err != nil {
		t.Fatal(err)
	}
	batch := &remoteBatchIntegrationFake{commit: entity.RemoteCommit{
		EntityID: id, Kind: kind, BaseVersion: 1, NextVersion: 2,
		MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Codec: 1,
		Mutations: []entity.RemoteDataMutation{{Collection: "players", ID: id, Version: 2, Mask: 1, Data: []byte("state")}},
	}}
	msg := &Msg{RemoteWriteBatch: batch}
	tx := NewRollbackTx(RollbackState)
	tx.handler = "remote.integration"
	if err := msg.finalizeRemoteWriteBatch(tx); err != nil {
		t.Fatal(err)
	}
	record, err := tx.prepareCommitRecord()
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Mutations) != 1 || record.Mutations[0].Remote == nil || len(record.Mutations[0].Data) != 0 {
		t.Fatalf("record=%+v", record)
	}
}

func TestCommitRecordAcceptsMixedRemoteAndOrdinaryMutations(t *testing.T) {
	const kind entity.EntityKind = 193
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, err := entity.BuildEntityID(9200, kind)
	if err != nil {
		t.Fatal(err)
	}
	txID := newTransactionID()
	remote := entity.RemoteCommit{
		TransactionID: entity.RemoteTransactionID(txID),
		EntityID:      id,
		Kind:          kind,
		BaseVersion:   1,
		NextVersion:   2,
		MarkerEpoch:   1,
		RouteEpoch:    1,
		Schema:        1,
		Codec:         1,
		Mutations: []entity.RemoteDataMutation{{
			Collection: "players",
			ID:         id,
			Version:    2,
			Mask:       1,
			Data:       []byte("remote-state"),
		}},
	}
	record := CommitRecord{
		ID: txID,
		Mutations: []EntityMutation{
			{
				Key:             dataengine.DocumentKey{Resource: "players", ID: id},
				Kind:            dataengine.MutationPut,
				ExpectedVersion: remote.BaseVersion,
				NextVersion:     remote.NextVersion,
				Remote:          &remote,
			},
			{
				Key:             dataengine.DocumentKey{Database: "game", Resource: "mail", ID: 42},
				Kind:            dataengine.MutationPut,
				ExpectedVersion: 0,
				NextVersion:     1,
				Data:            []byte("ordinary-state"),
			},
		},
	}
	if err := validateCommitRecord(record); err != nil {
		t.Fatalf("validate error=%v", err)
	}
}

func TestRemoteTransactionUsesNestOutbox(t *testing.T) {
	tx := NewRollbackTx(RollbackNone)
	tx.remoteWrite = true
	if err := tx.Emit(Effect{Topic: "player.changed", Payload: []byte("event")}); err != nil {
		t.Fatal(err)
	}
	if tx.durability != DurabilityStrict || len(tx.effects) != 1 {
		t.Fatalf("durability=%v effects=%d", tx.durability, len(tx.effects))
	}
}

func TestIndeterminateRemoteBatchIsNotAborted(t *testing.T) {
	batch := &remoteBatchIntegrationFake{}
	msg := &Msg{RemoteWriteBatch: batch, remoteFinalized: true}
	if err := msg.markRemoteWriteIndeterminateLocked(ErrCommitIndeterminate); err != nil {
		t.Fatal(err)
	}
	if err := msg.finishRemoteWriteBatch(context.Background(), ErrCommitIndeterminate); err != nil {
		t.Fatal(err)
	}
	if !batch.indeterminate || batch.aborted || !batch.closed {
		t.Fatalf("indeterminate=%v aborted=%v closed=%v", batch.indeterminate, batch.aborted, batch.closed)
	}
}

// outcomeDeferringBatch 模拟 remoteentity 在 strict 确认超时后的批次：Commit 返回“结果未知”，
// 批次接手提交后回调，finalizer 拿到持久结论后调用一次。
type outcomeDeferringBatch struct {
	stagedRemoteBatch
	mu      sync.Mutex
	outcome func(bool)
	ids     []int64
}

// EntityIDs 是批次里的 Remote 实体（被拒绝时只丢弃它们的 Sync 内容与事实）。
func (b *outcomeDeferringBatch) EntityIDs() []int64 { return append([]int64(nil), b.ids...) }

func (b *outcomeDeferringBatch) Commit(context.Context) ([]entity.RemoteCommitReceipt, error) {
	return nil, fmt.Errorf("%w: %w", entity.ErrRemotePersistenceIndeterminate, context.DeadlineExceeded)
}

func (b *outcomeDeferringBatch) DeferUntilDurableOutcome(fn func(bool)) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.outcome = fn
	return true
}

func (b *outcomeDeferringBatch) takeOutcome() func(bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	fn := b.outcome
	b.outcome = nil
	return fn
}

// 本地已持久提交、strict 远端确认超时时，Sync Confirm 与 AfterCommit 不能丢，也不能在没有持久结论时执行：它们随批次交给
// finalizer，Committed 时经 Nest 本地执行入口在业务池执行一次；Rejected 时丢弃 Sync 门（不阻塞后续提交、不执行 AfterCommit）；
// 停机排空拿不到结论时门保持冻结。
// 历史：RR-20260926-37。
func TestStrictRemoteConfirmTimeoutDefersPostCommitToDurableOutcome(t *testing.T) {
	for _, mode := range []entitysync.SyncMode{entitysync.ModePeriodic, entitysync.ModeOnChange} {
		for _, conclusion := range []string{"committed", "rejected", "none"} {
			t.Run(mode.String()+"/"+conclusion, func(t *testing.T) {
				unique := int64(9960)
				switch conclusion {
				case "rejected":
					unique = 9964
				case "none":
					unique = 9968
				}
				if mode == entitysync.ModeOnChange {
					unique += 20
				}
				localID, e := newAsyncPilotEntity(t, unique, 10)
				m, frames, _ := syncTestManager(t, e, mode)
				getter := newMockGetter()
				remoteID := mustBuildCastID(t, unique+1, entity.EntityCategoryRemote, nestRemoteManagedKind)
				getter.Add(newMockEntity(remoteID, entity.EntityCategoryRemote))
				getter.Add(e)
				first := &outcomeDeferringBatch{ids: []int64{remoteID}}
				useFirst := true
				manager := &bindingRemoteManager{stagedRemoteManager: stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) {
					if useFirst {
						return first, nil
					}
					return &stagedRemoteBatch{}, nil
				}}}
				mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(manager), NestOptionWithEntitySync(m), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
				name := NewHandlerName(fmt.Sprintf("strict_confirm_outcome_%d", unique))
				var hookMu sync.Mutex
				afterCommit, afterCommitOnFast := 0, 0
				mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
					e.dao.Value++
					e.MarkSyncDirty(1)
					AfterCommit(func() {
						hookMu.Lock()
						afterCommit++
						if fctx.InShortWorker() {
							afterCommitOnFast++
						}
						hookMu.Unlock()
					})
					return "ok", nil
				}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
				if err := m.Start(context.Background()); err != nil {
					t.Fatal(err)
				}
				if err := mgr.Start(); err != nil {
					t.Fatal(err)
				}
				defer mgr.Shutdown(context.Background())
				counts := func() (int, int) {
					hookMu.Lock()
					defer hookMu.Unlock()
					return afterCommit, afterCommitOnFast
				}
				send := func() any {
					msg, ch := GenSyncMsg(MsgTypeMulti)
					msg.Name = name.String()
					msg.Tids = []int64{remoteID, localID}
					msg.Cost = true
					msg.HasRemote = true
					if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
						t.Fatal(err)
					}
					return stagedWait(t, ch)
				}
				if reply, ok := send().(error); !ok || reply == nil {
					t.Fatalf("tx1 reply=%v, want the unknown remote confirmation", reply)
				}
				if e.Sync().SyncCommitReady() {
					t.Fatal("Sync released without a durable remote conclusion")
				}
				if n, _ := counts(); n != 0 {
					t.Fatalf("AfterCommit ran %d time(s) without a durable remote conclusion", n)
				}
				outcome := first.takeOutcome()
				if outcome == nil {
					t.Fatalf("post-commit work (Sync Confirm, AfterCommit) was not handed to the remote outcome: local entity %d stays frozen forever", localID)
				}
				run := manager.localExecutor()
				if run == nil {
					t.Fatal("Nest did not bind its local runtime")
				}
				switch conclusion {
				case "committed":
					// finalizer 拿到 Applied/Committed：经 Nest 本地执行入口调用一次（非业务 worker 发起）。
					done := make(chan error, 1)
					go func() { done <- run(func() { outcome(true) }) }()
					if err := <-done; err != nil {
						t.Fatal(err)
					}
				case "rejected":
					done := make(chan error, 1)
					go func() { done <- run(func() { outcome(false) }) }()
					if err := <-done; err != nil {
						t.Fatal(err)
					}
				}
				n, onFast := counts()
				switch conclusion {
				case "committed":
					if n != 1 || onFast != 1 {
						t.Fatalf("AfterCommit after durable commit: ran=%d onFast=%d, want exactly once on a fast worker", n, onFast)
					}
					if !e.Sync().SyncCommitReady() {
						t.Fatal("Sync not confirmed after the durable commit")
					}
				default:
					if n != 0 {
						t.Fatalf("AfterCommit ran %d time(s) for a transaction without a durable commit", n)
					}
				}
				useFirst = false
				if reply := send(); reply != "ok" {
					t.Fatalf("tx2 reply=%v", reply)
				}
				if conclusion == "none" {
					if e.Sync().SyncCommitReady() {
						t.Fatal("Sync gate released during shutdown drain without a durable conclusion")
					}
					return
				}
				if !e.Sync().SyncCommitReady() {
					t.Fatalf("later transaction still frozen behind tx1 (%s)", conclusion)
				}
				// on_change 按提交逐笔冻结：先交付 tx1 冻结的 11（本地内存值），再交付 tx2 的 12；periodic 直接交付最新值。
				want := byte(e.dao.Value)
				deadline := time.After(2 * time.Second)
				for delivered := false; !delivered; {
					_ = m.Flush(context.Background())
					select {
					case raw := <-frames:
						delivered = syncValue(t, raw) == want
					case <-deadline:
						t.Fatalf("value %d never delivered after a later successful transaction (%s)", want, conclusion)
					}
				}
			})
		}
	}
}

// SyncMutation.Reject：已准入提交的持久结论为拒绝时，门不再阻塞后续提交，本提交的兴趣事实按丢弃处理，后续提交的事实不受影响。
func TestSyncMutationRejectReleasesLaterCommits(t *testing.T) {
	_, e := newAsyncPilotEntity(t, 9990, 10)
	m, _, _ := syncTestManager(t, e, entitysync.ModePeriodic)
	es := []entity.IThreadSafeEntity{e}
	first := entity.BeginSyncMutation(es, m)
	firstFacts := e.Sync().SyncCommitCondition()
	e.MarkSyncDirty(1)
	first.Admit()
	first.Release()
	first.Finish(false)
	second := entity.BeginSyncMutation(es, m)
	secondFacts := e.Sync().SyncCommitCondition()
	e.MarkSyncDirty(1)
	second.Admit()
	second.Release()
	second.Confirm()
	second.Finish(false)
	if e.Sync().SyncCommitReady() {
		t.Fatal("second commit passed the unconfirmed first gate")
	}
	first.Reject()
	if !e.Sync().SyncCommitReady() {
		t.Fatal("rejected first commit still blocks the second")
	}
	if ready, discarded := firstFacts(); ready || !discarded {
		t.Fatalf("facts of the rejected commit ready=%v discarded=%v, want dropped", ready, discarded)
	}
	if ready, discarded := secondFacts(); !ready || discarded {
		t.Fatalf("facts of the later commit ready=%v discarded=%v, want ready", ready, discarded)
	}
	third := entity.BeginSyncMutation(es, m)
	thirdFacts := e.Sync().SyncCommitCondition()
	third.Admit()
	third.Release()
	third.Confirm()
	third.Finish(false)
	if ready, discarded := thirdFacts(); !ready || discarded {
		t.Fatalf("a commit that began after the rejection ready=%v discarded=%v, want ready", ready, discarded)
	}
}

// strictConfirmTimeoutBatch 模拟 remoteentity 在 Durability 2 下确认等待超时：本地事务已持久，
// Remote 结果未知（remoteWriteBatch.Commit 标 indeterminate 后返回错误，Close 交给 finalizer）。
type strictConfirmTimeoutBatch struct{ closeFailBatch }

func (b *strictConfirmTimeoutBatch) Commit(context.Context) ([]entity.RemoteCommitReceipt, error) {
	return nil, fmt.Errorf("%w: %w", entity.ErrRemotePersistenceIndeterminate, context.DeadlineExceeded)
}

// 提交之后发生的释放 / 回调错误必须带 ErrAfterCommitFailed（保留原因的 errors.Is 链），未提交（Abort）或结果未知的回复不得带该哨兵。
// 历史：RR-20260926-46。
func TestRemoteReplyDistinguishesCommittedFromUncommitted(t *testing.T) {
	for _, tc := range []struct {
		name      string
		unique    int64
		batch     func() entity.RemoteWriteBatch
		fail      bool
		committed bool
		cause     error
	}{
		{name: "committed then close failed", unique: 9420, batch: func() entity.RemoteWriteBatch { return &closeFailBatch{} }, committed: true, cause: entity.ErrRemoteReleaseIncomplete},
		{name: "handler failed then close failed", unique: 9430, batch: func() entity.RemoteWriteBatch { return &closeFailBatch{} }, fail: true, cause: entity.ErrRemoteReleaseIncomplete},
		{name: "remote confirmation unknown", unique: 9440, batch: func() entity.RemoteWriteBatch { return &strictConfirmTimeoutBatch{} }, cause: entity.ErrRemotePersistenceIndeterminate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			localID, e := newAsyncPilotEntity(t, tc.unique, 10)
			getter := newMockGetter()
			remoteID := mustBuildCastID(t, tc.unique+1, entity.EntityCategoryRemote, nestRemoteManagedKind)
			getter.Add(newMockEntity(remoteID, entity.EntityCategoryRemote))
			getter.Add(e)
			batch := tc.batch()
			manager := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) { return batch, nil }}
			mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(manager), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
			name := NewHandlerName(fmt.Sprintf("remote_sentinel_%d", tc.unique))
			mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				if tc.fail {
					return nil, errors.New("business refused")
				}
				return "ok", nil
			}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer mgr.Shutdown(context.Background())
			msg, ch := GenSyncMsg(MsgTypeMulti)
			msg.Name, msg.Tids, msg.Cost, msg.HasRemote = name.String(), []int64{remoteID, localID}, true, true
			if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
				t.Fatal(err)
			}
			reply := stagedWait(t, ch)
			err, _ := reply.(error)
			if err == nil {
				t.Fatalf("reply=%v, want an error", reply)
			}
			if !errors.Is(err, tc.cause) {
				t.Fatalf("reply=%v lost its cause %v", err, tc.cause)
			}
			if got := errors.Is(err, ErrAfterCommitFailed); got != tc.committed {
				t.Fatalf("reply=%v: errors.Is(ErrAfterCommitFailed)=%v, want %v (committed=%v)", err, got, tc.committed, tc.committed)
			}
		})
	}
}

func TestRemoteAfterAdmissionPanicDoesNotAbortDurableCommit(t *testing.T) {
	localID, e := newAsyncPilotEntity(t, 9960, 10)
	getter := newMockGetter()
	remoteID := mustBuildCastID(t, 9961, entity.EntityCategoryRemote, nestRemoteManagedKind)
	getter.Add(newMockEntity(remoteID, entity.EntityCategoryRemote))
	getter.Add(e)
	batch := &stagedRemoteBatch{}
	var committed bool
	batch.commit = func() { committed = true }
	manager := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) { return batch, nil }}
	committer := &recordingCommitter{}
	mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(manager), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
	name := NewHandlerName("remote_committed_hook_panic")
	mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		old := e.dao.Value
		RecordUndo(e.dao, 1, func() error { e.dao.Value = old; return nil })
		e.dao.Value++
		if err := MarkPersist(e.dao, 1); err != nil {
			return nil, err
		}
		CurrentRollbackTx().AfterAdmission(func() { panic("lifecycle finalizer panic") })
		return "ok", nil
	}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())
	msg, ch := GenSyncMsg(MsgTypeMulti)
	msg.Name = name.String()
	msg.Tids = []int64{remoteID, localID}
	msg.Cost = true
	msg.HasRemote = true
	if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
		t.Fatal(err)
	}
	got := stagedWait(t, ch)
	t.Logf("reply=%v", got)
	t.Logf("WAL record committed: mutations=%d; local value=%d; remote batch Commit called=%v Abort called=%v", len(committer.record.Mutations), e.dao.Value, committed, batch.aborted.Load())
	if len(committer.record.Mutations) == 0 || batch.aborted.Load() || !committed {
		t.Errorf("remote batch aborted after the transaction was durably committed")
	}
}

func TestPostRemoteCommitRunsAllCallbacksAfterPanic(t *testing.T) {
	msg := &Msg{}
	called := 0
	msg.addPostRemoteCommit(func() { panic("after commit") }, func() { called++ })
	if err := msg.runPostRemoteCommit(); !errors.Is(err, ErrAfterCommitFailed) {
		t.Fatalf("callback error=%v", err)
	}
	if called != 1 {
		t.Fatal("panic skipped framework completion")
	}
	if err := msg.runPostRemoteCommit(); err != nil || called != 1 {
		t.Fatal("callbacks repeated")
	}
}

// 引擎 fence 之后，带 Remote 批次、没有 effect 的 memory handler 的 Remote 直写同样被拒绝：回复满足 errors.Is ErrNestFenced
// 与 ErrCommitRejected，Remote 批次没有 Commit、至少一次 Abort，committer 不被调用；RollbackNone 的内存修改不撤销。
// 历史：RR-20260930-12。
func TestFencedMemoryRemoteWithoutEffectIsRefused(t *testing.T) {
	localID, e := newAsyncPilotEntity(t, 49700, 10)
	getter := newMockGetter()
	remoteID := mustBuildCastID(t, 49701, entity.EntityCategoryRemote, nestRemoteManagedKind)
	getter.Add(newMockEntity(remoteID, entity.EntityCategoryRemote))
	getter.Add(e)
	recording := &recordingRemoteBatch{}
	manager := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) { return recording, nil }}
	committer := &recordsCommitter{}
	var mgr *NestMgr
	mgr = NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(manager), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
	name := NewHandlerName("rr20260930_12_memory_remote_no_effect")
	mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		e.dao.Value = 77
		_ = MarkPersist(e.dao, 1)
		// 没有 Emit：记录不交给 committer。别的原因 fence 引擎（模拟另一条消息结果未知）。
		mgr.Fence(errors.New("rr12: fenced by another message"))
		return "ok", nil
	}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Shutdown(context.Background()) }()
	msg, ch := GenSyncMsg(MsgTypeMulti)
	msg.Name, msg.Tids, msg.Cost, msg.HasRemote = name.String(), []int64{remoteID, localID}, true, true
	if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
		t.Fatal(err)
	}
	reply := stagedWait(t, ch)
	err, _ := reply.(error)
	t.Logf("reply=%v committer=%d remote commits=%d aborts=%d value=%d", reply, committer.count(), recording.commits.Load(), recording.aborts.Load(), e.dao.Value)
	if c := recording.commits.Load(); c != 0 {
		t.Fatalf("Remote batch was committed %d time(s) after the engine was fenced: a Durability 0 write reached the authority behind the fence", c)
	}
	if !errors.Is(err, ErrNestFenced) || !errors.Is(err, ErrCommitRejected) {
		t.Fatalf("reply=%v, want errors.Is ErrNestFenced and ErrCommitRejected (decision table row 12)", reply)
	}
	for _, committed := range []error{ErrCommitIndeterminate, ErrAfterCommitFailed, ErrNestedTransactionCommitted, ErrRemotePartRejected} {
		if errors.Is(err, committed) {
			t.Fatalf("reply %v must not claim a (possible) commit via %v", err, committed)
		}
	}
	if a := recording.aborts.Load(); a == 0 {
		t.Fatal("Remote batch was not aborted")
	}
	if n := committer.count(); n != 0 {
		t.Fatalf("committer received %d record(s)", n)
	}
	if e.dao.Value != 77 {
		t.Fatalf("declared entity value=%d, want 77: RollbackNone does not undo in-memory changes", e.dao.Value)
	}
}
