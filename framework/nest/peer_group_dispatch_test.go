package nest

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/internal/entitypermit"
)

// 同时在 getter 的显式目标表、真实 EntityManager 组索引登记，避免用仅设置 Base 的替身伪造闭包。
func peerDispatchFixture(t *testing.T, groups ...int64) (*NestMgr, []*rollbackTestEntity) {
	t.Helper()
	g := newMockGetter()
	var es []*rollbackTestEntity
	for i, group := range groups {
		id := mustBuildCastID(t, int64(27000+i), 1, nestLocalKind)
		e := &rollbackTestEntity{EntityBase: entity.NewEntityBase(id, 1, false, nestLocalKind), dao: &rollbackTestDao{id: id, Value: 10}}
		e.Base().SetGroupLockIDForTest(entitypermit.System(), group)
		if err := g.groups.TryAdd(e); err != nil {
			t.Fatal(err)
		}
		g.Add(e)
		es = append(es, e)
	}
	return &NestMgr{getter: g, committer: &recordingCommitter{}}, es
}

type peerDispatchSyncObserver struct {
	captured []int64
	leaked   bool
}

func (*peerDispatchSyncObserver) BindSyncProducer() {}
func (*peerDispatchSyncObserver) WakeSync()         {}
func (o *peerDispatchSyncObserver) CaptureSync(s *entity.SubjectSyncState) error {
	o.captured = append(o.captured, s.SubjectID())
	o.leaked = o.leaked || CurrentEntityLockGroup() != nil
	return nil
}

func TestPeerDispatchEnlistsImplicitDAO(t *testing.T) {
	mgr, entities := peerDispatchFixture(t, 71, 71, 0, 72)
	a, b, ungrouped, other := entities[0], entities[1], entities[2], entities[3]
	observer := &peerDispatchSyncObserver{}
	mgr.entitySync = observer
	b.SetSyncState(entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{Enabled: true, SubjectID: b.GUId()}))
	biz := errors.New("roll back peer")
	for _, fail := range []bool{true, false} {
		var scope *EntityLockGroupScope
		entry := handlerEntry{meta: HandlerMeta{Durability: DurabilityStrict, Rollback: RollbackState}, handler: func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
			if !slices.Equal(es, []entity.IThreadSafeEntity{a, ungrouped, a}) {
				t.Fatalf("changed handler parameters: %v", es)
			}
			scope = CurrentEntityLockGroup()
			v, err := GetGroupMember[*rollbackTestEntity](b.GUId())
			if err != nil || v != b {
				t.Fatalf("missing peer: %v", err)
			}
			if _, err := GetGroupMember[*mockEntity](b.GUId()); !errors.Is(err, ErrGroupMemberUnavailable) {
				t.Fatal("wrong type did not fail")
			}
			if scope.Get(other.GUId()) != nil || scope.Get(ungrouped.GUId()) != nil {
				t.Fatal("leaked non-member")
			}
			var ids []int64
			scope.Range(func(e entity.IThreadSafeEntity) bool { ids = append(ids, e.GUId()); return true })
			if !slices.Equal(ids, []int64{a.GUId(), b.GUId()}) {
				t.Fatalf("range: %v", ids)
			}
			blocked := make(chan bool, 1)
			go func() {
				returned := false
				scope.Range(func(entity.IThreadSafeEntity) bool { returned = true; return true })
				blocked <- (!returned && scope.Get(b.GUId()) == nil)
			}()
			if !<-blocked {
				t.Fatal("scope crossed goroutine")
			}
			if err := entity.WithGuardScope(entitypermit.System(), "nested", func(*entity.GuardScope) error {
				if scope.Get(b.GUId()) != nil || CurrentEntityLockGroup() != nil {
					t.Fatal("scope crossed nested guard")
				}
				return nil
			}); err != nil {
				return nil, err
			}
			v.Sync().MarkDirty(1)
			v.dao.Value = 33
			if err := MarkPersist(v.dao, 1); err != nil {
				return nil, err
			}
			if fail {
				return nil, biz
			}
			return nil, nil
		}}
		err := entity.WithGuardScope(entitypermit.System(), "peer-dao", func(*entity.GuardScope) error {
			_, err := mgr.dispatchMany(entry, "peer-dao", []int64{a.GUId(), ungrouped.GUId(), a.GUId()}, nil)
			return err
		})
		if scope == nil || scope.Get(b.GUId()) != nil || CurrentEntityLockGroup() != nil {
			t.Fatal("expired scope remained active")
		}
		if fail {
			if !errors.Is(err, biz) || b.dao.Value != 10 || b.dao.Tracker.Dirty() || len(observer.captured) != 0 || b.Sync().PendingDirty() {
				t.Fatalf("rollback err=%v value=%v dirty=%v", err, b.dao.Value, b.dao.Tracker.Dirty())
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(observer.captured, []int64{b.GUId()}) || observer.leaked || !b.Sync().SyncCommitReady() {
			t.Fatalf("implicit Sync capture=%v leaked=%v ready=%v", observer.captured, observer.leaked, b.Sync().SyncCommitReady())
		}
		record := mgr.committer.(*recordingCommitter).record
		if b.dao.Value != 33 || len(record.Mutations) != 1 || record.Mutations[0].Key.ID != b.GUId() {
			t.Fatalf("implicit peer not in commit: %+v", record)
		}
	}
}

