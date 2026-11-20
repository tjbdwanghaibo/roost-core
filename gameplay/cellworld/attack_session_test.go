package cellworld

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 走公开 API 与正常的 Nest handler；错误不能同时泄露准入回执。
func checkSessionAttack(t *testing.T, n *Node, base int64, s BaseSession, target, requestID int64, wantErr error) HitID {
	t.Helper()
	hit, err := n.AttackSession(context.Background(), base, s, target, 7, requestID)
	if !errors.Is(err, wantErr) || (err != nil && hit != (HitID{})) || (err == nil && hit.ID == "") {
		t.Fatalf("AttackSession(%s, %d, %d) = %+v, %v; want %v", s.ConnectionID, target, requestID, hit, err, wantErr)
	}
	return hit
}

func TestAttackSessionTakeoverRollbackAndNextEntry(t *testing.T) {
	const u = 41
	p := newNumberedTestPlayer(u)
	old := BaseSession{"account", "tcp-A", 1}
	next := BaseSession{"account", "tcp-B", 2}
	var inactive atomic.Bool
	spec, lb := loopbackSpec(t, 1, 1)
	spec.baseMod = func(cfg *NodeConfig) {
		cfg.ReconnectGrace = time.Minute
		cfg.SessionActive = func(id int64, conn string) bool {
			return id == p.ID() && (conn == old.ConnectionID || conn == next.ConnectionID && !inactive.Load())
		}
	}
	var sent atomic.Int32
	lb.setFault(func(_ string, m *wireMsg) (bool, bool, time.Duration) {
		if m.Kind == kindAttack {
			sent.Add(1)
		}
		return false, false, 0
	})
	w := startWorld(t, spec)
	w.base.manager.Add(p)
	if err := w.base.sync.OpenSession(u); err != nil {
		t.Fatal(err)
	}
	n, ctx, cell, target := w.base.node, context.Background(), CellID{}, npcID(1)
	w.spawn(cell, EntityState{ID: target, X: 550, Y: 500, HP: 100})
	admit := func(s BaseSession, entry uint64) {
		t.Helper()
		st, err := n.AdmitSession(ctx, p.ID(), actorID(u), spatial.Point{X: 500, Y: 500}, 100, s)
		if err != nil || st.Entry != entry || st.Session.BaseSession != s {
			t.Fatalf("admit %s: entry=%d session=%+v err=%v", s.ConnectionID, st.Entry, st.Session, err)
		}
	}
	attack := func(s BaseSession, req int64, wantErr error) HitID {
		t.Helper()
		return checkSessionAttack(t, n, p.ID(), s, target, req, wantErr)
	}
	admit(old, 1)
	w.waitView(u, settleWithin, "owner", func(v View) bool { return v.Cell == w.grid.SID(cell) })
	first := attack(old, 1, nil)
	// 只读回放：命中取回原回执；未命中的新号按会话失效拒绝，不出手、不推进序号和账本。
	before, err := n.BaseStateOf(ctx, p.ID())
	if err != nil {
		t.Fatal(err)
	}
	high := p.ledger.v.high
	if got, err := n.AttackReceiptSession(ctx, p.ID(), old, target, 1); err != nil || got != first {
		t.Fatalf("receipt replay = %+v, %v; want %+v", got, err, first)
	}
	for _, req := range []int64{2, 0} {
		if got, err := n.AttackReceiptSession(ctx, p.ID(), old, target, req); !errors.Is(err, ErrStaleSession) || got != (HitID{}) {
			t.Fatalf("replay-only request %d = %+v, %v; want ErrStaleSession", req, got, err)
		}
	}
	if st, err := n.BaseStateOf(ctx, p.ID()); err != nil || st.AttackSeq != before.AttackSeq || p.ledger.v.high != high {
		t.Fatalf("replay-only attacked: seq %d->%d high %d->%d err=%v", before.AttackSeq, st.AttackSeq, high, p.ledger.v.high, err)
	}
	admit(next, 1) // 两条 TCP 连接仍然活跃，仅玩家绑定发生接替。
	const collide = 1 + AttackRequestWindow
	reads := p.reads.Load()
	for _, req := range []int64{1, collide, 0} {
		attack(old, req, ErrStaleSession)
	}
	if p.reads.Load() != reads {
		t.Fatal("replaced session read the ledger")
	}
	// 覆盖同一槽时故意令提交失败。原槽、高水位、序号和提交后发件必须一起回滚。
	p.rejectNext.Store(true)
	attack(next, collide, errRejectSessionCommit)
	if got := attack(next, 1, nil); got != first {
		t.Fatalf("original receipt after rollback = %+v, want %+v", got, first)
	}
	second := attack(next, collide, nil)
	third := attack(next, 0, nil)
	if second.Seq != first.Seq+1 || third.Seq != second.Seq+1 {
		t.Fatalf("unexpected hit sequences: %d, %d, %d", first.Seq, second.Seq, third.Seq)
	}
	if results := w.waitResults(3, settleWithin); len(results) != 3 {
		t.Fatalf("want three attack results, got %+v", results)
	}
	if err := n.Leave(ctx, p.ID()); err != nil {
		t.Fatal(err)
	}
	attack(old, collide, ErrStaleSession) // 已离场但仍有 Admission，绝非冷恢复。
	// 旧服务端入口重建下一次 Entry，也不能丢掉已经提交的 Admission 高水位。
	st, err := n.EnsureEntered(ctx, p.ID(), actorID(u), spatial.Point{X: 500, Y: 500}, 100)
	if err != nil || st.Entry != 2 || st.Session.BaseSession != next {
		t.Fatalf("next entry lost the binding: entry=%d session=%+v err=%v", st.Entry, st.Session, err)
	}
	if got := attack(next, collide, nil); got != second {
		t.Fatalf("next entry returned %+v, want %+v", got, second)
	}
	inactive.Store(true) // 绑定仍当前，但传输层已不活跃。
	attack(next, collide, ErrStaleSession)
	inactive.Store(false)
	if err := n.DisconnectSession(ctx, p.ID(), next.SessionID, next.ConnectionID); err != nil {
		t.Fatal(err)
	}
	eventually(t, settleWithin, "offline marker", func() (bool, string) {
		st := w.baseState(u)
		return st.Session.OfflineAt != 0, fmt.Sprint(st.Session)
	})
	attack(next, collide, ErrStaleSession) // 即使传输表仍报活，离线标记也应拒绝。
	deadline, cancel := context.WithTimeout(ctx, settleWithin)
	defer cancel()
	if err := n.Drain(deadline); err != nil {
		t.Fatal(err)
	}
	apps := w.applications()
	if sent.Load() != 3 || len(apps) != 3 || apps[first] != 1 || apps[second] != 1 || apps[third] != 1 {
		t.Fatalf("sent=%d Cell applications=%v", sent.Load(), apps)
	}
	// Inspect 读区域镜像，要等 NPC 的心跳把已经提交的 HP 报到区域。
	eventually(t, settleWithin, "target HP 79 after three hits", func() (bool, string) {
		snap, ok := w.roleOf(cell, target)
		return ok && snap.State.HP == 79, fmt.Sprint(snap)
	})
}

