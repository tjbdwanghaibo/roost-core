package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	monster "example.com/mmorpg-practice/game/entities/monster"
	player "example.com/mmorpg-practice/game/entities/player"
	scene "example.com/mmorpg-practice/game/entities/scene"
	trade "example.com/mmorpg-practice/game/entities/trade"
	sender "example.com/mmorpg-practice/game/handler/syncsender"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest/entitytimer"
)

// 崩溃点前子进程提交的 strict 记录数：种子 3（卖家、买家、怪物）+ 挂单、卖家确认、买家受伤、复活登记、
// 两次攻击、拾取、任务领奖、Buff 共 9 条业务命令。关掉后台投影后这些记录全部停在 WAL、一条也没有投影。
const mmoCrashUnprojected = 12

// mmoCrashFacts 是崩溃屏障 / 第三进程核对用的事实：四个实体的 DAO 完整持久字节与两个定时器节点 ID。
type mmoCrashFacts struct {
	Seller, Buyer, Trade, Mob []byte
	BuffTimer, ReviveTimer    int64
	Unprojected               uint64
}

type mmoCrashIDs struct{ seller, buyer, trade, mob, area int64 }

// newMMOCrashIDs 先注册实体种类（构造完整 ID 要按种类查锁档），再构造本用例专用的实体 ID。
func newMMOCrashIDs(t *testing.T) mmoCrashIDs {
	t.Helper()
	registerMMORealHandlers()
	build := func(unique int64, kind entity.EntityKind) int64 {
		id, err := entity.BuildEntityID(unique, kind)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	return mmoCrashIDs{
		seller: build(7101, player.EntityKindPlayer),
		buyer:  build(7102, player.EntityKindPlayer),
		trade:  build(7101, trade.EntityKindTrade),
		mob:    build(7101, monster.EntityKindMonster),
		area:   build(7101, scene.EntityKindScene),
	}
}

// mmoCrashLoad 冷加载（或取已加载的）四个实体并直接读 DAO 持久字节。只能在投递循环启动之前 / 停止之后调用：
// 此时没有 Tick 与之并发，读到的就是已提交状态。
func mmoCrashLoad(t *testing.T, h *mmoRealRuntime, ids mmoCrashIDs) (seller, buyer entity.IThreadSafeEntity, facts mmoCrashFacts) {
	t.Helper()
	ctx := context.Background()
	get := func(id int64, category entity.EntityCategory) entity.IThreadSafeEntity {
		e, err := h.access.Get(ctx, id, category)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	seller, buyer = get(ids.seller, entity.EntityCategoryOther), get(ids.buyer, entity.EntityCategoryOther)
	s, b := seller.(*player.Player).Dao(), buyer.(*player.Player).Dao()
	facts = mmoCrashFacts{
		Seller:      s.Marshal(),
		Buyer:       b.Marshal(),
		Trade:       get(ids.trade, entity.EntityCategoryWorld).(*trade.Trade).Dao().Marshal(),
		Mob:         get(ids.mob, entity.EntityCategoryWorld).(*monster.Monster).Dao().Marshal(),
		BuffTimer:   s.GetBuffTimer(),
		ReviveTimer: b.GetReviveTimer(),
	}
	return seller, buyer, facts
}

func (f mmoCrashFacts) sameState(other mmoCrashFacts) bool {
	return bytes.Equal(f.Seller, other.Seller) && bytes.Equal(f.Buyer, other.Buyer) && bytes.Equal(f.Trade, other.Trade) && bytes.Equal(f.Mob, other.Mob) && f.BuffTimer == other.BuffTimer && f.ReviveTimer == other.ReviveTimer
}

// mmoCrashReplayCombat 用原参数重放崩溃前已确认的战斗与领奖命令：同一输入序号 / 已拾取 / 已领奖都按幂等返回，
// 怪物已死后的受伤请求被拒；任何一条都不能改状态。
func mmoCrashReplayCombat(t *testing.T, s *sender.MmoGameplaySender, ids mmoCrashIDs) {
	t.Helper()
	ctx := context.Background()
	if err := s.MultiSync_MMORPGStrike(ctx, ids.seller, ids.mob, 2, mmoRealT0+1000); err != nil {
		t.Fatal("strike replay", err)
	}
	if err := s.MultiSync_MMORPGLoot(ctx, ids.seller, ids.mob); err != nil {
		t.Fatal("loot replay", err)
	}
	if err := s.Sync_MMORPGQuest(ctx, ids.seller); err != nil {
		t.Fatal("quest replay", err)
	}
	if err := s.MultiSync_MMORPGHurt(ctx, ids.buyer, ids.mob); err == nil {
		t.Fatal("a dead monster hurt the buyer again")
	}
	if err := s.MultiSync_MMORPGOffer(ctx, ids.trade, ids.seller, ids.buyer, 10, 1, mmoRealT0); err != nil {
		t.Fatal("offer replay", err)
	}
}

// 崩溃点选“野外战斗进行中”：买家在交易窗口开着时被怪物击倒、复活在计时；卖家击杀怪物、拾取并领了任务奖，
// 刚上了 Buff（计时中）；双方的托管交易已挂单、卖家已确认（卖家 1 件物品、买家 10 金币托管在 Trade 实体里）。
// 这是 MMORPG 最典型的在途状态组合：两种 Player 定时器节点（Buff、复活）同时在计时，托管资产不在任何一方
// 背包里，战斗奖励已发。全部 12 条 strict 记录已确认、未投影时 SIGKILL 子进程；新进程从未投影 WAL 恢复。
//
// 恢复后的业务：离线一小时，Buff 与复活都已过期，投递循环各补触发一次；交易超过期限，由服务内部按到期清理
// 退还托管资产，买家迟到的确认被拒。只终止自己启动的写入子进程；不模拟磁盘断电或 strict 回复未知。
func TestMMORPGRealCrashRecovery(t *testing.T) {
	role := os.Getenv("ROOST_MMORPG_CHILD")
	root := os.Getenv("ROOST_MMORPG_WAL")
	ids := newMMOCrashIDs(t)
	ctx := context.Background()
	if role == "write" {
		// 本用例没有配置目录；子进程被 SIGKILL 不会执行任何 t.Cleanup，所以也不用 t.TempDir，产物都在 WAL 根下。
		h := openMMORealRuntime(t, root, true, mmoRealT0)
		s := sender.NewMmoGameplaySender(h.scheduler)
		for _, c := range []struct {
			id   int64
			kind entity.EntityKind
		}{{ids.seller, player.EntityKindPlayer}, {ids.buyer, player.EntityKindPlayer}, {ids.trade, trade.EntityKindTrade}, {ids.mob, monster.EntityKindMonster}} {
			if _, err := h.access.Create(&entity.EntityCreateParam{IsCreate: true, Id: c.id, Kind: c.kind}); err != nil {
				t.Fatal(err)
			}
		}
		h.seed(t, ids.seller, 50, 3, 100, ids.area)
		h.seed(t, ids.buyer, 50, 0, 25, ids.area)
		h.seed(t, ids.mob, 0, 0, 20, ids.area)
		if err := s.MultiSync_MMORPGOffer(ctx, ids.trade, ids.seller, ids.buyer, 10, 1, mmoRealT0); err != nil {
			t.Fatal(err)
		}
		if err := s.MultiSync_MMORPGResolveTrade(ctx, ids.trade, ids.seller, ids.buyer, ids.seller, 1, mmoRealT0); err != nil {
			t.Fatal(err)
		}
		if err := s.MultiSync_MMORPGHurt(ctx, ids.buyer, ids.mob); err != nil {
			t.Fatal(err)
		}
		if err := s.Sync_MMORPGRevive(ctx, ids.buyer); err != nil {
			t.Fatal(err)
		}
		if err := s.MultiSync_MMORPGStrike(ctx, ids.seller, ids.mob, 1, mmoRealT0); err != nil {
			t.Fatal(err)
		}
		if err := s.MultiSync_MMORPGStrike(ctx, ids.seller, ids.mob, 2, mmoRealT0+1000); err != nil {
			t.Fatal(err)
		}
		if err := s.MultiSync_MMORPGLoot(ctx, ids.seller, ids.mob); err != nil {
			t.Fatal(err)
		}
		if err := s.Sync_MMORPGQuest(ctx, ids.seller); err != nil {
			t.Fatal(err)
		}
		if err := s.Sync_MMORPGBuff(ctx, ids.seller, 7); err != nil {
			t.Fatal(err)
		}
		seller, buyer, facts := mmoCrashLoad(t, h, ids)
		sd, bd := seller.(*player.Player).Dao(), buyer.(*player.Player).Dao()
		stats := h.projector.Stats()
		if stats.Projected != 0 || stats.WALUnacked != mmoCrashUnprojected {
			t.Fatalf("missing strict crash boundary: projected=%d unacked=%d want 0 / %d", stats.Projected, stats.WALUnacked, mmoCrashUnprojected)
		}
		if sd.GetBuffValue() != 7 || facts.BuffTimer == 0 || sd.GetItems() != 3 || sd.GetGold() != 55 || bd.GetHP() != 0 || facts.ReviveTimer == 0 || bd.GetGold() != 40 || sd.TimersLen() != 1 || bd.TimersLen() != 1 {
			t.Fatal("crash point is not the intended in-flight state")
		}
		facts.Unprojected = stats.WALUnacked
		raw, _ := json.Marshal(facts)
		if err := os.WriteFile(filepath.Join(root, "ready.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
		select {} // 父进程读到屏障后 SIGKILL；不会执行 Runtime / Nest cleanup。
	}
	if role == "verify" {
		h := openMMORealRuntime(t, root, false, mmoRealT0+2*time.Hour.Milliseconds())
		raw, err := os.ReadFile(filepath.Join(root, "claimed.json"))
		if err != nil {
			t.Fatal(err)
		}
		var expected mmoCrashFacts
		if err := json.Unmarshal(raw, &expected); err != nil {
			t.Fatal(err)
		}
		seller, buyer, facts := mmoCrashLoad(t, h, ids)
		if !facts.sameState(expected) {
			t.Fatal("cold facts differ before replay")
		}
		if _, ok := player.TimerDriver.DueOf(seller); ok {
			t.Fatal("seller reloaded with a deadline after its Buffs expired")
		}
		if _, ok := player.TimerDriver.DueOf(buyer); ok {
			t.Fatal("buyer reloaded with a deadline after reviving")
		}
		s := sender.NewMmoGameplaySender(h.scheduler)
		mmoCrashReplayCombat(t, s, ids)
		if err := s.MultiSync_MMORPGResolveTrade(ctx, ids.trade, ids.seller, ids.buyer, 0, 3, h.clock.Load()); err != nil {
			t.Fatal("expiry replay", err)
		}
		if err := s.MultiSync_MMORPGResolveTrade(ctx, ids.trade, ids.seller, ids.buyer, ids.seller, 1, mmoRealT0); err == nil {
			t.Fatal("the original seller confirmation reopened a refunded trade")
		}
		if err := s.Sync_MMORPGRevive(ctx, ids.buyer); err == nil {
			t.Fatal("a living buyer registered another revive")
		}
		if _, _, again := mmoCrashLoad(t, h, ids); !again.sameState(expected) {
			t.Fatal("third-process replay duplicated rewards, refunds or timers")
		}
		return
	}
	if root == "" {
		t.Fatal("unique WAL directory required")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "child.log")
	out, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestMMORPGRealCrashRecovery$", "-test.v", "-test.timeout=90s")
	child.Env = append(os.Environ(), "ROOST_MMORPG_CHILD=write")
	child.Stdout = out
	child.Stderr = out
	if err := child.Start(); err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { err := child.Wait(); _ = out.Close(); done <- err; close(done) }()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			_ = child.Process.Kill()
			<-done
		}
	})
	var raw []byte
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(25 * time.Millisecond)
	defer poll.Stop()
	for raw == nil {
		value, err := os.ReadFile(filepath.Join(root, "ready.json"))
		if err == nil {
			raw = value
			break
		}
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			log, _ := os.ReadFile(logPath)
			t.Fatalf("child failed before barrier: %v\n%s", err, log)
		case <-deadline.C:
			t.Fatal("child deadline: " + logPath)
		case <-poll.C:
		}
	}
	var barrier mmoCrashFacts
	if err := json.Unmarshal(raw, &barrier); err != nil {
		t.Fatal(err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("child did not crash")
	}

	// 新进程：同一 WAL、同一库，业务时间推进一小时（Buff 与复活都已过期，交易超过 5 秒期限）。
	h := openMMORealRuntime(t, root, false, mmoRealT0+time.Hour.Milliseconds())
	s := sender.NewMmoGameplaySender(h.scheduler)
	seller, buyer, facts := mmoCrashLoad(t, h, ids)
	if !facts.sameState(barrier) {
		t.Fatal("WAL recovery lost or changed escrow / combat rewards / Buff / revive timer")
	}
	// 冷加载时 Host 把两个玩家的存量最早期限报告给驱动索引。到这里投递循环还没启动，上面直接读 DAO 不会与 Tick 并发。
	if due, ok := player.TimerDriver.DueOf(seller); !ok || due.UnixMilli() != mmoRealT0+2000 {
		t.Fatalf("cold load did not report the stored Buff deadline: due=%v ok=%v", due, ok)
	}
	if due, ok := player.TimerDriver.DueOf(buyer); !ok || due.UnixMilli() != mmoRealT0+3000 {
		t.Fatalf("cold load did not report the stored revive deadline: due=%v ok=%v", due, ok)
	}
	// 客户端重试崩溃前已确认的命令（原参数）：战斗、拾取、领奖、挂单、卖家确认都按原结果返回，重复复活复用
	// 原期限；状态逐字节不变，不重复发奖、不重复托管。
	mmoCrashReplayCombat(t, s, ids)
	if err := s.MultiSync_MMORPGResolveTrade(ctx, ids.trade, ids.seller, ids.buyer, ids.seller, 1, mmoRealT0); err != nil {
		t.Fatal("seller confirmation replay", err)
	}
	if err := s.Sync_MMORPGRevive(ctx, ids.buyer); err != nil {
		t.Fatal("revive replay", err)
	}
	if _, _, again := mmoCrashLoad(t, h, ids); !again.sameState(barrier) {
		t.Fatal("retrying confirmed commands changed recovered state")
	}

	// 启动驱动自己的投递循环（与进程装配同一个入口），用例不手发 Tick、不调用 DeliverDue。循环发现两个已过期的
	// 玩家，各投递一次加载后的补触发：Buff 清除、买家复活。离线一小时只补这一次，不追帧。业务时钟在两次推进之间
	// 冻结，循环按 MaxWait 重读时钟，所以“何时到期”完全由用例推进时钟决定，计数是确定的。
	deliveredBefore, redeliveredBefore := mmoTimerCounter(entitytimer.DeliveredMetric), mmoTimerCounter(entitytimer.RedeliveredMetric)
	if err := player.TimerDriver.Start(); err != nil {
		t.Fatal(err)
	}
	awaitMMOLoop(t, "the overdue Buff expiry and revive", deliveredBefore, 2, func() bool {
		a, b := h.view(t, ids.seller), h.view(t, ids.buyer)
		return a.BuffValue == 0 && a.BuffTimer == 0 && a.Timers == 0 && b.HP == 100 && b.ReviveTimer == 0 && b.Timers == 0
	})
	if _, ok := player.TimerDriver.DueOf(seller); ok {
		t.Fatal("expired Buff left a deadline in the driver index")
	}
	if _, ok := player.TimerDriver.DueOf(buyer); ok {
		t.Fatal("finished revive left a deadline in the driver index")
	}

	// 交易已过 5 秒期限：服务内部按到期清理（actorID 0）退还托管资产，重试不重复退；买家迟到的确认被拒。
	for range 3 {
		if err := s.MultiSync_MMORPGResolveTrade(ctx, ids.trade, ids.seller, ids.buyer, 0, 3, h.clock.Load()); err != nil {
			t.Fatal("trade expiry", err)
		}
	}
	if err := s.MultiSync_MMORPGResolveTrade(ctx, ids.trade, ids.seller, ids.buyer, ids.buyer, 1, h.clock.Load()); err == nil {
		t.Fatal("late buyer confirmation completed a refunded trade")
	}
	if v := h.view(t, ids.trade); v.TradeStatus != 3 {
		t.Fatalf("trade status=%d want refunded", v.TradeStatus)
	}

	// 下一个期限：卖家重新上 Buff，新节点的期限（当前业务时间 + 2 秒）经提交进入索引；期限之前业务时间冻结，
	// 循环不投递。把业务时间直接推进到这个期限，循环（至多 MaxWait 后重读时钟）按时投递一次。
	if err := s.Sync_MMORPGBuff(ctx, ids.seller, 5); err != nil {
		t.Fatal(err)
	}
	next := h.clock.Load() + 2000
	awaitMMOLoop(t, "nothing before the new Buff deadline", deliveredBefore, 2, func() bool {
		due, ok := player.TimerDriver.DueOf(seller)
		return ok && due.UnixMilli() == next
	})
	if v := h.view(t, ids.seller); v.BuffValue != 5 {
		t.Fatal("the loop expired the new Buff before its deadline")
	}
	h.clock.Store(next)
	awaitMMOLoop(t, "the next Buff expiry at its deadline", deliveredBefore, 3, func() bool {
		v := h.view(t, ids.seller)
		return v.BuffValue == 0 && v.BuffTimer == 0 && v.Timers == 0
	})
	if got := mmoTimerCounter(entitytimer.RedeliveredMetric) - redeliveredBefore; got != 0 {
		t.Fatalf("driver redelivered %d ticks: a Tick commit did not report its next deadline", got)
	}

	// 先停投递循环再读 DAO 与关 Nest（close 里也会再停一次）。
	if err := player.TimerDriver.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if got := mmoTimerCounter(entitytimer.DeliveredMetric) - deliveredBefore; got != 3 {
		t.Fatalf("driver loop delivered %d ticks in total, want 3", got)
	}
	_, _, final := mmoCrashLoad(t, h, ids)
	a, b := h.view(t, ids.seller), h.view(t, ids.buyer)
	if a.Gold != 55 || a.Items != 4 || a.QuestKills != 1 || a.QuestClaimed != 1 || b.Gold != 50 || b.Items != 0 || b.HP != 100 || final.BuffTimer != 0 || final.ReviveTimer != 0 {
		t.Fatalf("recovery changed economy: seller gold=%d items=%d kills=%d claim=%d; buyer gold=%d items=%d hp=%d", a.Gold, a.Items, a.QuestKills, a.QuestClaimed, b.Gold, b.Items, b.HP)
	}
	expectedRaw, _ := json.Marshal(final)
	if err := os.WriteFile(filepath.Join(root, "claimed.json"), expectedRaw, 0600); err != nil {
		t.Fatal(err)
	}
	h.close(t)
	if t.Failed() {
		return
	}
	verify := exec.Command(os.Args[0], "-test.run=^TestMMORPGRealCrashRecovery$", "-test.v", "-test.timeout=60s")
	verify.Env = append(os.Environ(), "ROOST_MMORPG_CHILD=verify")
	if output, err := verify.CombinedOutput(); err != nil {
		t.Fatalf("third-process cold verification failed: %v\n%s", err, output)
	}
	if err := h.mongo.Database(h.database).Drop(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("SIGKILL: %d unprojected strict records; escrow, combat rewards, Buff and revive timers restored byte for byte; driver loop delivered one catch-up per overdue player after cold load and the next Buff expiry at its deadline; expired trade refunded once; third process replayed without duplicating rewards or refunds", barrier.Unprojected)
}
