package alliance

import (
	"context"
	"encoding/json"
	"errors"
	player "example.com/slg-alliance-practice/game/entities/player"
	scoutmap "example.com/slg-alliance-practice/game/entities/scout_map"
	handler "example.com/slg-alliance-practice/game/handler"
	sender "example.com/slg-alliance-practice/game/handler/syncsender"
	"example.com/slg-alliance-practice/game/scout"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/remoteentity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/clock"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 地图共享权威读与原服提交不原子；不把路由校验当成跨事务取消已准入报告的协议。
func ownerRoute(lease entity.RemoteEntityMarkerLease, found bool, err error) (scout.OwnerRoute, error) {
	if err != nil {
		return scout.OwnerRoute{}, err
	}
	if !found || !lease.Shared {
		return scout.OwnerRoute{}, errors.New("shared owner missing")
	}
	return scout.OwnerRoute{SID: lease.OwnerSid, Marker: lease.MarkerEpoch, Route: lease.RouteEpoch}, nil
}
func compareManagedFacts(t *testing.T, path string, actual reportFacts) {
	t.Helper()
	raw, err := os.ReadFile(path)
	check(t, err)
	current, err := json.Marshal(actual)
	check(t, err)
	if string(raw) != string(current) {
		t.Fatalf("managed cold facts differ BEFORE RPC: %s", path)
	}
}
func runManagedMapChild(t *testing.T, role string) {
	ctx := context.Background()
	root := os.Getenv("SLG_MANAGED_ROOT")
	if root == "" {
		t.Fatal("missing isolated managed root")
	}
	scoutmap.RegisterEntity()
	player.RegisterEntity()
	admission := os.Getenv("SLG_MANAGED_ADMISSION") == "1"
	mapSequence, playerSequence := int64(99235), int64(100100)
	if admission {
		mapSequence, playerSequence = 99236, 100200
	}
	mapID, err := entity.BuildEntityID(mapSequence, scoutmap.EntityKindScoutMap)
	check(t, err)
	recipient, err := entity.BuildEntityID(playerSequence, player.EntityKindPlayer)
	check(t, err)
	var sid int32
	isHome := strings.HasPrefix(role, "home-")
	switch role {
	case "owner-a":
		sid = 3100
	case "owner-b", "owner-b-recover":
		sid = 3101
	case "owner-c":
		sid = 3102
	case "home-write", "home-recover":
		sid = 1001
	default:
		t.Fatal("invalid managed role", role)
	}
	var snapshot func(context.Context) (reportFacts, error)
	var owner *allianceOwner
	var home *rewardRuntime
	var resolve func(context.Context, int64) (scout.OwnerRoute, error)
	if isHome {
		home = openRewardRuntime(t, sid, role == "home-write")
		hs := sender.NewAllianceRewardsSender(home.nest)
		if role == "home-write" {
			_, err = home.access.Create(&entity.EntityCreateParam{IsCreate: true, Id: recipient, Kind: player.EntityKindPlayer})
			check(t, err)
			_, err = hs.Sync_AllianceHomeInit(ctx, recipient, sid)
			check(t, err)
		}
		snapshot = func(ctx context.Context) (reportFacts, error) {
			var f reportFacts
			var err error
			f.Inbox, err = hs.Sync_AllianceInbox(ctx, recipient)
			if err != nil {
				return f, err
			}
			f.Wallet, err = hs.Sync_AllianceWallet(ctx, recipient)
			return f, err
		}
		mongo, err := mongodriver.NewClient(fmongo.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")), mongodriver.IndexMigrationPolicy{})
		check(t, err)
		t.Cleanup(func() { check(t, mongo.Close(context.Background())) })
		// 只通过公开 OwnershipStore 读权威元数据，不 Claim 地图，不在原服装载地图实例。
		authority := remoteentity.NewMongoCommitter(mongo, os.Getenv("ROOST_SLG_REWARD_DB"), sid, 0)
		resolve = func(ctx context.Context, id int64) (scout.OwnerRoute, error) {
			return ownerRoute(authority.GetOwnership(ctx, id))
		}
	} else {
		scoutmap.RegisterEntity()
		handler.RegisterAllianceRewardsNestHandlers()
		owner = openPracticeRemoteOwner(t, os.Getenv("ROOST_SLG_REWARD_DB")+":multi-map", sid, role == "owner-a", mapID, scoutmap.EntityKindScoutMap)
		ms := sender.NewAllianceRewardsSender(owner.nest)
		if role == "owner-a" {
			check(t, ms.Sync_AllianceMapInit(ctx, mapID, recipient, 1001))
			freezeBusinessTime(1700000000000)
			_, err = ms.Sync_AllianceMarch(ctx, mapID, "handoff-march", 1, 503, 700, 700, false)
			check(t, err)
			freezeBusinessTime(1700000010000)
			applied, err := ms.Sync_AllianceCompleteArrival(ctx, mapID, "handoff-march", false)
			check(t, err)
			clock.SetSource(nil)
			if !applied {
				t.Fatal("missing initial report")
			}
		}
		snapshot = func(ctx context.Context) (reportFacts, error) {
			var f reportFacts
			var err error
			f.Map, err = ms.Sync_AllianceMapSnapshot(ctx, mapID)
			if err != nil {
				return f, err
			}
			f.Report, err = ms.Sync_AllianceReport(ctx, mapID, "handoff-march")
			if err != nil {
				return f, err
			}
			f.Pending, err = ms.Sync_AlliancePendingReports(ctx, mapID, 128)
			return f, err
		}
		resolve = func(ctx context.Context, id int64) (scout.OwnerRoute, error) {
			return ownerRoute(owner.manager.GetRemoteOwnership(ctx, id))
		}
	}
	initial, err := snapshot(ctx)
	check(t, err)
	switch role {
	case "owner-b", "owner-b-recover":
		compareManagedFacts(t, filepath.Join(root, "map-before.json"), initial)
	case "owner-c":
		compareManagedFacts(t, filepath.Join(root, "map-final.json"), initial)
	case "home-recover":
		compareManagedFacts(t, filepath.Join(root, "home-durable.json"), initial)
	}
	kind := "managedprocessmap"
	if isHome {
		kind = "managedprocesshome"
	}
	b := crossReportBus(t, sid, kind)
	// 测试控制独立于业务 dispatcher；不能让释放屏障的命令排在被屏障阻塞的请求后面。
	control := crossReportBus(t, sid, kind+"control")
	admin := func(method string, fn bus.RpcHandlerFunc) {
		target := control
		if method == "tick" || method == "tick-held" || method == "probe" {
			target = b
		}
		check(t, target.HandleRpc(method, func(c *bus.RpcContext) (any, error) {
			if c.FromSid != reportAdminSID {
				return nil, errors.New("admin required")
			}
			return fn(c)
		}))
	}
	if isHome {
		// 故障入口仅存在于测试进程。模式切换原子化，屏障在路由通过后、Player Sender 前。
		var fault atomic.Int32
		var entered atomic.Bool
		release := make(chan struct{})
		var releaseOnce sync.Once
		defer releaseOnce.Do(func() { close(release) })
		admin("fault", func(c *bus.RpcContext) (any, error) {
			var mode int32
			if err := c.Decode(&mode); err != nil {
				return nil, err
			}
			if mode < 0 || mode > 3 {
				return nil, errors.New("invalid test fault")
			}
			fault.Store(mode)
			return true, nil
		})
		admin("admitted", func(c *bus.RpcContext) (any, error) { return entered.Load(), nil })
		admin("release", func(c *bus.RpcContext) (any, error) { releaseOnce.Do(func() { close(release) }); return true, nil })
		authoritativeResolve := resolve
		resolve = func(ctx context.Context, id int64) (scout.OwnerRoute, error) {
			switch fault.Load() {
			case 1:
				return scout.OwnerRoute{}, errors.New("injected authority query failure")
			case 2:
				route, err := authoritativeResolve(ctx, id)
				if err != nil {
					return route, err
				}
				<-ctx.Done()
				return route, nil
			default:
				return authoritativeResolve(ctx, id)
			}
		}
		receiver := scout.RoutedReportReceiver{Timeout: 10 * time.Second, HomeSID: sid, MapID: mapID, Recipient: recipient, Resolve: resolve, Receive: func(ctx context.Context, r scout.Report) error {
			if fault.Load() == 3 {
				entered.Store(true)
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			applied, err := sender.NewAllianceRewardsSender(home.nest).Sync_AllianceReceiveReport(ctx, recipient, sid, r, false)
			if err != nil {
				return err
			}
			if role == "home-write" && applied {
				if home.projector.Stats().WALUnacked != 2 {
					return errors.New("expected unprojected init and receipt")
				}
				f, err := snapshot(ctx)
				if err != nil {
					return err
				}
				if f.Wallet != initial.Wallet {
					return errors.New("receipt changed wallet")
				}
				if err = saveReportFacts(filepath.Join(root, "home-durable.json"), f); err != nil {
					return err
				}
				if admission {
					return nil
				} // 交接前已准入的请求正常回执，旧 owner 的后续 ACK 仍应被 fence。
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		}}
		check(t, b.HandleRpc("receive", func(c *bus.RpcContext) (any, error) {
			scoped := receiver
			if fault.Load() == 2 {
				scoped.Timeout = 100 * time.Millisecond
			}
			return scoped.Handle(c)
		}))
	} else {
		ms := sender.NewAllianceRewardsSender(owner.nest)
		call := func(ctx context.Context, req scout.RoutedReport) error {
			var accepted bool
			if err := b.CallTo(ctx, "managedprocesshome", 1001, "receive", req, &accepted); err != nil {
				return err
			}
			if !accepted {
				return errors.New("missing durable acknowledgment")
			}
			return nil
		}
		d := scout.ReportDelivery{MapID: mapID, Read: func(ctx context.Context, key string) (scout.Report, error) {
			return ms.Sync_AllianceReport(ctx, mapID, key)
		}, Receive: func(ctx context.Context, r scout.Report) error {
			route, err := resolve(ctx, mapID)
			if err != nil {
				return err
			}
			return call(ctx, scout.RoutedReport{Report: r, Owner: route})
		}, Ack: func(ctx context.Context, r scout.Report) error {
			return ms.Sync_AllianceAckReport(ctx, mapID, r, false)
		}}
		tick := func(c *bus.RpcContext, budget time.Duration) (any, error) {
			tickCtx, cancel := context.WithTimeout(c.Context(), budget)
			defer cancel()
			worker := ArrivalWorker{Limit: 1, Discover: func(ctx context.Context, limit int) ([]string, error) {
				return ms.Sync_AlliancePendingReports(ctx, mapID, limit)
			}, Consume: d.Deliver}
			return true, worker.Tick(tickCtx)
		}
		admin("tick", func(c *bus.RpcContext) (any, error) { return tick(c, time.Second) })
		admin("tick-held", func(c *bus.RpcContext) (any, error) { return tick(c, 10*time.Second) })
		admin("transfer", func(c *bus.RpcContext) (any, error) {
			var next int32
			if err := c.Decode(&next); err != nil {
				return nil, err
			}
			if next != 3101 && next != 3102 {
				return nil, errors.New("invalid next owner")
			}
			lease, err := owner.manager.TransferRemoteOwnership(c.Context(), mapID, next)
			return ownerRoute(lease, err == nil, err)
		})
		admin("fenced", func(c *bus.RpcContext) (any, error) {
			return errors.Is(ms.Sync_AllianceAckReport(c.Context(), mapID, initial.Report, false), entity.ErrRemoteFenced), nil
		})
		admin("probe", func(c *bus.RpcContext) (any, error) {
			var req scout.RoutedReport
			if err := c.Decode(&req); err != nil {
				return nil, err
			}
			return true, call(c.Context(), req)
		})
	}
	admin("route", func(c *bus.RpcContext) (any, error) { return resolve(c.Context(), mapID) })
	admin("facts", func(c *bus.RpcContext) (any, error) { return snapshot(c.Context()) })
	stop := make(chan struct{})
	var once sync.Once
	admin("stop", func(c *bus.RpcContext) (any, error) { once.Do(func() { close(stop) }); return true, nil })
	check(t, b.Start())
	check(t, control.Start())
	check(t, saveReportFacts(filepath.Join(os.Getenv("ROOST_SLG_REWARD_WAL"), "ready-"+role+".json"), initial))
	select {
	case <-stop:
	case <-time.After(40 * time.Second):
		t.Fatal("managed process stop timeout")
	}
	drain, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	check(t, b.StopWithContext(drain))
	check(t, control.StopWithContext(drain))
}

func runManagedMapProcesses(t *testing.T) {
	admission := os.Getenv("SLG_MANAGED_ADMISSION") == "1"
	directory := "managed-process"
	if admission {
		directory = "managed-admission"
	}
	root := filepath.Join(os.Getenv("ROOST_SLG_REWARD_WAL"), directory)
	check(t, os.MkdirAll(root, 0700))
	t.Setenv("SLG_MANAGED_ROOT", root)
	start := func(role, dir string) *reportProcess {
		return startReportProcessEntry(t, role, 1001, filepath.Join(root, dir), "TestSLGRemoteMapReports", "SLG_MANAGED_CHILD")
	}
	admin := crossReportBus(t, reportAdminSID, "managedprocessadmin")
	check(t, admin.Start())
	a := start("owner-a", "owner-a")
	h := start("home-write", "home")
	call := func(sid int32, method string, req, resp any) error {
		kind := "managedprocessmap"
		if sid == 1001 {
			kind = "managedprocesshome"
		}
		if method != "tick" && method != "tick-held" && method != "probe" {
			kind += "control"
		}
		return reportCall(admin, sid, kind, method, req, resp)
	}
	facts := func(sid int32) reportFacts { var f reportFacts; check(t, call(sid, "facts", true, &f)); return f }
	before := facts(3100)
	homeBefore := facts(1001)
	check(t, saveReportFacts(filepath.Join(root, "map-before.json"), before))
	var original, next scout.OwnerRoute
	check(t, call(3100, "route", true, &original))
	var heldDone chan error
	var accepted bool
	if admission {
		// 错误和超时后的迟到成功都不能进入收件；恢复不换事件身份。
		for _, mode := range []int32{1, 2} {
			check(t, call(1001, "fault", mode, &accepted))
			if err := call(3100, "tick", true, &accepted); err == nil {
				t.Fatal("authority fault allowed delivery")
			}
			current := facts(1001)
			if current.Inbox != homeBefore.Inbox || current.Wallet != homeBefore.Wallet {
				t.Fatal("failed authority check changed Player")
			}
			current = facts(3100)
			if current.Map != before.Map {
				t.Fatal("failed authority check acknowledged report")
			}
		}
		check(t, call(1001, "fault", int32(3), &accepted))
		heldDone = make(chan error, 1)
		heldCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		go func() {
			var ok bool
			heldDone <- admin.CallTo(heldCtx, "managedprocessmap", 3100, "tick-held", true, &ok)
		}()
		wait, cancelWait := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelWait()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			var admitted bool
			check(t, call(1001, "admitted", true, &admitted))
			if admitted {
				break
			}
			select {
			case err := <-heldDone:
				t.Fatalf("request exited before admission barrier: %v", err)
			case <-wait.Done():
				t.Fatal("admission barrier timeout")
			case <-ticker.C:
			}
		}
		current := facts(1001)
		if current.Inbox != homeBefore.Inbox {
			t.Fatal("barrier must precede Player commit")
		}
	}
	check(t, call(3100, "transfer", int32(3101), &next))
	if next.SID != 3101 || next.Marker <= original.Marker || next.Route <= original.Route {
		t.Fatal("first handoff generations invalid")
	}
	var fenced bool
	check(t, call(3100, "fenced", true, &fenced))
	if !fenced {
		t.Fatal("live old process not fenced")
	}
	if admission {
		check(t, call(1001, "release", true, &accepted))
		select {
		case err := <-heldDone:
			if err == nil {
				t.Fatal("old owner acknowledged admitted report after transfer")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("admitted request did not drain")
		}
		check(t, call(1001, "fault", int32(0), &accepted))
	}
	if err := call(3100, "probe", scout.RoutedReport{Report: before.Report, Owner: original}, &accepted); err == nil {
		t.Fatal("old process delivered after transfer")
	}
	b := start("owner-b", "owner-b")
	if !admission {
		if err := call(3101, "tick", true, &accepted); err == nil {
			t.Fatal("lost receive response acknowledged")
		}
	}
	raw, err := os.ReadFile(filepath.Join(root, "home-durable.json"))
	check(t, err)
	var durable reportFacts
	check(t, json.Unmarshal(raw, &durable))
	var receipts map[string]scout.Report
	check(t, json.Unmarshal([]byte(durable.Inbox), &receipts))
	if len(receipts) != 1 || receipts[before.Report.Identity()] != before.Report || durable.Wallet != homeBefore.Wallet {
		t.Fatal("durable receipt changed facts")
	}
	pending := facts(3101)
	if pending.Map != before.Map || len(pending.Pending) != 1 {
		t.Fatal("unknown receive response lost pending report")
	}
	// 真实强杀新 owner 与原服。Remote 地图已提交到 Mongo 权威；Player 有两条未投影 WAL。
	b.kill(t)
	h.kill(t)
	h = start("home-recover", "home")
	b = start("owner-b-recover", "owner-b")
	check(t, call(3100, "fenced", true, &fenced))
	if !fenced {
		t.Fatal("old process regained write after new owner crash")
	}
	if err := call(3100, "probe", scout.RoutedReport{Report: before.Report, Owner: next}, &accepted); err == nil {
		t.Fatal("old process spoofed new route")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); var ok bool; errs <- call(3101, "tick", true, &ok) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		check(t, err)
	}
	final := facts(3101)
	homeFinal := facts(1001)
	if len(final.Pending) != 0 || homeFinal.Inbox != durable.Inbox || homeFinal.Wallet != durable.Wallet {
		t.Fatal("recovery duplicated report or lost ACK")
	}
	check(t, saveReportFacts(filepath.Join(root, "map-final.json"), final))
	var last scout.OwnerRoute
	check(t, call(3101, "transfer", int32(3102), &last))
	if last.SID != 3102 || last.Marker <= next.Marker || last.Route <= next.Route {
		t.Fatal("second handoff generations invalid")
	}
	check(t, call(3101, "fenced", true, &fenced))
	if !fenced {
		t.Fatal("second process not fenced")
	}
	c := start("owner-c", "owner-c")
	check(t, call(3102, "tick", true, &accepted))
	current := facts(3102)
	if current.Map != final.Map {
		t.Fatal("third owner changed final map")
	}
	for _, p := range []struct {
		sid int32
		p   *reportProcess
	}{{3100, a}, {3101, b}, {3102, c}, {1001, h}} {
		check(t, call(p.sid, "stop", true, &accepted))
		p.p.wait(t)
	}
	check(t, admin.StopWithContext(context.Background()))
	if admission {
		t.Log("admission barrier verified: authority errors/late success do not write; admitted report commits across handoff but old owner cannot ACK")
	}
	t.Log(fmt.Sprintf("independent owner processes 3100 -> 3101 -> 3102: old owner alive/fenced, owner 3101 and home SIGKILL, original WAL/authority recovery, eight duplicate ticks, final ACK survives handoff"))
}
