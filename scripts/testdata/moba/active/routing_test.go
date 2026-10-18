package handler

import (
	"context"
	player "example.com/moba-practice/game/entities/player"
	sender "example.com/moba-practice/game/handler/syncsender"
	"example.com/moba-practice/game/moba"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/nettransport"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	natsdriver "github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const mobaAuthoritySID int32 = 9000
const mobaAdminSID int32 = 9999

func mobaBus(t *testing.T, sid int32, kind string) *bus.Bus {
	t.Helper()
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Fatal("isolated NATS required")
	}
	a, err := natsdriver.Assemble(fnats.DefaultConfig(url), natsdriver.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b := bus.New(a.Client, a.RPC, nil, bus.Config{Sid: sid, SvcType: kind, Prefix: os.Getenv("ROOST_MOBA_DB") + ".moba", WorkerNum: 2, QueueCap: 32})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := b.StopWithContext(ctx); err != nil {
			t.Error(err)
		}
		if err := a.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return b
}
func mobaRPC(b *bus.Bus, sid int32, kind, method string, req, resp any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return b.CallTo(ctx, kind, sid, method, req, resp)
}
func mustMOBARPC(t *testing.T, b *bus.Bus, sid int32, kind, method string, req, resp any) {
	t.Helper()
	if err := mobaRPC(b, sid, kind, method, req, resp); err != nil {
		t.Fatal(method, err)
	}
}
func registerStop(t *testing.T, b *bus.Bus) <-chan struct{} {
	t.Helper()
	stop := make(chan struct{})
	var once sync.Once
	if err := b.HandleRpc("stop", func(c *bus.RpcContext) (any, error) {
		if c.FromSid != mobaAdminSID {
			return nil, moba.ErrRule
		}
		once.Do(func() { close(stop) })
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	return stop
}

type awardDelivery struct {
	Limit   int
	OmitAck bool
}
type playerFacts struct {
	ID, Gold, Wins, Losses int64
	Receipts               int
}

func runMOBARoutingChild(t *testing.T, role string) {
	parts := strings.Split(role, "-")
	sid64, err := strconv.ParseInt(parts[1], 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	sid := int32(sid64)
	kind := "battle"
	if parts[0] == "origin" {
		kind = "origin"
	}
	b := mobaBus(t, sid, kind)
	stop := registerStop(t, b)
	var record moba.Round
	mustMOBARPC(t, b, mobaAuthoritySID, "authority", "round", moba.Change{Op: "read"}, &record)
	if kind == "battle" {
		var endpoint *moba.HostEndpoint
		var actor *moba.Actor
		var mu sync.Mutex
		t.Cleanup(func() {
			if actor != nil {
				_ = actor.Stop(context.Background())
			}
		})
		if err := b.HandleRpc("load", func(c *bus.RpcContext) (any, error) {
			if c.FromSid != mobaAdminSID {
				return nil, moba.ErrRule
			}
			mu.Lock()
			defer mu.Unlock()
			var r moba.Round
			if err := b.CallTo(c.Context(), "authority", mobaAuthoritySID, "round", moba.Change{Op: "read"}, &r); err != nil {
				return nil, err
			}
			if r.HostSID != sid {
				return nil, moba.ErrRule
			}
			if actor != nil {
				if err := actor.Stop(c.Context()); err != nil {
					return nil, err
				}
			}
			persist := func(ctx context.Context, change moba.Change) (moba.Round, error) {
				var saved moba.Round
				err := b.CallTo(ctx, "authority", mobaAuthoritySID, "round", change, &saved)
				return saved, err
			}
			room, err := moba.RecoverRoom(r, persist, nettransport.TransportFunc{Datagram: func(context.Context, nettransport.SessionID, []byte) error { return nil }})
			if err != nil {
				return nil, err
			}
			actor = moba.NewActor(room)
			endpoint = moba.NewHostEndpoint(actor, r, mobaAdminSID)
			return r, nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := b.HandleRpc("room", func(c *bus.RpcContext) (any, error) {
			var req moba.RoomRequest
			if err := c.Decode(&req); err != nil {
				return nil, err
			}
			mu.Lock()
			defer mu.Unlock()
			if endpoint == nil {
				return nil, moba.ErrRule
			}
			return endpoint.Call(c.Context(), c.FromSid, req)
		}); err != nil {
			t.Fatal(err)
		}
	} else {
		h := openActiveRuntime(t, "cross-origin-"+strconv.Itoa(int(sid)), false)
		if !strings.HasSuffix(role, "-restart") {
			h.createPlayers(t, record, sid)
		}
		if err := b.HandleRpc("relay", func(c *bus.RpcContext) (any, error) {
			if c.FromSid != mobaAdminSID {
				return nil, moba.ErrRule
			}
			var req moba.RoomRequest
			if err := c.Decode(&req); err != nil {
				return nil, err
			}
			var r moba.Round
			if err := b.CallTo(c.Context(), "authority", mobaAuthoritySID, "round", moba.Change{Op: "read"}, &r); err != nil {
				return nil, err
			}
			found := false
			for _, member := range r.Members {
				if member.PlayerID == req.PlayerID && member.SID == sid && req.PlayerSID == sid {
					found = true
					break
				}
			}
			if !found {
				return nil, moba.ErrRule
			}
			req.Epoch = r.Epoch
			var out moba.RoomReply
			err := b.CallTo(c.Context(), "battle", r.HostSID, "room", req, &out)
			return out, err
		}); err != nil {
			t.Fatal(err)
		}
		if err := b.HandleRpc("awards", func(c *bus.RpcContext) (any, error) {
			if c.FromSid != mobaAdminSID {
				return nil, moba.ErrRule
			}
			var req awardDelivery
			if err := c.Decode(&req); err != nil {
				return nil, err
			}
			delivered := 0
			for i, m := range record.Members {
				if m.SID != sid {
					continue
				}
				var award moba.Award
				if err := b.CallTo(c.Context(), "authority", mobaAuthoritySID, "award", i, &award); err != nil {
					return nil, err
				}
				if award.Member != m {
					return nil, moba.ErrRule
				}
				if err := sender.NewMobaRoundSender(h.scheduler).Sync_MOBAAward(c.Context(), m.PlayerID, award); err != nil {
					return nil, err
				}
				delivered++
				if !req.OmitAck {
					var r moba.Round
					if err := b.CallTo(c.Context(), "authority", mobaAuthoritySID, "round", moba.Change{Op: "read"}, &r); err != nil {
						return nil, err
					}
					if err := b.CallTo(c.Context(), "authority", mobaAuthoritySID, "round", moba.Change{Op: "ack", HostSID: r.HostSID, Epoch: r.Epoch, Seat: i, Digest: award.Digest}, &r); err != nil {
						return nil, err
					}
				}
				if req.Limit > 0 && delivered >= req.Limit {
					break
				}
			}
			return delivered, nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := b.HandleRpc("facts", func(c *bus.RpcContext) (any, error) {
			if c.FromSid != mobaAdminSID {
				return nil, moba.ErrRule
			}
			out := []playerFacts{}
			for _, m := range record.Members {
				if m.SID != sid {
					continue
				}
				v, err := h.access.Get(c.Context(), m.PlayerID, entity.EntityCategoryOther)
				if err != nil {
					return nil, err
				}
				d := v.(*player.Player).Dao()
				receipts := strings.Count(d.GetMobaRewards(), "\"RoundID\"")
				out = append(out, playerFacts{m.PlayerID, d.GetGold(), d.GetWins(), d.GetLosses(), receipts})
			}
			return out, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	writeMOBAReady(t, "ready-"+role, true)
	select {
	case <-stop:
	case <-time.After(70 * time.Second):
		t.Fatal("routing child stop deadline")
	}
	// 排空总线先于 Entity / WAL 关闭，等待已接纳的 RPC 真正返回。
	if err := b.StopWithContext(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// 四个真实子进程（两个原服、两个 battle host）加固定权威：NATS 路由、重连、旧宿主隔离与逐玩家奖励恢复。
// 使用合成的已鉴权连接上下文，不替代真实 account 登录；KCP 客户端传输在单独用例验收。
func TestMOBARealCrossSIDHostRouting(t *testing.T) {
	if role := os.Getenv("ROOST_MOBA_CHILD"); role != "" {
		runMOBARoutingChild(t, role)
		return
	}
	if err := os.MkdirAll(os.Getenv("ROOST_MOBA_WAL"), 0700); err != nil {
		t.Fatal(err)
	}
	h := openActiveRuntime(t, "cross-authority", false)
	r := h.createRound(t)
	authority := mobaBus(t, mobaAuthoritySID, "authority")
	if err := authority.HandleRpc("round", func(c *bus.RpcContext) (any, error) {
		var change moba.Change
		if err := c.Decode(&change); err != nil {
			return nil, err
		}
		switch change.Op {
		case "read":
		case "frame":
			if c.FromSid != change.HostSID || c.FromSid != 2000 && c.FromSid != 2001 {
				return nil, moba.ErrRule
			}
		case "ack":
			current, err := h.apply(c.Context(), moba.Change{Op: "read"})
			if err != nil {
				return nil, err
			}
			if change.Seat < 0 || change.Seat >= moba.ArenaSeats || current.Members[change.Seat].SID != c.FromSid {
				return nil, moba.ErrRule
			}
		default:
			return nil, moba.ErrRule
		}
		return h.apply(c.Context(), change)
	}); err != nil {
		t.Fatal(err)
	}
	if err := authority.HandleRpc("award", func(c *bus.RpcContext) (any, error) {
		var seat int
		if err := c.Decode(&seat); err != nil {
			return nil, err
		}
		r, err := h.apply(c.Context(), moba.Change{Op: "read"})
		if err != nil {
			return nil, err
		}
		awards, err := r.Awards(h.roundID)
		if err != nil {
			return nil, err
		}
		if seat < 0 || seat >= len(awards) || awards[seat].Member.SID != c.FromSid {
			return nil, moba.ErrRule
		}
		return awards[seat], nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := authority.Start(); err != nil {
		t.Fatal(err)
	}
	admin := mobaBus(t, mobaAdminSID, "drill")
	if err := admin.Start(); err != nil {
		t.Fatal(err)
	}
	children := map[string]*mobaChild{}
	for _, role := range []string{"host-2000", "host-2001", "origin-1000", "origin-1001"} {
		child := startMOBAChild(t, "TestMOBARealCrossSIDHostRouting", role)
		children[role] = child
		waitMOBAReady(t, child, "ready-"+role)
	}
	mustMOBARPC(t, admin, 2000, "battle", "load", true, &r)
	relay := func(i int, op string, session int, payload []byte) {
		t.Helper()
		m := r.Members[i]
		var out moba.RoomReply
		mustMOBARPC(t, admin, m.SID, "origin", "relay", moba.RoomRequest{Op: op, PlayerID: m.PlayerID, PlayerSID: m.SID, Session: nettransport.SessionID(session), Frame: 1, Payload: payload}, &out)
	}
	for i := range moba.ArenaSeats {
		relay(i, "bind", 101+i, nil)
		relay(i, "input", 101+i, realArenaInput(1, i))
	}
	var frame moba.RoomReply
	mustMOBARPC(t, admin, 2000, "battle", "room", moba.RoomRequest{Op: "tick", Epoch: 1}, &frame)
	if frame.Frame.ID != 1 {
		t.Fatal("initial routed frame missing")
	}
	// 管理接管留在权威 Sender；公网 / 原服不能任意改宿主或推进帧。
	r = h.mustApply(t, moba.Change{Op: "handoff", HostSID: 2000, Epoch: 1, NextHost: 2001})
	if err := mobaRPC(admin, 2000, "battle", "room", moba.RoomRequest{Op: "tick", Epoch: 1}, &frame); err == nil {
		t.Fatal("old host wrote after transfer")
	}
	mustMOBARPC(t, admin, 2001, "battle", "load", true, &r)
	for i := range moba.ArenaSeats {
		relay(i, "bind", 201+i, nil)
	}
	m := r.Members[0]
	if err := mobaRPC(admin, m.SID, "origin", "relay", moba.RoomRequest{Op: "input", PlayerID: m.PlayerID, PlayerSID: m.SID, Session: 101, Frame: 2, Payload: moba.ArenaInput{}.Bytes()}, &frame); err == nil {
		t.Fatal("old session admitted")
	}
	if err := mobaRPC(admin, 2001, "battle", "room", moba.RoomRequest{Op: "input", Epoch: 2, PlayerID: m.PlayerID, PlayerSID: m.SID, Session: 201, Frame: 2, Payload: moba.ArenaInput{}.Bytes()}, &frame); err == nil {
		t.Fatal("wrong origin sid admitted")
	}
	// 一队四名不同成员投降，终态从真实路由输入和服务器参考生成。
	for i := range 4 {
		m := r.Members[i]
		mustMOBARPC(t, admin, m.SID, "origin", "relay", moba.RoomRequest{Op: "input", PlayerID: m.PlayerID, PlayerSID: m.SID, Session: nettransport.SessionID(201 + i), Frame: 2, Payload: moba.ArenaInput{Action: moba.ArenaSurrender}.Bytes()}, &frame)
	}
	mustMOBARPC(t, admin, 2001, "battle", "room", moba.RoomRequest{Op: "tick", Epoch: 2}, &frame)
	r = h.mustApply(t, moba.Change{Op: "read"})
	if r.Phase != "ended" || r.Reference.Winner != 2 || len(r.Frames) != 2 {
		t.Fatal("server terminal not authoritative")
	}
	var delivered int
	mustMOBARPC(t, admin, 1000, "origin", "awards", awardDelivery{Limit: 2, OmitAck: true}, &delivered)
	if delivered != 2 {
		t.Fatal("partial reward boundary not reached")
	}
	child := children["origin-1000"]
	if err := child.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-child.done; err == nil {
		t.Fatal("reward child did not crash")
	}
	restarted := startMOBAChild(t, "TestMOBARealCrossSIDHostRouting", "origin-1000-restart")
	children["origin-1000"] = restarted
	waitMOBAReady(t, restarted, "ready-origin-1000-restart")
	for range 2 {
		for _, sid := range []int32{1000, 1001} {
			mustMOBARPC(t, admin, sid, "origin", "awards", awardDelivery{}, &delivered)
			if delivered != 5 {
				t.Fatal("missing roster awards")
			}
		}
	}
	for _, sid := range []int32{1000, 1001} {
		var facts []playerFacts
		mustMOBARPC(t, admin, sid, "origin", "facts", true, &facts)
		if len(facts) != 5 {
			t.Fatal("Player ownership changed")
		}
		for _, fact := range facts {
			index := -1
			for i, m := range r.Members {
				if m.PlayerID == fact.ID {
					index = i
				}
			}
			if fact.Receipts != 1 || index < 0 {
				t.Fatal("missing / duplicate durable reward receipt", fact)
			}
			if index >= 5 {
				if fact.Gold != 10 || fact.Wins != 1 || fact.Losses != 0 {
					t.Fatal("winner paid incorrectly", fact)
				}
			} else if fact.Gold != 0 || fact.Wins != 0 || fact.Losses != 1 {
				t.Fatal("loser stats incorrect", fact)
			}
		}
	}
	r = h.mustApply(t, moba.Change{Op: "read"})
	for _, ack := range r.Acknowledged {
		if !ack {
			t.Fatal("terminal delivery unfinished")
		}
	}
	for role, child := range children {
		sid64, _ := strconv.ParseInt(strings.Split(role, "-")[1], 10, 32)
		kind := "battle"
		if strings.HasPrefix(role, "origin") {
			kind = "origin"
		}
		var stopped bool
		mustMOBARPC(t, admin, int32(sid64), kind, "stop", true, &stopped)
		select {
		case err := <-child.done:
			if err != nil {
				raw, _ := os.ReadFile(child.log)
				t.Fatalf("child failed: %v\n%s", err, raw)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("child did not drain")
		}
	}
	if err := authority.StopWithContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.close(t)
	if err := h.mongo.Database(h.database).Drop(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Log(fmt.Sprintf("NATS 5-process drill: 10 members, host 2000 -> 2001, epoch 1 -> 2, 2 rewards before origin crash; 10 receipts exactly once, all acknowledged"))
}
