package alliance

import (
	"context"
	"encoding/json"
	"errors"
	player "example.com/slg-alliance-practice/game/entities/player"
	scoutmap "example.com/slg-alliance-practice/game/entities/scout_map"
	sender "example.com/slg-alliance-practice/game/handler/syncsender"
	"example.com/slg-alliance-practice/game/scout"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/clock"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	natsdriver "github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const reportAdminSID int32 = 9000

type reportFacts struct {
	Map, Inbox, Wallet string
	Report             scout.Report
	Pending            []string
}

func saveReportFacts(path string, v reportFacts) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err = os.WriteFile(path+".tmp", raw, 0600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}
func crossReportBus(t *testing.T, sid int32, kind string) *bus.Bus {
	a, err := natsdriver.Assemble(fnats.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")), natsdriver.ClientOptions{})
	check(t, err)
	b := bus.New(a.Client, a.RPC, nil, bus.Config{Sid: sid, SvcType: kind, Prefix: os.Getenv("ROOST_SLG_REWARD_DB") + ".report", WorkerNum: 2, QueueCap: 32})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		check(t, b.StopWithContext(ctx))
		check(t, a.Close(ctx))
	})
	return b
}
func reportCall(b *bus.Bus, sid int32, kind, method string, in, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return b.CallTo(ctx, kind, sid, method, in, out)
}
func runCrossReportChild(t *testing.T, role string) {
	home64, err := strconv.ParseInt(os.Getenv("SLG_REPORT_HOME"), 10, 32)
	check(t, err)
	home := int32(home64)
	if home != 1000 && home != 1001 {
		t.Fatal("invalid report home")
	}
	mapSID := home + 1000
	isMap := strings.HasPrefix(role, "map-")
	sid, kind := home, "reporthome"
	if isMap {
		sid, kind = mapSID, "reportmap"
	}
	root := os.Getenv("ROOST_SLG_REWARD_WAL")
	n := openRewardRuntime(t, sid, strings.HasSuffix(role, "write"))
	s := sender.NewAllianceRewardsSender(n.nest)
	recipient, err := entity.BuildEntityID(int64(home)*100, player.EntityKindPlayer)
	check(t, err)
	mapID, err := entity.BuildEntityID(int64(mapSID)*100, scoutmap.EntityKindScoutMap)
	check(t, err)
	ctx := context.Background()
	if role == "map-write" {
		_, err = n.access.Create(&entity.EntityCreateParam{IsCreate: true, Id: mapID, Kind: scoutmap.EntityKindScoutMap})
		check(t, err)
		check(t, s.Sync_AllianceMapInit(ctx, mapID, recipient, home))
		freezeBusinessTime(1700000000000)
		_, err = s.Sync_AllianceMarch(ctx, mapID, "cross-march", 1, 503, 700, 700, false)
		check(t, err)
		freezeBusinessTime(1700000010000)
		applied, err := s.Sync_AllianceCompleteArrival(ctx, mapID, "cross-march", false)
		check(t, err)
		clock.SetSource(nil)
		if !applied || n.projector.Stats().WALUnacked != 3 {
			t.Fatal("map setup must remain unprojected")
		}
	}
	facts := func(ctx context.Context) (reportFacts, error) {
		var out reportFacts
		var err error
		if isMap {
			out.Map, err = s.Sync_AllianceMapSnapshot(ctx, mapID)
			if err != nil {
				return out, err
			}
			out.Report, err = s.Sync_AllianceReport(ctx, mapID, "cross-march")
			if err != nil {
				return out, err
			}
			out.Pending, err = s.Sync_AlliancePendingReports(ctx, mapID, 128)
		} else {
			out.Inbox, err = s.Sync_AllianceInbox(ctx, recipient)
			if err != nil {
				return out, err
			}
			out.Wallet, err = s.Sync_AllianceWallet(ctx, recipient)
		}
		return out, err
	}
	initial, err := facts(ctx)
	check(t, err)
	if !strings.HasSuffix(role, "write") {
		name := "cross-durable.json"
		if strings.HasSuffix(role, "verify") {
			name = "cross-final.json"
		}
		raw, err := os.ReadFile(filepath.Join(root, name))
		check(t, err)
		current, err := json.Marshal(initial)
		check(t, err)
		if string(raw) != string(current) {
			t.Fatal("cross report cold facts differ BEFORE RPC/retry")
		}
	} else if isMap {
		check(t, saveReportFacts(filepath.Join(root, "cross-durable.json"), initial))
	}
	b := crossReportBus(t, sid, kind)
	if isMap {
		d := scout.ReportDelivery{MapID: mapID, Read: func(ctx context.Context, key string) (scout.Report, error) {
			return s.Sync_AllianceReport(ctx, mapID, key)
		}, Receive: func(ctx context.Context, r scout.Report) error {
			var accepted bool
			if err := b.CallTo(ctx, "reporthome", r.HomeSID, "receive", r, &accepted); err != nil {
				return err
			}
			if !accepted {
				return errors.New("missing durable receipt acknowledgment")
			}
			return nil
		}, Ack: func(ctx context.Context, r scout.Report) error { return s.Sync_AllianceAckReport(ctx, mapID, r, false) }}
		check(t, b.HandleRpc("tick", func(c *bus.RpcContext) (any, error) {
			if c.FromSid != reportAdminSID {
				return nil, errors.New("admin required")
			}
			tickCtx, cancel := context.WithTimeout(c.Context(), time.Second)
			defer cancel()
			w := ArrivalWorker{Limit: 1, Discover: func(ctx context.Context, limit int) ([]string, error) {
				return s.Sync_AlliancePendingReports(ctx, mapID, limit)
			}, Consume: d.Deliver}
			return true, w.Tick(tickCtx)
		}))
		// 仅测试控制入口：以合法地图来源发送错误目的信息，验证接收方的配置检查。
		check(t, b.HandleRpc("probe", func(c *bus.RpcContext) (any, error) {
			if c.FromSid != reportAdminSID {
				return nil, errors.New("admin required")
			}
			var r scout.Report
			if err := c.Decode(&r); err != nil {
				return nil, err
			}
			var accepted bool
			err := b.CallTo(c.Context(), "reporthome", home, "receive", r, &accepted)
			return accepted, err
		}))
	} else {
		receiver := scout.ReportReceiver{HomeSID: home, MapSID: mapSID, MapID: mapID, Recipient: recipient, Receive: func(ctx context.Context, r scout.Report) error {
			applied, err := s.Sync_AllianceReceiveReport(ctx, recipient, home, r, false)
			if err != nil {
				return err
			}
			if role == "home-write" && applied {
				if n.projector.Stats().WALUnacked != 1 {
					return errors.New("receipt was not the only unprojected transaction")
				}
				durable, err := facts(ctx)
				if err != nil {
					return err
				}
				if durable.Wallet != initial.Wallet {
					return errors.New("cross report changed wallet")
				}
				if err = saveReportFacts(filepath.Join(root, "cross-durable.json"), durable); err != nil {
					return err
				}
				<-ctx.Done() // 真正让 RPC 超时；原服已经 durable，但不回复成功。
				return ctx.Err()
			}
			return nil
		}}
		check(t, b.HandleRpc("receive", receiver.Handle))
	}
	check(t, b.HandleRpc("facts", func(c *bus.RpcContext) (any, error) {
		if c.FromSid != reportAdminSID {
			return nil, errors.New("admin required")
		}
		return facts(c.Context())
	}))
	stop := make(chan struct{})
	var once sync.Once
	check(t, b.HandleRpc("stop", func(c *bus.RpcContext) (any, error) {
		if c.FromSid != reportAdminSID {
			return nil, errors.New("admin required")
		}
		once.Do(func() { close(stop) })
		return true, nil
	}))
	check(t, b.Start())
	check(t, saveReportFacts(filepath.Join(root, "ready-"+role+".json"), initial))
	select {
	case <-stop:
	case <-time.After(40 * time.Second):
		t.Fatal("cross report child stop timeout")
	}
	// 总线停止/排空必须先于 Runtime 清理，保留在途处理使用的依赖。
	drain, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	check(t, b.StopWithContext(drain))
}