func TestPeerDispatchCrossGroupMultiIsNotAnUnqualifiedRead(t *testing.T) {
	mgr, es := peerDispatchFixture(t, 5, 5, 3, 3, 0)
	entry := handlerEntry{meta: HandlerMeta{Durability: DurabilityMemory}, handler: func(actual []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		if !slices.Equal(actual, []entity.IThreadSafeEntity{es[2], es[0], es[4]}) {
			t.Fatal("changed multi parameters")
		}
		if CurrentEntityLockGroup() != nil {
			t.Fatal("ambiguous scope")
		}
		for _, e := range es {
			if !entity.CurrentGuardScope().Guard().GuardedEntity(e) {
				t.Fatalf("not guarded: %d", e.GUId())
			}
			if _, err := GetGroupMember[*rollbackTestEntity](e.GUId()); !errors.Is(err, ErrGroupMemberUnavailable) {
				t.Fatal("ambiguous read")
			}
		}
		return nil, nil
	}}
	err := entity.WithGuardScope(entitypermit.System(), "cross-group", func(*entity.GuardScope) error {
		_, err := mgr.dispatchMany(entry, "cross-group", []int64{es[2].GUId(), es[0].GUId(), es[4].GUId()}, nil)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// commit 内的用户钩子已经不在业务代码内。特别是嵌套事务准备/提交时 CurrentRollbackTx 可能又是外层或 nil。
type peerDispatchCheckCommitter struct{ check func() }

func (c peerDispatchCheckCommitter) Commit(context.Context, CommitRecord) error {
	c.check()
	return nil
}

func TestPeerDispatchBlocksNestedTransactionThroughCommit(t *testing.T) {
	mgr, es := peerDispatchFixture(t, 21, 21)
	a, b := es[0], es[1]
	for _, withMsg := range []bool{true, false} {
		entry := handlerEntry{meta: HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory}, handler: func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
			scope := CurrentEntityLockGroup()
			if scope.Get(b.GUId()) != b {
				t.Fatal("no outer access")
			}
			check := func() {
				if scope.Get(b.GUId()) != nil {
					t.Errorf("nested raw pointer, withMsg=%v", withMsg)
				}
			}
			called := false
			_, err := RunIsolatedTransaction(context.Background(), peerDispatchCheckCommitter{func() { called = true; check() }}, "isolated", func() (any, error) { check(); return nil, addPeerTestMutation(CurrentRollbackTx(), b.GUId()) })
			if err != nil {
				return nil, err
			}
			if !called || scope.Get(b.GUId()) != b {
				t.Fatal("outer scope not restored")
			}
			return nil, nil
		}}
		err := entity.WithGuardScope(entitypermit.System(), "nested-commit", func(*entity.GuardScope) error {
			if withMsg {
				defer pushCurrentNestDispatchMsg(&Msg{Type: MsgTypeSingle, Tid: a.GUId()})()
			}
			_, err := mgr.dispatchMany(entry, "nested-commit", []int64{a.GUId()}, nil)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// 模拟老版本或定制的组索引；测试不依赖 EntityManager 是否允许新建这种非法成员。
type peerTestLegacyIndex struct {
	*mockGetter
	extra entity.IThreadSafeEntity
}

func (g *peerTestLegacyIndex) GetGroupEntities(id int64) []entity.IThreadSafeEntity {
	es := g.mockGetter.GetGroupEntities(id)
	if g.extra.Base().GroupLockID() == id {
		return append(es, g.extra)
	}
	return es
}

func (g *peerTestLegacyIndex) GetGroupEntity(groupID, id int64) entity.IThreadSafeEntity {
	if g.extra.Base().GroupLockID() == groupID && g.extra.GUId() == id {
		return g.extra
	}
	return g.mockGetter.GetGroupEntity(groupID, id)
}

// 即使基础索引来自旧版本、放进不合法的 Remote 或已绑定伴生对端，也不能借出原始指针。
func TestPeerDispatchRejectsUnsafeCombinations(t *testing.T) {
	mgr, es := peerDispatchFixture(t, 91)
	g := mgr.getter.(*mockGetter)
	id := mustBuildCastID(t, 27704, entity.EntityCategoryRemote, nestRemoteCapableKind)
	remote := newMockEntityWithKind(id, entity.EntityCategoryRemote, nestRemoteCapableKind)
	remote.Base().SetGroupLockIDForTest(entitypermit.System(), 91)
	mgr.getter = &peerTestLegacyIndex{mockGetter: g, extra: remote}
	entry := handlerEntry{handler: func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		t.Fatal("unsafe composition entered handler")
		return nil, nil
	}}
	check := func(m *NestMgr, ids ...int64) {
		err := entity.WithGuardScope(entitypermit.System(), "reject-peer", func(*entity.GuardScope) error { _, err := m.dispatchMany(entry, "reject-peer", ids, nil); return err })
		if !errors.Is(err, ErrPeerGroupUnsupported) {
			t.Fatalf("unsafe combination: %v", err)
		}
	}
	check(mgr, es[0].GUId())
	cf := newCompNestFixture(t, &recordingCommitter{})
	// 两种域各自合法也不等于可以组合：显式的普通组 + 已绑定的未分组伴生目标。
	localID := mustBuildCastID(t, 27705, 1, nestLocalKind)
	local := newMockEntityWithKind(localID, 1, nestLocalKind)
	local.Base().SetGroupLockIDForTest(entitypermit.System(), 92)
	cf.em.Add(local)
	check(cf.NestMgr, localID, cf.a.GUId())
}

func TestPeerDispatchRevalidatesAnchorAfterCallback(t *testing.T) {
	mgr, es := peerDispatchFixture(t, 45, 45)
	entry := handlerEntry{meta: HandlerMeta{Durability: DurabilityMemory}, handler: func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		s := CurrentEntityLockGroup()
		if s.Get(es[1].GUId()) != es[1] {
			t.Fatal("missing initial peer")
		}
		es[0].Base().SetGroupLockIDForTest(entitypermit.System(), 46)
		es[0].Base().SetGroupLockIDForTest(entitypermit.System(), 45) // 造成 ABA：不能只验组 ID。
		if s.Get(es[1].GUId()) != nil {
			t.Fatal("lost explicit anchor but obtained peer")
		}
		return nil, nil
	}}
	if err := entity.WithGuardScope(entitypermit.System(), "anchor-aba", func(*entity.GuardScope) error {
		_, err := mgr.dispatchMany(entry, "anchor-aba", []int64{es[0].GUId()}, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func addPeerTestMutation(tx *RollbackTx, id int64) error {
	// 明确一笔独立记录，空事务不会进入 committer 的 Commit 钩子。
	dao := &rollbackTestDao{id: id}
	mutation, err := dao.PrepareMutation(PersistChange{Mask: 1})
	if err != nil {
		return err
	}
	return tx.AddMutation(mutation)
}
