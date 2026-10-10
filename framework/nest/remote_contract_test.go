package nest

import (
	"context"
	"errors"
	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/remoteentity"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
			mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(remote), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}))
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
			defer owner.RegisterOnEntityRelease(func(entity.IThreadSafeEntity) {
				if armed {
					armed = false
					panic(errors.New("release hook failed"))
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
			mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(manager), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}))
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
			if replyErr == nil || !strings.Contains(replyErr.Error(), "release hook failed") {
				t.Fatalf("post-commit hook failure must be reported, reply=%v", got)
			}
			if e.dao.Value != 11 {
				t.Fatalf("committed local value rolled back: %d", e.dao.Value)
			}
		})
	}
}