func TestAttackSessionColdBaseReceiptOnly(t *testing.T) {
	// 模拟恢复的持久槽；不恢复绑定和路由，实际文件 WAL 的回放由 demo 集成测试负责。
	const req = AttackRequestWindow + 1
	p := newNumberedTestPlayer(51)
	target := npcID(12)
	hit := HitID{Attacker: actorID(51), Seq: 3, ID: "from-wal"}
	p.ledger.v.high = req
	p.ledger.v.slots[req%AttackRequestWindow] = AttackReceipt{RequestID: req, Target: target, Hit: hit}
	before := p.ledger.v
	live := BaseSession{"account", "tcp-live", 1}
	proc := startProc(t, procSpec{sid: 201, base: true, grid: worldGrid(t, 1, 1), transport: newLoopback(), mod: func(cfg *NodeConfig) {
		cfg.SessionActive = func(id int64, conn string) bool { return id == p.ID() && conn == live.ConnectionID }
	}})
	defer proc.stop()
	proc.manager.Add(p)
	n := proc.node
	checkSessionAttack(t, n, p.ID(), BaseSession{}, target, req, ErrStaleSession)
	checkSessionAttack(t, n, p.ID(), BaseSession{"other", "tcp-other", 2}, target, req, ErrStaleSession)
	checkSessionAttack(t, n, p.ID(), live, target, 0, ErrNotInWorld)
	checkSessionAttack(t, n, p.ID(), live, target, -1, ErrAttackRequestInvalid)
	if p.reads.Load() != 0 {
		t.Fatal("cold Base exposed the ledger without an authenticated numbered request")
	}
	if got := checkSessionAttack(t, n, p.ID(), live, target, req, nil); got != hit {
		t.Fatalf("recovered receipt = %+v, want %+v", got, hit)
	}
	checkSessionAttack(t, n, p.ID(), live, target+1, req, ErrAttackRequestConflict)
	checkSessionAttack(t, n, p.ID(), live, target, 1, ErrAttackRequestExpired)
	checkSessionAttack(t, n, p.ID(), live, target, req+1, ErrNotInWorld)
	if got, err := n.AttackReceiptSession(context.Background(), p.ID(), live, target, req); err != nil || got != hit {
		t.Fatalf("cold replay-only receipt = %+v, %v; want %+v", got, err, hit)
	}
	if _, err := n.AttackReceiptSession(context.Background(), p.ID(), live, target, req+1); !errors.Is(err, ErrNotInWorld) {
		t.Fatalf("cold replay-only miss = %v, want ErrNotInWorld", err)
	}
	if st, err := n.BaseStateOf(context.Background(), p.ID()); err != nil || st.InWorld || st.AttackSeq != 0 || st.Session != (SessionState{}) || p.ledger.v != before {
		t.Fatalf("cold receipt query changed the player: %+v, %v, high=%d", st, err, p.ledger.v.high)
	}
}
