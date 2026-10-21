package actionflow

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
	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
)

// 真实 Nest 引擎 + 启动的心跳驱动：Agent 的任务心跳在实体锁与事务内 tick 任务，计划步骤的时限与重试经它生效。

const kindAgentHolder entity.EntityKind = 242

var runAgent = nest.NewHandlerName("actionflow_test.run")

func init() {
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kindAgentHolder, Category: entity.EntityCategoryOther, BusinessPool: entity.BusinessPoolShort})
	nest.MustRegisterHandlerWithMeta(runAgent, func(es []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
		return nil, params[0].(func(*agentHolder) error)(es[0].(*agentHolder))
	}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityAsync})
}

type agentHolder struct {
	*entity.EntityBase
	host  *heartbeat.Host
	agent *Agent
	mu    sync.Mutex
	log   []string
}

func (h *agentHolder) Base() *entity.EntityBase             { return h.EntityBase }
func (h *agentHolder) OnDestroy(entity.EntityDestroyReason) { h.host.OnDestroy() }

func (h *agentHolder) OnActionEnd(id int64, _ ActionKind, reason ActionReason) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.log = append(h.log, fmt.Sprintf("action %d %d", id, reason.ToActionResult(ActionStatusSuccess).Status))
}

func (h *agentHolder) OnMissionEnd(m Mission, _ ActionReason) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.log = append(h.log, fmt.Sprintf("mission %d %d", m.ID(), m.Status()))
}

func (h *agentHolder) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.log...)
}

type nullAgentCommitter struct{}

func (nullAgentCommitter) Commit(context.Context, dataengine.CommitRecord) error { return nil }

func TestPlanStepTimeoutsRunOnTheRealMissionHeartbeat(t *testing.T) {
	manager := entity.NewEntityManager()
	engine := nest.NewEngine(
		nest.NestOptionWithGetter(entity.NewManagerAccess(manager)),
		nest.NestOptionWithTransactionCommitter(nullAgentCommitter{}),
		nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 2, QueueCap: 1024}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 64}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 64}),
	)
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
	driver := heartbeat.NewDriver(heartbeat.Config{Name: "actionflow-nest", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err := driver.Bind(engine, manager.Get); err != nil {
		t.Fatal(err)
	}
	if err := driver.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = driver.Stop(context.Background()) })

	id, err := entity.BuildEntityID(1, kindAgentHolder)
	if err != nil {
		t.Fatal(err)
	}
	h := &agentHolder{EntityBase: entity.NewEntityBase(id, entity.EntityCategoryOther, false, kindAgentHolder)}
	h.host = driver.NewHost(h)
	registry := newActionOnlyRegistry(t)
	if err := registry.RegisterMission(1, func() Mission { return NewPlanMission(1) }); err != nil {
		t.Fatal(err)
	}
	h.agent, err = NewAgent(h, AgentConfig{
		Registry:     registry,
		GroupForKind: func(ActionKind) (ActionGroup, bool) { return 1, true },
		Beat:         &BeatConfig{Host: h.host, Period: 500 * time.Millisecond}, // 动作组慢跳，只看任务心跳
		Observer:     h,
		OnError:      func(err error) { t.Errorf("agent error: %v", err) },
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.Add(h)
	run := func(fn func(*agentHolder) error) {
		t.Helper()
		if _, err := engine.Request(context.Background(), runAgent, h.ID(), nest.NewParams(fn)); err != nil {
			t.Fatal(err)
		}
	}

	plan := MissionPlan{Period: 50 * time.Millisecond, Steps: []MissionStep{{Action: 1, Param: &runnerTestAction{label: "stuck"}, Timeout: 150 * time.Millisecond, Retry: 1}}}
	run(func(h *agentHolder) error { return h.agent.SetMission(1, plan) })
	deadline := time.Now().Add(60 * time.Second)
	for len(h.seen()) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the mission to time out: %v", h.seen())
		}
		time.Sleep(2 * time.Millisecond)
	}
	// 结束发生在拍的事务里；心跳登记的停止在提交后生效，经同实体的下一条消息等它收尾再读（见 movement 的 settle）。
	run(func(*agentHolder) error { return nil })
	if got := fmt.Sprint(h.seen()); got != "[action 1 5 action 2 5 mission 1 5]" || driver.Registered() != 0 {
		t.Fatalf("observer saw %s registered %d, want two expired attempts, an expired mission and no heartbeats", got, driver.Registered())
	}
}
