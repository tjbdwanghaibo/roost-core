package alliance

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/redistest"
	"github.com/tjbdwanghaibo/roost-core/service/activity"
)

func TestSLGAllianceActivity(t *testing.T) {
	if prefix := os.Getenv("SLG_ALLIANCE_CHILD"); prefix != "" {
		if !safeNamespace(prefix) {
			t.Fatal("unsafe namespace")
		}
		coordinatorProcess(t, prefix)
		return
	}
	addr := os.Getenv("ROOST_DATAENGINE_IT_REDIS_ADDR")
	if addr == "" || os.Getenv("ROOST_DATAENGINE_IT_NATS_URL") == "" {
		t.Skip("run scripts/test-slg-alliance.sh with isolated Redis and NATS")
	}
	runAllianceActivity(t, nil)
}

// deliver 用于扩展同一条活动链路；基础入口只验收原服义务，生成 Player 入口继续实际到账。
func runAllianceActivity(t *testing.T, deliver func(*testing.T, string, activity.Key)) {
	addr := os.Getenv("ROOST_DATAENGINE_IT_REDIS_ADDR")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	prefix := namespace()
	redistest.Cleanup(t, addr, prefix)
	stop := startCoordinator(t, prefix)
	r := redisClient(t)
	c1, c2 := client(t, prefix, 1000), client(t, prefix, 1001)
	key := activity.Key{GroupID: "alliance", ActivityID: "season-1-window-1", Phase: activity.PhaseSettle}
	a, e := newAlliance(r, prefix, key)
	check(t, e)
	check(t, a.open(ctx))
	_, e = c1.OpenActivity(ctx, key, []int32{1000, 1001})
	check(t, e)
	f := newFog(t)
	s, e := a.read(ctx)
	check(t, e)
	check(t, f.reconcile(s))
	if !f.visible(t, 1) || !f.visible(t, 2) {
		t.Fatal("alliance visibility missing")
	}
	if e := a.contribute(ctx, 1001, "alice", "wrong-home", time.Now()); !errors.Is(e, errRefused) {
		t.Fatal("wrong static home admitted", e)
	}
	// 同一战果的并发重试只留一个意图； sid 来自原服可信路由。
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- a.contribute(ctx, 1000, "alice", "battle-a", time.Now()) }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		check(t, e)
	}
	check(t, a.contribute(ctx, 1001, "bob", "battle-b", time.Now()))
	if e := a.contribute(ctx, 1001, "bob", "battle-a", time.Now()); !errors.Is(e, errRefused) {
		t.Fatal("identity reused by another participant", e)
	}
	if e := a.phase(ctx, c1, 1000); !errors.Is(e, errRefused) {
		t.Fatal("phase advanced with open admission", e)
	}
	// 侦察是独立授权来源；退盟撤销共享视野，但仍有侦察权时不应隐藏据点。
	f.interest.Relation("scout").Set(2, []int64{99})
	if !f.visible(t, 2) {
		t.Fatal("scout view missing")
	}
	check(t, a.revoke(ctx, "bob"))
	s, e = a.read(ctx)
	check(t, e)
	check(t, f.reconcile(s))
	if !f.visible(t, 2) {
		t.Fatal("revoking alliance incorrectly removed scout")
	}
	f.interest.Relation("scout").Clear(2)
	if f.visible(t, 2) || !f.visible(t, 1) {
		t.Fatal("last source revocation incorrect")
	}
	if e := a.contribute(ctx, 1001, "bob", "after-revoke", time.Now()); !errors.Is(e, errRefused) {
		t.Fatal("revoked member contributed", e)
	}
	check(t, a.contribute(ctx, 1001, "bob", "battle-b", time.Now())) // 查询已获准的旧战果，不增加新贡献。
	if e := a.drain(ctx, lostProgress{c1}, time.Now()); e == nil {
		t.Fatal("lost-reply seam did not fire")
	}
	check(t, a.freeze(ctx))
	if e := a.phase(ctx, c1, 1000); !errors.Is(e, errRefused) {
		t.Fatal("unconfirmed outbox was settled", e)
	}
	if e := a.contribute(ctx, 1000, "alice", "after-freeze", time.Now()); !errors.Is(e, errRefused) {
		t.Fatal("frozen window admitted new contribution", e)
	}
	// 重建真正的协调器子进程以及业务对象，不能用旧内存的 Sent 标记冒充恢复。
	stop()
	stop = startCoordinator(t, prefix)
	a, e = newAlliance(r, prefix, key)
	check(t, e)
	// 超出 TTL 的待报意图必须人工对账，绝不能换请求身份或盲目重加。
	if e := a.drain(ctx, c1, time.Now().Add(2*time.Hour)); e == nil {
		t.Fatal("expired intent was automatically replayed")
	}
	check(t, a.drain(ctx, c2, time.Now()))
	check(t, a.drain(ctx, c1, time.Now()))
	for _, p := range []string{"alice", "bob"} {
		v, found, e := c1.LookupParticipant(ctx, key, p)
		check(t, e)
		if !found || v.Score != 10 || v.Progress != 1 || v.Applies != 1 {
			t.Fatalf("participant %s counted more than once: %+v", p, v)
		}
	}
	check(t, a.phase(ctx, c1, 1000))
	pending, found, e := c1.LookupActivity(ctx, key)
	check(t, e)
	if !found || pending.Status != activity.StatusCollecting {
		t.Fatal("completed before second home notified", pending)
	}
	check(t, a.phase(ctx, c2, 1001))
	s, e = a.read(ctx)
	check(t, e)
	tokens := map[int32]string{}
	for _, sid := range []int32{1000, 1001} {
		h, e := newHome(r, prefix, sid)
		check(t, e)
		d, e := c1.AttemptDispatch(ctx, key, sid)
		check(t, e)
		tokens[sid] = d.Token
		check(t, h.receive(ctx, d, s))
		wrong := d
		wrong.GameSID = 2000
		if e = h.receive(ctx, wrong, s); !errors.Is(e, errRefused) {
			t.Fatal("wrong home reward accepted", e)
		}
		forged := clone(s)
		v := forged.Intents["battle-a"]
		v.Home = sid
		forged.Intents["another"] = v
		if e = h.receive(ctx, d, forged); !errors.Is(e, errRefused) {
			t.Fatal("changed immutable reward accepted", e)
		}
		// 不 ACK：模拟原服已持久接收，但活动确认未完成。
	}
	stop()
	stop = startCoordinator(t, prefix)
	defer stop()
	for _, sid := range []int32{1000, 1001} {
		h, e := newHome(r, prefix, sid)
		check(t, e)
		before, found, e := h.store.Get(ctx, "rewards")
		check(t, e)
		if !found || len(before.Value.Receipts) != 1 {
			t.Fatal("cold receipt missing")
		}
		// OwedDispatches 发现旧窗口，不从当前活动时间反推身份。
		var owed []activity.Key
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			owed, e = c2.OwedDispatches(ctx, key.GroupID, sid, 10)
			check(t, e)
			if len(owed) > 0 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if len(owed) != 1 || owed[0] != key {
			t.Fatalf("owed old window: %v", owed)
		}
		d, e := c2.AttemptDispatch(ctx, key, sid)
		check(t, e)
		if d.Token != tokens[sid] || d.Attempts != 2 {
			t.Fatal("dispatch identity changed on restart", d)
		}
		check(t, h.receive(ctx, d, s))
		after, found, e := h.store.Get(ctx, "rewards")
		check(t, e)
		if !found || after.Version != before.Version {
			t.Fatal("duplicate reward changed durable receipt")
		}
		p := "alice"
		if sid == 1001 {
			p = "bob"
		}
		if after.Value.Receipts[key.String()].Points[p] != 10 {
			t.Fatal("original home reward mismatch")
		}
		if _, e = c2.AckDispatch(ctx, key, sid, "wrong-token"); e == nil {
			t.Fatal("wrong ack token accepted")
		}
		_, e = c2.AckDispatch(ctx, key, sid, d.Token)
		check(t, e)
		_, e = c1.AckDispatch(ctx, key, sid, d.Token)
		check(t, e)
		owed, e = c1.OwedDispatches(ctx, key.GroupID, sid, 10)
		check(t, e)
		if len(owed) != 0 {
			t.Fatal("acked work still owed")
		}
	}
	if deliver != nil {
		deliver(t, prefix, key)
	}
	t.Log("real NATS + Redis: restarted coordinator; two static-home clients; alliance/scout source revocation; durable contribution replay once; frozen-window barrier; original-home obligations survive missing ACK and replay without rewrite (base workflow establishes obligations; optional generated consumer verifies Player delivery)")
}
