package ai

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	coreflow "github.com/tjbdwanghaibo/roost-core/gameplay/actionflow"
	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
)

// AI 心跳（D21 C4）与定时冻结（C3）。

// pacedStrategy 是带 TickInterval 的测试策略（interval 可在运行中改：策略随自身状态改周期）。
type pacedStrategy struct {
	controllerTestStrategy
	mu        sync.Mutex
	interval  time.Duration
	ticks     int
	outsideTx int // 不在 Nest 事务里的决策次数
}

func (s *pacedStrategy) TickInterval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.interval
}

func (s *pacedStrategy) Tick(*Context, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ticks++
	if nest.CurrentRollbackTx() == nil {
		s.outsideTx++
	}
}

func (s *pacedStrategy) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ticks
}

type beatTestEntity struct{ *entity.EntityBase }

func (e *beatTestEntity) Base() *entity.EntityBase { return e.EntityBase }

func newBeatControllerForTest(t *testing.T, hooks ControllerHooks) (*Controller, *heartbeat.Driver) {
	t.Helper()
	d := heartbeat.NewDriver(heartbeat.Config{Name: "ai-test", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	owner := &beatTestEntity{entity.NewEntityBase(1<<20, entity.EntityCategoryOther, true)}
	c, err := NewBeatController(hooks, BeatConfig{Host: d.NewHost(owner)})
	if err != nil {
		t.Fatal(err)
	}
	return c, d
}

// 周期由策略决定（不实现取 BeatConfig.Period，返回 0 停），没有策略或无定时的冻结时停；每拍之后按策略重新取档。
func TestTheAIBeatsAtTheStrategysIntervalAndStopsWhenIdleOrFrozen(t *testing.T) {
	c, d := newBeatControllerForTest(t, ControllerHooks{})
	state := func() string {
		if c.BeatReg() == nil {
			return "none"
		}
		return fmt.Sprintf("%v %v", c.BeatReg().Active(), c.BeatReg().Period())
	}
	if state() != "none" {
		t.Fatalf("no strategy: %s", state())
	}
	plain := &controllerTestStrategy{name: "plain"}
	if err := c.SetStrategy(plain); err != nil {
		t.Fatal(err)
	}
	if state() != "true 500ms" || d.Registered() != 1 {
		t.Fatalf("strategy without TickInterval: %s registered %d", state(), d.Registered())
	}
	paced := &pacedStrategy{controllerTestStrategy: controllerTestStrategy{name: "paced"}, interval: 300 * time.Millisecond}
	if err := c.SetStrategy(paced); err != nil {
		t.Fatal(err)
	}
	if state() != "true 200ms" {
		t.Fatalf("300ms strategy: %s (want the 200ms tier)", state())
	}
	c.Freeze()
	if state() != "false 200ms" || d.Registered() != 0 {
		t.Fatalf("frozen: %s registered %d", state(), d.Registered())
	}
	c.Recover()
	paced.mu.Lock()
	paced.interval = time.Second
	paced.mu.Unlock()
	if err := c.onBeat(heartbeat.Beat{Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if state() != "true 1s" || paced.count() != 1 {
		t.Fatalf("strategy slowed down: %s ticks %d", state(), paced.count())
	}
	paced.mu.Lock()
	paced.interval = 0
	paced.mu.Unlock()
	if err := c.onBeat(heartbeat.Beat{Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if state() != "false 1s" {
		t.Fatalf("strategy that needs no beats: %s", state())
	}
	paced.mu.Lock()
	paced.interval = time.Second
	paced.mu.Unlock()
	c.Recover() // 重新对齐
	c.Shutdown("leaving")
	if state() != "false 1s" || d.Registered() != 0 {
		t.Fatalf("after Shutdown: %s registered %d", state(), d.Registered())
	}
}

// 定时冻结（C3）：冻结期间心跳照常跳但不决策，到期后的第一拍恢复并决策；可选同时结束在途动作；Freeze 取消定时。
func TestFreezeForRecoversOnTheFirstBeatAfterItEnds(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	ended := 0
	c, _ := newBeatControllerForTest(t, ControllerHooks{Now: func() time.Time { return now }, EndActions: func(coreflow.ActionReason) { ended++ }})
	paced := &pacedStrategy{controllerTestStrategy: controllerTestStrategy{name: "paced"}, interval: 100 * time.Millisecond}
	if err := c.SetStrategy(paced); err != nil {
		t.Fatal(err)
	}
	c.FreezeFor(time.Second, true)
	if !c.Frozen() || c.FrozenUntil() != now.Add(time.Second) || !c.BeatReg().Active() || ended != 1 {
		t.Fatalf("FreezeFor: frozen %v until %v beating %v ended %d", c.Frozen(), c.FrozenUntil(), c.BeatReg().Active(), ended)
	}
	_ = c.onBeat(heartbeat.Beat{Now: now.Add(999 * time.Millisecond)})
	if paced.count() != 0 || !c.Frozen() {
		t.Fatalf("before the end: ticks %d frozen %v", paced.count(), c.Frozen())
	}
	_ = c.onBeat(heartbeat.Beat{Now: now.Add(time.Second)})
	if paced.count() != 1 || c.Frozen() || !c.FrozenUntil().IsZero() {
		t.Fatalf("at the end: ticks %d frozen %v until %v", paced.count(), c.Frozen(), c.FrozenUntil())
	}
	c.FreezeFor(time.Second, false)
	c.Freeze()
	_ = c.onBeat(heartbeat.Beat{Now: now.Add(time.Hour)})
	if !c.Frozen() || c.BeatReg().Active() || ended != 1 {
		t.Fatalf("Freeze after FreezeFor: frozen %v beating %v ended %d", c.Frozen(), c.BeatReg().Active(), ended)
	}
}

// 经真实 Nest 与启动的心跳驱动：AI 在实体锁与事务内按策略周期决策；冻结时驱动上没有它的登记；定时冻结到期后自己
// 恢复、接着决策。

const kindAIHolder entity.EntityKind = 244

var runAI = nest.NewHandlerName("ai_test.run")

func init() {
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kindAIHolder, Category: entity.EntityCategoryOther, BusinessPool: entity.BusinessPoolShort})
	nest.MustRegisterHandlerWithMeta(runAI, func(es []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
		return nil, params[0].(func(*aiHolder) error)(es[0].(*aiHolder))
	}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityAsync})
}

type aiHolder struct {
	*entity.EntityBase
	host *heartbeat.Host
	ai   *Controller
}

func (h *aiHolder) Base() *entity.EntityBase             { return h.EntityBase }
func (h *aiHolder) OnDestroy(entity.EntityDestroyReason) { h.host.OnDestroy() }

type nullAICommitter struct{}

func (nullAICommitter) Commit(context.Context, dataengine.CommitRecord) error { return nil }

func TestTheAIDecidesOnItsOwnHeartbeat(t *testing.T) {
	manager := entity.NewEntityManager()
	engine := nest.NewEngine(
		nest.NestOptionWithGetter(entity.NewManagerAccess(manager)),
		nest.NestOptionWithTransactionCommitter(nullAICommitter{}),
		nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 2, QueueCap: 1024}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 64}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 64}),
	)
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
	driver := heartbeat.NewDriver(heartbeat.Config{Name: "ai-nest", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err := driver.Bind(engine, manager.Get); err != nil {
		t.Fatal(err)
	}
	if err := driver.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = driver.Stop(context.Background()) })
	id, err := entity.BuildEntityID(1, kindAIHolder)
	if err != nil {
		t.Fatal(err)
	}
	h := &aiHolder{EntityBase: entity.NewEntityBase(id, entity.EntityCategoryOther, false, kindAIHolder)}
	h.host = driver.NewHost(h)
	h.ai, err = NewBeatController(ControllerHooks{OnError: func(err error) { t.Errorf("ai error: %v", err) }}, BeatConfig{Host: h.host})
	if err != nil {
		t.Fatal(err)
	}
	manager.Add(h)
	run := func(fn func(*aiHolder) error) {
		t.Helper()
		if _, err := engine.Request(context.Background(), runAI, h.ID(), nest.NewParams(fn)); err != nil {
			t.Fatal(err)
		}
	}
	waitTicks := func(strategy *pacedStrategy, n int) {
		t.Helper()
		deadline := time.Now().Add(60 * time.Second)
		for strategy.count() < n {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %d decisions, got %d", n, strategy.count())
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	paced := &pacedStrategy{controllerTestStrategy: controllerTestStrategy{name: "paced"}, interval: 50 * time.Millisecond}
	run(func(h *aiHolder) error { return h.ai.SetStrategy(paced) })
	waitTicks(paced, 3)
	run(func(h *aiHolder) error { h.ai.Freeze(); return nil })
	run(func(*aiHolder) error { return nil }) // 等冻结那条消息的提交后回调（登记停止生效）收尾
	if driver.Registered() != 0 {
		t.Fatalf("frozen AI still registered: %d", driver.Registered())
	}
	frozenAt := paced.count()
	run(func(h *aiHolder) error { h.ai.FreezeFor(150*time.Millisecond, false); return nil })
	waitTicks(paced, frozenAt+2)
	run(func(*aiHolder) error { return nil })
	var frozen bool
	run(func(h *aiHolder) error { frozen = h.ai.Frozen(); return nil })
	paced.mu.Lock()
	outside := paced.outsideTx
	paced.mu.Unlock()
	if frozen || driver.Registered() != 1 || outside != 0 {
		t.Fatalf("after the timed freeze: frozen %v registered %d decisions outside a transaction %d", frozen, driver.Registered(), outside)
	}
}
