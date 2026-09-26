package policy

import (
	"context"
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/spatial"
)

// RR-20260926-58（REPRO-2026-09-26-05 §7 改写）：一笔 Nest 事务同时修改本地实体 2 与 Remote 实体 3。本地部分已持久提交，
// Remote 部分之后被权威持久拒绝，finalizer 按 Remote 批次的实体 ID 调用 RejectEntities。本地实体的内存保留新位置
// （没有任何路径回滚它），它排队的移动事实必须照常应用到 AOI；只有被拒绝的 Remote 实体的事实被丢弃。
func TestRemoteRejectKeepsCommittedLocalInterestFact(t *testing.T) {
	m := newManager(t)
	in := newInterest(t, m, "team")
	player(t, m, 1)
	player(t, m, 2)
	player(t, m, 3)
	for id, x := range map[int64]int64{1: 10, 2: 20, 3: 30} {
		if err := in.Enter(id, spatial.Point{X: x, Y: 10}); err != nil {
			t.Fatal(err)
		}
	}
	mustApply(t, in)
	if got := len(in.Visible(1)); got != 2 {
		t.Fatalf("setup: observer 1 sees %d subject(s), want 2", got)
	}
	local := &queuedEntity{entity.NewEntityBase(2, entity.EntityCategory(4), true)}
	local.SetSyncState(subjectState(t, 2))
	remote := &queuedEntity{entity.NewEntityBase(3, entity.EntityCategory(4), true)}
	remote.SetSyncState(subjectState(t, 3))
	mutation := entity.BeginSyncMutation([]entity.IThreadSafeEntity{local, remote}, m)
	if err := in.QueueMove(local, spatial.Point{X: 900, Y: 900}, true); err != nil {
		t.Fatal(err)
	}
	if err := in.QueueMove(remote, spatial.Point{X: 900, Y: 900}, true); err != nil {
		t.Fatal(err)
	}
	mutation.Admit()
	mutation.Release()
	// Remote 确认无结论 → finalizer 拿到持久拒绝：只拒绝 Remote 批次里的实体。
	mutation.RejectEntities([]int64{remote.ID()})
	if err := m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !local.Sync().SyncCommitReady() || !remote.Sync().SyncCommitReady() {
		t.Fatalf("gates still frozen: local=%v remote=%v", local.Sync().SyncCommitReady(), remote.Sync().SyncCommitReady())
	}
	visible := in.Visible(1)
	if slices.Contains(visible, int64(2)) {
		t.Fatalf("local entity 2 moved to (900,900) in durably committed memory, but its AOI move was discarded by the Remote rejection: observer 1 still sees %v", visible)
	}
	if !slices.Contains(visible, int64(3)) {
		t.Fatalf("rejected Remote entity 3's move was applied: observer 1 sees %v, want 3 still in range", visible)
	}
}

// 没有 SubjectSyncState 的实体只能经 SyncConditionFor 的回退路径看整笔提交：RejectEntities 之后按实体 ID 判断，
// 被拒绝的 Remote 实体的事实丢弃，已提交的本地实体的事实就绪。
func TestRejectEntitiesJudgesEntitiesWithoutSyncStateByID(t *testing.T) {
	m := newManager(t)
	_, closeScope := entity.NewGuardScope("reject-entities")
	defer closeScope()
	local := &queuedEntity{entity.NewEntityBase(2, entity.EntityCategory(4), true)}
	remote := &queuedEntity{entity.NewEntityBase(3, entity.EntityCategory(4), true)}
	subject := &queuedEntity{entity.NewEntityBase(4, entity.EntityCategory(4), true)}
	subject.SetSyncState(subjectState(t, 4))
	mutation := entity.BeginSyncMutation([]entity.IThreadSafeEntity{subject}, m)
	localFacts, remoteFacts := entity.SyncConditionFor(local), entity.SyncConditionFor(remote)
	mutation.Admit()
	mutation.Release()
	if ready, _ := localFacts(); ready {
		t.Fatal("facts ready before any durable conclusion")
	}
	mutation.RejectEntities([]int64{remote.ID()})
	mutation.Finish(false)
	if ready, discarded := localFacts(); !ready || discarded {
		t.Fatalf("committed local entity facts ready=%v discarded=%v, want ready", ready, discarded)
	}
	if ready, discarded := remoteFacts(); ready || !discarded {
		t.Fatalf("rejected Remote entity facts ready=%v discarded=%v, want discarded", ready, discarded)
	}
	if !subject.Sync().SyncCommitReady() {
		t.Fatal("committed entity gate still frozen")
	}
}

// 纯 Remote 事务被拒绝（批次覆盖 mutation 的全部实体）：行为与 Reject 相同——门不再阻塞、本提交的事实全部丢弃。
func TestPureRemoteRejectDiscardsItsInterestFacts(t *testing.T) {
	m := newManager(t)
	in := newInterest(t, m, "team")
	player(t, m, 1)
	player(t, m, 3)
	if err := in.Enter(1, spatial.Point{X: 10, Y: 10}); err != nil {
		t.Fatal(err)
	}
	if err := in.Enter(3, spatial.Point{X: 30, Y: 10}); err != nil {
		t.Fatal(err)
	}
	mustApply(t, in)
	remote := &queuedEntity{entity.NewEntityBase(3, entity.EntityCategory(4), true)}
	remote.SetSyncState(subjectState(t, 3))
	mutation := entity.BeginSyncMutation([]entity.IThreadSafeEntity{remote}, m)
	if err := in.QueueMove(remote, spatial.Point{X: 900, Y: 900}, true); err != nil {
		t.Fatal(err)
	}
	mutation.Admit()
	mutation.Release()
	mutation.RejectEntities([]int64{remote.ID()})
	if err := m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !remote.Sync().SyncCommitReady() {
		t.Fatal("rejected commit still blocks the gate")
	}
	if visible := in.Visible(1); !slices.Contains(visible, int64(3)) {
		t.Fatalf("rejected Remote move was applied: observer 1 sees %v", visible)
	}
}