type reportProcess struct {
	cmd     *exec.Cmd
	done    chan error
	stopped bool
	log     string
}

func startReportProcess(t *testing.T, role string, home int32, root string) *reportProcess {
	t.Helper()
	check(t, os.MkdirAll(root, 0700))
	log, err := os.Create(filepath.Join(root, "cross-"+role+".log"))
	check(t, err)
	t.Cleanup(func() { _ = log.Close() })
	cmd := exec.Command(os.Args[0], "-test.run=^TestSLGAlliancePlayerDelivery$", "-test.timeout=55s")
	cmd.Env = append(os.Environ(), "SLG_REPORT_CHILD="+role, "SLG_REPORT_HOME="+fmt.Sprint(home), "ROOST_SLG_REWARD_WAL="+root)
	cmd.Stdout = log
	cmd.Stderr = log
	p := &reportProcess{cmd: cmd, done: make(chan error, 1), log: log.Name()}
	check(t, cmd.Start())
	go func() { p.done <- cmd.Wait() }()
	t.Cleanup(func() {
		if !p.stopped {
			_ = cmd.Process.Kill()
			<-p.done
		}
	})
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case err := <-p.done:
			p.stopped = true
			raw, _ := os.ReadFile(p.log)
			t.Fatalf("%s exited: %v\n%s", role, err, raw)
		case <-deadline.C:
			raw, _ := os.ReadFile(p.log)
			t.Fatalf("%s readiness timeout\n%s", role, raw)
		case <-poll.C:
			if _, err := os.Stat(filepath.Join(root, "ready-"+role+".json")); err == nil {
				return p
			}
		}
	}
}
func (p *reportProcess) kill(t *testing.T) {
	t.Helper()
	check(t, p.cmd.Process.Kill())
	err := <-p.done
	p.stopped = true
	if err == nil {
		t.Fatal("expected SIGKILL")
	}
}
func (p *reportProcess) wait(t *testing.T) {
	t.Helper()
	select {
	case err := <-p.done:
		p.stopped = true
		if err != nil {
			raw, _ := os.ReadFile(p.log)
			t.Fatalf("cross child: %v\n%s", err, raw)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cross child drain timeout")
	}
}

func runCrossReport(t *testing.T, home int32, root string) {
	t.Helper()
	mapRoot := filepath.Join(root, "cross-map")
	admin := crossReportBus(t, reportAdminSID, "reportadmin")
	check(t, admin.Start())
	homeProc := startReportProcess(t, "home-write", home, root)
	mapProc := startReportProcess(t, "map-write", home, mapRoot)
	mapSID := home + 1000
	get := func(sid int32, kind string) reportFacts {
		var f reportFacts
		check(t, reportCall(admin, sid, kind, "facts", true, &f))
		return f
	}
	beforeMap := get(mapSID, "reportmap")
	beforeHome := get(home, "reporthome")
	var ok bool
	// 管理端直接伪造报告无法穿过来源检查；合法地图来源的错误路由同样拒绝。
	if err := reportCall(admin, home, "reporthome", "receive", beforeMap.Report, &ok); err == nil {
		t.Fatal("untrusted source accepted")
	}
	for _, mutate := range []func(*scout.Report){func(r *scout.Report) {
		r.HomeSID = 1001
		if home == 1001 {
			r.HomeSID = 1000
		}
	}, func(r *scout.Report) { r.Recipient++ }, func(r *scout.Report) { r.MapID++ }} {
		wrong := beforeMap.Report
		mutate(&wrong)
		if err := reportCall(admin, mapSID, "reportmap", "probe", wrong, &ok); err == nil {
			t.Fatal("wrong report route accepted")
		}
	}
	if err := reportCall(admin, mapSID, "reportmap", "tick", true, &ok); err == nil {
		t.Fatal("lost receipt response acknowledged")
	}
	raw, err := os.ReadFile(filepath.Join(root, "cross-durable.json"))
	check(t, err)
	var durable reportFacts
	check(t, json.Unmarshal(raw, &durable))
	var inbox map[string]scout.Report
	check(t, json.Unmarshal([]byte(durable.Inbox), &inbox))
	if len(inbox) != 2 || inbox[beforeMap.Report.Identity()] != beforeMap.Report || durable.Wallet != beforeHome.Wallet {
		t.Fatal("cross receipt missing or wallet changed")
	}
	current := get(mapSID, "reportmap")
	if current.Map != beforeMap.Map || len(current.Pending) != 1 {
		t.Fatal("map acknowledged unknown receive")
	}
	homeProc.kill(t)
	mapProc.kill(t)
	homeProc = startReportProcess(t, "home-recover", home, root)
	mapProc = startReportProcess(t, "map-recover", home, mapRoot)
	// 两边先核对冷事实再开放 RPC；并发地图工作轮次共享原永久身份。
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var result bool
			errs <- reportCall(admin, mapSID, "reportmap", "tick", true, &result)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		check(t, err)
	}
	afterMap := get(mapSID, "reportmap")
	afterHome := get(home, "reporthome")
	if len(afterMap.Pending) != 0 || afterHome.Inbox != durable.Inbox || afterHome.Wallet != beforeHome.Wallet {
		t.Fatal("cross retry changed immutable receipt")
	}
	check(t, saveReportFacts(filepath.Join(mapRoot, "cross-final.json"), afterMap))
	check(t, saveReportFacts(filepath.Join(root, "cross-final.json"), afterHome))
	stop := func() {
		check(t, reportCall(admin, mapSID, "reportmap", "stop", true, &ok))
		mapProc.wait(t)
		check(t, reportCall(admin, home, "reporthome", "stop", true, &ok))
		homeProc.wait(t)
	}
	stop()
	homeProc = startReportProcess(t, "home-verify", home, root)
	mapProc = startReportProcess(t, "map-verify", home, mapRoot)
	check(t, reportCall(admin, mapSID, "reportmap", "tick", true, &ok))
	finalMap := get(mapSID, "reportmap")
	finalHome := get(home, "reporthome")
	if finalMap.Map != afterMap.Map || finalHome.Inbox != afterHome.Inbox || finalHome.Wallet != afterHome.Wallet {
		t.Fatal("third process changed completed cross report")
	}
	stop()
	// 同一父进程还会验下一个原服；先解绑管理端 SID 避免重复实例收包。
	check(t, admin.StopWithContext(context.Background()))
	t.Logf("cross report map %d -> static home %d: NATS timeout, both WALs SIGKILL, eight retries, third-process verification", mapSID, home)
}
