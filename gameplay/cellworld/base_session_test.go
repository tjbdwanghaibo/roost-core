package cellworld

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 真实 Nest 锁、strict 进场与 Cell 收件；SessionActive 只替换本地 TCP 连接表。
func TestBaseSessionAdmissionStopReconnectAndGrace(t *testing.T) {
	spec, _ := loopbackSpec(t, 1, 1)
	var live sync.Map
	const grace = 500 * time.Millisecond
	spec.baseMod = func(cfg *NodeConfig) {
		cfg.ReconnectGrace = grace
		cfg.SessionActive = func(_ int64, c string) bool { _, ok := live.Load(c); return ok }
	}
	w := startWorld(t, spec)
	const u = 41
	base, actor := w.base.player(u).ID(), actorID(u)
	n, ctx := w.base.node, context.Background()
	spawn := spatial.Point{X: 500, Y: 500}
	admit := func(s BaseSession) (BaseState, error) { return n.AdmitSession(ctx, base, actor, spawn, 100, s) }
	state := func() BaseState {
		st, err := n.BaseStateOf(ctx, base)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	old := BaseSession{"account-session", "nonce-A", 1}
	next := BaseSession{"account-session", "nonce-B", 2} // 同一 SessionID，不同的物理连接。
	third := BaseSession{"another-login", "nonce-C", 3}
	live.Store(old.ConnectionID, true)
	start := make(chan struct{})
	results := make(chan error, 16)
	for range cap(results) {
		go func() {
			<-start
			st, err := admit(old)
			if err == nil && (st.Entry != 1 || st.Actor != actor) {
				err = fmt.Errorf("wrong state %+v", st)
			}
			results <- err
		}()
	}
	close(start)
	for range cap(results) {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := n.EnsureEntered(ctx, base, actor+1, spawn, 100); err == nil {
		t.Fatal("different actor admitted")
	}
	if err := n.Enter(ctx, base, actor, spawn, 100); err == nil {
		t.Fatal("legacy Enter accepted duplicate")
	}
	w.waitView(u, settleWithin, "first owner", func(v View) bool { return v.Cell == 101 })
	move := func(s BaseSession, v int64) {
		t.Helper()
		if err := n.MoveSession(ctx, base, s, v, 0); err != nil {
			t.Fatal(err)
		}
	}
	move(old, 100)
	w.waitView(u, settleWithin, "moving owner", func(v View) bool { return v.Self.VX == 100 })
	// 当前关闭必须转发零速度，不依赖 Cell 租约到期。
	disconnect := func(s BaseSession) {
		t.Helper()
		if err := n.DisconnectSession(ctx, base, s.SessionID, s.ConnectionID); err != nil {
			t.Fatal(err)
		}
	}
	disconnect(old)
	w.waitView(u, settleWithin, "zero reached owner", func(v View) bool { return v.Self.VX == 0 })
	stopped := state()
	if !stopped.InWorld || stopped.Session.OfflineAt == 0 || stopped.VX != 0 {
		t.Fatalf("close: %+v", stopped)
	}
	// 强制重复事件通过同一个事务入口，并等待它处理完成，排除只检查异步投递成功。
	closeSync := func(s BaseSession) {
		t.Helper()
		_, err := n.cfg.Nest.Request(ctx, hBaseCmd, base, nest.Params{baseCmd{kind: cmdDisconnect, session: s, closedAt: time.Now()}})
		if err != nil {
			t.Fatal(err)
		}
	}
	closeSync(old)
	if st := state(); st.Session.OfflineAt != stopped.Session.OfflineAt || st.IntentSeq != stopped.IntentSeq {
		t.Fatal("duplicate close changed deadline or reissued stop")
	}
	live.Store(next.ConnectionID, true)
	if st, err := admit(next); err != nil || st.Entry != 1 || st.Session.OfflineAt != 0 {
		t.Fatalf("short reconnect: %+v %v", st, err)
	}
	move(next, 62)
	closeSync(old)
	if _, err := admit(old); !errors.Is(err, ErrStaleSession) {
		t.Fatalf("older live admission: %v", err)
	}
	if err := n.MoveSession(ctx, base, old, 93, 0); !errors.Is(err, ErrStaleSession) {
		t.Fatalf("older input: %v", err)
	}
	time.Sleep(time.Until(time.Unix(0, stopped.Session.OfflineAt).Add(grace)))
	w.base.waitBaseBeats(2)
	if st := state(); !st.InWorld || st.VX != 62 || st.Session.BaseSession != next {
		t.Fatalf("late close/deadline changed live session: %+v", st)
	}
	// 新连接先接替、旧关闭后执行：先原子停旧意图，然后旧关闭不能停新输入。
	live.Store(third.ConnectionID, true)
	if st, err := admit(third); err != nil || st.Entry != 1 || st.VX != 0 {
		t.Fatalf("takeover: %+v %v", st, err)
	}
	move(third, 79)
	closeSync(next)
	if st := state(); st.VX != 79 || st.Session.OfflineAt != 0 {
		t.Fatalf("late B close: %+v", st)
	}
	// 故意丢失第三条连接的关闭事件：心跳补偿并经正常 Leave 移除 Real。
	live.Delete(third.ConnectionID)
	eventually(t, settleWithin, "offline stop after missed event", func() (bool, string) {
		st := state()
		return st.Session.OfflineAt != 0 && st.VX == 0, fmt.Sprint(st.Session)
	})
	eventually(t, settleWithin+grace, "grace leave", func() (bool, string) { return !state().InWorld, "still in world" })
	eventually(t, settleWithin, "owner removed after leave", func() (bool, string) { _, found := w.roleOf(CellID{}, actor); return !found, "still in cell" })
	if _, err := admit(next); !errors.Is(err, ErrStaleSession) {
		t.Fatalf("old overlapping connection came back after leave: %v", err)
	}
	fourth := BaseSession{"session-4", "nonce-D", 4}
	live.Store(fourth.ConnectionID, true)
	if st, err := admit(fourth); err != nil || st.Entry != 2 || st.Actor != actor {
		t.Fatalf("readmission after expiry: %+v %v", st, err)
	}
}

type rejectSessionCommit struct{}

var errRejectSessionCommit = errors.New("test: reject strict session commit")

func (rejectSessionCommit) PrepareCommit(*nest.RollbackTx) error { return errRejectSessionCommit }

// 让旧 admission 已进 handler、调用方先超时，再允许它执行。不存在由提交后单边通知
// 唤醒的等待者；结果未知的旧请求与后续新请求最终按实体内的 fence 收敛。
func TestBaseSessionTimeoutRollbackAndDrain(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var waiting atomic.Bool
	p := startProc(t, procSpec{sid: 201, base: true, grid: worldGrid(t, 1, 1), transport: newLoopback(), mod: func(cfg *NodeConfig) {
		cfg.ReconnectGrace = time.Minute
		cfg.SessionActive = func(_ int64, c string) bool {
			if c == "reject-commit" {
				if err := nest.CurrentRollbackTx().RegisterCommitParticipant(rejectSessionCommit{}); err != nil {
					panic(err)
				}
			}
			if c == "A" && waiting.CompareAndSwap(false, true) {
				close(entered)
				<-release
			}
			return true
		}
	}})
	defer p.stop()
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	n, base, actor := p.node, p.player(42).ID(), actorID(42)
	spawn := spatial.Point{X: 100, Y: 200}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := n.AdmitSession(ctx, base, actor, spawn, 100, BaseSession{"s", "A", 1})
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline reported %v", err)
	}
	unblock()
	newest := BaseSession{"s", "B", 2}
	bg := context.Background()
	st, err := n.AdmitSession(bg, base, actor, spawn, 100, newest)
	if err != nil || st.Entry != 1 || st.Session.BaseSession != newest {
		t.Fatalf("retry: %+v %v", st, err)
	}
	// 准入中发现不同化身拒绝；不得移交绑定，也不遗留 Drain 票据。
	if _, err := n.AdmitSession(bg, base, actorID(43), spawn, 100, BaseSession{"s3", "C", 3}); err == nil {
		t.Fatal("mismatched actor accepted")
	}
	if err := n.MoveSession(bg, base, newest, 22, 33); err != nil {
		t.Fatal(err)
	}
	if _, err := n.AdmitSession(bg, base, actor, spawn, 100, BaseSession{"s4", "reject-commit", 4}); !errors.Is(err, errRejectSessionCommit) {
		t.Fatalf("commit failure: %v", err)
	}
	st, err = n.BaseStateOf(bg, base)
	if err != nil || st.Session.BaseSession != newest || st.VX != 22 || st.VY != 33 || st.Entry != 1 {
		t.Fatalf("failed admission changed binding or velocity: %+v %v", st, err)
	}
	stop, done := context.WithTimeout(bg, time.Second)
	defer done()
	if err := n.Drain(stop); err != nil {
		t.Fatalf("rollback stranded drain: %v", err)
	}
}
