package alliance

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	player "example.com/slg-alliance-practice/game/entities/player"
	sender "example.com/slg-alliance-practice/game/handler/syncsender"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
	"github.com/tjbdwanghaibo/roost-core/service/activity"
)

// 活动 ACK 后的义务仍可枚举。每次重试读取原服持久事实，不能靠 OwedDispatches
// 或内存待发列表恢复 Player 到账；不能把客户端上报的金额传给生成 Sender。
func creditObligation(t *testing.T, h *home, key activity.Key, n *rewardRuntime, p string) int64 {
	t.Helper()
	ctx := context.Background()
	v, found, e := h.store.Get(ctx, "rewards")
	check(t, e)
	if !found || v.Value.SID != h.sid {
		t.Fatal("durable home obligation missing")
	}
	obligation, ok := v.Value.Receipts[key.String()]
	if !ok || obligation.Key != key {
		t.Fatal("wrong durable window")
	}
	amount, ok := obligation.Points[p]
	if !ok {
		t.Fatal("no obligation for player")
	}
	gold, e := sender.NewAllianceRewardsSender(n.nest).Sync_AllianceCredit(ctx, n.id, h.sid, key, amount)
	check(t, e)
	return gold
}
func rewardChild(t *testing.T, mode string) {
	sid64, e := strconv.ParseInt(os.Getenv("SLG_REWARD_SID"), 10, 32)
	check(t, e)
	sid := int32(sid64)
	if sid != 1000 && sid != 1001 {
		t.Fatal("unknown static home")
	}
	prefix := os.Getenv("SLG_REWARD_PREFIX")
	if !safeNamespace(prefix) {
		t.Fatal("unsafe namespace")
	}
	key := activity.Key{GroupID: "alliance", ActivityID: "season-1-window-1", Phase: activity.PhaseSettle}
	h, e := newHome(redisClient(t), prefix, sid)
	check(t, e)
	n := openRewardRuntime(t, sid, mode == "write")
	s := sender.NewAllianceRewardsSender(n.nest)
	ctx := context.Background()
	name := "alice"
	if sid == 1001 {
		name = "bob"
	}
	root := os.Getenv("ROOST_SLG_REWARD_WAL")
	if mode == "write" {
		_, e = n.access.Create(&entity.EntityCreateParam{IsCreate: true, Id: n.id, Kind: player.EntityKindPlayer})
		check(t, e)
		_, e = s.Sync_AllianceHomeInit(ctx, n.id, sid)
		check(t, e)
		if gold := creditObligation(t, h, key, n, name); gold != 10 {
			t.Fatal("first credit", gold)
		}
		raw, e := s.Sync_AllianceWallet(ctx, n.id)
		check(t, e)
		stats := n.projector.Stats()
		if stats.Projected != 0 || stats.WALUnacked != 2 {
			t.Fatalf("want two strict unprojected records: %+v", stats)
		}
		check(t, os.WriteFile(filepath.Join(root, "ready.json"), []byte(raw), 0600))
		select {} // 父进程仅在两条已确认 strict 记录后杀掉自己启动的原服子进程。
	}
	if mode != "recover" && mode != "verify" {
		t.Fatal("unknown child mode")
	}
	expected, e := os.ReadFile(filepath.Join(root, "ready.json"))
	check(t, e)
	raw, e := s.Sync_AllianceWallet(ctx, n.id)
	check(t, e)
	if raw != string(expected) {
		t.Fatal("cold wallet differs BEFORE retry", raw, string(expected))
	}
	var wallet struct {
		Home     int32
		Gold     int64
		Receipts string
	}
	check(t, json.Unmarshal([]byte(raw), &wallet))
	var receipts map[string]int64
	check(t, json.Unmarshal([]byte(wallet.Receipts), &receipts))
	if wallet.Home != sid || wallet.Gold != 10 || len(receipts) != 1 || receipts[key.String()] != 10 {
		t.Fatal("cold permanent receipt or asset missing", wallet)
	}
	// 一笔已到账义务并发重投，资产与账本保持一致；错误金额及错服不能借旧身份绕过检查。
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g, e := s.Sync_AllianceCredit(ctx, n.id, sid, key, 10)
			if e == nil && g != 10 {
				e = fmt.Errorf("duplicate gold=%d", g)
			}
			errs <- e
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		check(t, e)
	}
	wrongSID := int32(1001)
	if sid == 1001 {
		wrongSID = 1000
	}
	if _, e = s.Sync_AllianceCredit(ctx, n.id, wrongSID, key, 10); e == nil {
		t.Fatal("wrong home credited")
	}
	if _, e = s.Sync_AllianceHomeInit(ctx, n.id, wrongSID); e == nil {
		t.Fatal("static home changed")
	}
	if _, e = s.Sync_AllianceCredit(ctx, n.id, sid, key, 11); e == nil {
		t.Fatal("changed replay amount accepted")
	}
	if g := creditObligation(t, h, key, n, name); g != 10 {
		t.Fatal("obligation re-read credited twice")
	}
	after, e := s.Sync_AllianceWallet(ctx, n.id)
	check(t, e)
	if after != raw {
		t.Fatal("replay or refusal changed wallet")
	}
}

