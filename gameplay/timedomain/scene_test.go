package timedomain_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/gameplay/actionflow"
	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
	"github.com/tjbdwanghaibo/roost-core/gameplay/timedomain"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

// 本测试二进制里唯一的实体种类。
const memberKind entity.EntityKind = 241

func init() {
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: memberKind, Category: entity.EntityCategoryOther, BusinessPool: entity.BusinessPoolShort})
}

type nullCommitter struct{}

func (nullCommitter) Commit(context.Context, dataengine.CommitRecord) error { return nil }

// sceneFields 扮演场景实体的 DAO：三个 nopersist,sync 字段，同步给客户端（测试里只存值）。
type sceneFields struct{ state timedomain.State }

func (f *sceneFields) TimeDomain() timedomain.State     { return f.state }
func (f *sceneFields) SetTimeDomain(s timedomain.State) { f.state = s }

type member struct{ *entity.EntityBase }

func (m *member) Base() *entity.EntityBase { return m.EntityBase }

// tick 是动作一拍看到的时间：ActionContext 的 Now / Elapsed 与这条消息冻结的业务时间。
type tick struct {
	now, business time.Time
	elapsed       time.Duration
}

// walkAction 是一个一直走的动作，记下每一拍的时间。
type walkAction struct {
	mu      sync.Mutex
	started time.Time
	ticks   []tick
}

func (*walkAction) Kind() actionflow.ActionKind { return 1 }
func (a *walkAction) Start(ctx *actionflow.ActionContext) error {
	a.started = ctx.Now
	return nil
}
func (*walkAction) Cancel(*actionflow.ActionContext, string) {}
func (a *walkAction) Tick(ctx *actionflow.ActionContext) (bool, actionflow.ActionResult) {
	a.mu.Lock()
	a.ticks = append(a.ticks, tick{now: ctx.Now, business: fctx.Now(), elapsed: ctx.Elapsed})
	a.mu.Unlock()
	return false, actionflow.ActionResult{}
}
func (a *walkAction) snapshot() []tick {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]tick(nil), a.ticks...)
}

// 场景 2 倍速：场景里成员的 BeatRunner 经心跳选项挂上场景的时间域，动作每拍的 ActionContext.Now 是域时间、
// Elapsed 是域时间的经过——正好是这一拍业务经过的两倍；调度仍按真实时间（档位 100ms 不变）。经真实 Nest 引擎与
// 运行中的心跳驱动。
func TestSceneMembersActOnTheSceneTimeDomain(t *testing.T) {
	manager := entity.NewEntityManager()
	engine := nest.NewEngine(nest.NestOptionWithGetter(entity.NewManagerAccess(manager)), nest.NestOptionWithTransactionCommitter(nullCommitter{}),
		nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 2, QueueCap: 1024}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 64}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 64}))
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
	driver := heartbeat.NewDriver(heartbeat.Config{Name: "scene-test"})
	if err := driver.Bind(engine, func(id int64) entity.IThreadSafeEntity { return manager.Get(id) }); err != nil {
		t.Fatal(err)
	}
	if err := driver.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = driver.Stop(context.Background()) })

	// 场景实体：初始化阶段构造时间域，改成 2 倍速（不在事务里，立即发布）。
	scene, err := timedomain.New(&sceneFields{}, timedomain.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := scene.SetRate(2); err != nil {
		t.Fatal(err)
	}

	// 成员实体：BeatRunner 的心跳登记挂上场景的时间域。
	id, err := entity.BuildEntityID(7, memberKind)
	if err != nil {
		t.Fatal(err)
	}
	m := &member{entity.NewEntityBase(id, entity.EntityCategoryOther, true, memberKind)}
	registry := actionflow.NewRegistry()
	if err := registry.RegisterAction(1, func(param any) (actionflow.Action, error) { return param.(actionflow.Action), nil }); err != nil {
		t.Fatal(err)
	}
	host := driver.NewHost(m)
	t.Cleanup(host.OnDestroy)
	runner, err := actionflow.NewBeatRunner(actionflow.ActionRunnerConfig{
		Registry:     registry,
		GroupForKind: func(actionflow.ActionKind) (actionflow.ActionGroup, bool) { return 1, true },
		Hooks:        actionflow.ActionRunnerHooks{OnError: func(err error) { t.Errorf("runner: %v", err) }},
	}, actionflow.BeatConfig{Host: host, Options: []heartbeat.RegOption{heartbeat.WithTimeDomain(scene)}})
	if err != nil {
		t.Fatal(err)
	}
	walk := &walkAction{}
	// 心跳之外调用 runner 时不带时间就是业务时间（fctx.Now）；在场景里要域时间，显式传 scene.Now()。
	if _, err := runner.Runner().Start(1, walk, 0, scene.Now()); err != nil {
		t.Fatal(err)
	}
	manager.Add(m)

	deadline := time.Now().Add(30 * time.Second)
	for len(walk.snapshot()) < 4 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d ticks in 30s", len(walk.snapshot()))
		}
		time.Sleep(10 * time.Millisecond)
	}
	ticks := walk.snapshot()
	for i := 1; i < len(ticks); i++ {
		previous, current := ticks[i-1], ticks[i]
		// 按墙上时间比（域时间不带单调时钟读数，Round(0) 去掉业务时间里的单调读数）。
		business := current.business.Round(0).Sub(previous.business.Round(0))
		if current.elapsed != 2*business || current.now.Sub(previous.now) != current.elapsed {
			t.Fatalf("tick %d: business moved %v, domain moved %v, Elapsed %v; want the domain and Elapsed at 2x the business time",
				i, business, current.now.Sub(previous.now), current.elapsed)
		}
	}
	if !ticks[0].now.After(walk.started) {
		t.Fatalf("the first tick's domain time %v is not after the action's start %v", ticks[0].now, walk.started)
	}
}