func TestSLGAlliancePlayerDelivery(t *testing.T) {
	if mode := os.Getenv("SLG_REWARD_CHILD"); mode != "" {
		rewardChild(t, mode)
		return
	}
	if os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI") == "" || os.Getenv("ROOST_DATAENGINE_IT_REDIS_ADDR") == "" || os.Getenv("ROOST_DATAENGINE_IT_NATS_URL") == "" {
		t.Skip("run scripts/test-slg-alliance-player.sh")
	}
	runAllianceActivity(t, func(t *testing.T, prefix string, key activity.Key) {
		for _, sid := range []int32{1000, 1001} {
			root := filepath.Join(os.Getenv("ROOST_SLG_REWARD_WAL"), fmt.Sprint(sid))
			check(t, os.MkdirAll(root, 0700))
			runRewardProcess(t, "write", sid, prefix, root)
			runRewardProcess(t, "recover", sid, prefix, root)
			runRewardProcess(t, "verify", sid, prefix, root)
		}
	})
	// 仅成功后删除本次唯一库；失败保留数据库、WAL 与子进程日志。
	mongo, e := mongodriver.NewClient(fmongo.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")), mongodriver.IndexMigrationPolicy{})
	check(t, e)
	defer mongo.Close(context.Background())
	check(t, mongo.Database(os.Getenv("ROOST_SLG_REWARD_DB")).Drop(context.Background()))
	t.Log("same activity workflow: both static homes credit generated Player DAO; two unprojected strict WAL records per home survive SIGKILL; cold wallet checked before replay; third process confirms one permanent receipt and 10 gold each; wrong home and changed amount refused")
}
func runRewardProcess(t *testing.T, mode string, sid int32, prefix, root string) {
	t.Helper()
	log, e := os.Create(filepath.Join(root, mode+".log"))
	check(t, e)
	defer log.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSLGAlliancePlayerDelivery$", "-test.timeout=45s")
	// exec 环境的同名项以后者覆盖；每个原服拥有单独 WAL，绝不并发打开同一路径。
	cmd.Env = append(os.Environ(), "SLG_REWARD_CHILD="+mode, "SLG_REWARD_SID="+fmt.Sprint(sid), "SLG_REWARD_PREFIX="+prefix, "ROOST_SLG_REWARD_WAL="+root)
	cmd.Stdout = log
	cmd.Stderr = log
	check(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stopped := false
	defer func() {
		if !stopped {
			_ = cmd.Process.Kill()
			<-done
		}
	}()
	fail := func(e error) { raw, _ := os.ReadFile(log.Name()); t.Fatalf("home %d %s: %v\n%s", sid, mode, e, raw) }
	if mode == "write" {
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case e := <-done:
				stopped = true
				fail(fmt.Errorf("writer exited before kill: %v", e))
			default:
			}
			if _, e := os.Stat(filepath.Join(root, "ready.json")); e == nil {
				check(t, cmd.Process.Kill())
				e := <-done
				stopped = true
				if e == nil {
					fail(fmt.Errorf("SIGKILL not observed"))
				}
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		fail(fmt.Errorf("strict commit barrier timeout"))
	} else {
		select {
		case e := <-done:
			stopped = true
			if e != nil {
				fail(e)
			}
		case <-time.After(30 * time.Second):
			fail(fmt.Errorf("recovery timed out"))
		}
	}
}
