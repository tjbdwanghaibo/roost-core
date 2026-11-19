package physicsroom

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/gameplay/level"
	gamephysics "github.com/tjbdwanghaibo/roost-core/gameplay/physics"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
	domain "github.com/tjbdwanghaibo/roost-core/service/physicsroom"
)

var hostKindOnce sync.Once

func TestEntityHostIsolationAndCleanup(t *testing.T) {
	// 241 与本包原生提交测试使用的 240 分开，支持重复运行。
	hostKindOnce.Do(func() {
		if err := entity.RegisterEntityKindCategory(241, 1); err != nil {
			panic(err)
		}
	})
	h, err := NewEntityHost(241, 1, 2, nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 2, QueueCap: 16}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 8}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 8}))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	defer h.Close(ctx)
	a, b := domain.Ref{ID: "a", Epoch: 1}, domain.Ref{ID: "b", Epoch: 2}
	f := gamephysics.Frame{Epoch: 1, Tick: 1, States: []physx.State{{ID: 1, Position: physx.Vec3{3, 0, 0}, Rotation: physx.Quat{0, 0, 0, 1}}}}
	if _, err = h.Commit(ctx, a, domain.Tick{Frame: f}); err != nil {
		t.Fatal(err)
	}
	f.States[0].Position[0] = 99 // 提交后调用者修改不能影响 DAO。
	f.Epoch = 2
	if _, err = h.Commit(ctx, b, domain.Tick{Frame: f}); err != nil {
		t.Fatal(err)
	}
	got, err := h.Snapshot(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if got.States[0].Position[0] != 3 {
		t.Fatalf("frame alias: %+v", got)
	}
	got.States[0].Position[0] = 50
	again, err := h.Snapshot(ctx, a)
	if err != nil || again.States[0].Position[0] != 3 {
		t.Fatalf("snapshot alias: %+v %v", again, err)
	}
	f.Epoch = 1
	f.Tick = 3
	if _, err = h.Commit(ctx, a, domain.Tick{Frame: f}); err == nil {
		t.Fatal("out of order accepted")
	}
	h.Forget(a)
	if _, err = h.Snapshot(ctx, a); !errors.Is(err, domain.ErrMissing) {
		t.Fatal(err)
	}
	got, err = h.Snapshot(ctx, b)
	if err != nil || got.States[0].Position[0] != 99 {
		t.Fatalf("other room damaged: %+v %v", got, err)
	}
	if err = h.Close(ctx); err != nil {
		t.Fatal(err)
	}
	f.Epoch = b.Epoch
	if _, err = h.Commit(ctx, b, domain.Tick{Frame: f}); !errors.Is(err, domain.ErrStopping) {
		t.Fatal(err)
	}
}

// courseFlow：掉进 pit 记一次并请求重生；进 goal 结算；成员离开时结束 run 阶段（用来取消还没回执的动作）。
const courseFlow = `{"Version":1,"Name":"course","Revision":1,"TimeDomain":"tick","Initial":["run"],` +
	`"Variables":[{"Scope":"global","Name":"falls","Default":{"Kind":"int"},"Public":true}],"Stages":[{"ID":"run","Triggers":[` +
	`{"ID":"fall","Event":"region.enter","Conditions":[{"Left":{"EventField":"region"},"Op":"eq","Right":{"Literal":{"Kind":"string","Text":"pit"}}}],"Actions":[` +
	`{"Kind":"add","Scope":"global","Name":"falls","Value":{"Literal":{"Kind":"int","Int":1}}},{"Kind":"request","Name":"physics_respawn","Args":{"actor":{"EventField":"actor"}}}]},` +
	`{"ID":"reach","Event":"region.enter","Conditions":[{"Left":{"EventField":"region"},"Op":"eq","Right":{"Literal":{"Kind":"string","Text":"cp"}}}],"Actions":[` +
	`{"Kind":"request","Name":"physics_checkpoint","Args":{"actor":{"EventField":"actor"},"spawn":{"Literal":{"Kind":"string","Text":"cp"}}}}]},` +
	`{"ID":"quit","Event":"member.leave","Actions":[{"Kind":"end","Name":"run"}]},` +
	`{"ID":"win","Event":"region.enter","Conditions":[{"Left":{"EventField":"region"},"Op":"eq","Right":{"Literal":{"Kind":"string","Text":"goal"}}}],"Actions":[{"Kind":"finish","Result":"clear"}]}]}]}`

// courseSpec 是一条 8m 长的窄道：玩家 1、2 在中间，往 -x 先过检查点区域再掉下去，往 +x 是终点。
func courseSpec(id, instance string) domain.Spec {
	return domain.Spec{ID: id, Instance: instance, LifetimeSeconds: 60,
		Bodies: []physx.Body{
			{ID: 100, Shape: physx.Box, Position: physx.Vec3{0, -.5, 0}, Dimensions: physx.Vec3{4, .5, 2}},
			{ID: 1, Shape: physx.Sphere, Position: physx.Vec3{0, .5, 0}, Dimensions: physx.Vec3{.5}, Mass: 1},
			{ID: 2, Shape: physx.Sphere, Position: physx.Vec3{0, .5, 1.2}, Dimensions: physx.Vec3{.5}, Mass: 1},
		},
		Players: []uint64{1, 2},
		Regions: []gamephysics.Region{
			{ID: "pit", Center: physx.Vec3{0, -5, 0}, HalfExtents: physx.Vec3{40, 2, 40}},
			{ID: "cp", Center: physx.Vec3{-2, .5, 0}, HalfExtents: physx.Vec3{.5, 1, 2}},
			{ID: "goal", Center: physx.Vec3{3, .5, 0}, HalfExtents: physx.Vec3{.5, 1, 2}},
		},
		Spawns: []gamephysics.Spawn{
			{ID: "start", Positions: []physx.Vec3{{0, .5, 0}, {0, .5, 1.2}}},
			{ID: "cp", Positions: []physx.Vec3{{-2, .5, 0}, {-2, .5, 1.2}}},
		},
		DefaultSpawn: "start", Flow: []byte(courseFlow)}
}

// 提交 handler 里任何一步出错，物理帧与流程状态一起撤回；重试同一帧能成功。
// 流程状态没变的提交不交回动作与视图；被取消的动作的回执作废但不算错；结束之后同一笔里的事实不再投递。
func TestRoomEntityRollsBackFrameAndLevelTogether(t *testing.T) {
	hostKindOnce.Do(func() {
		if err := entity.RegisterEntityKindCategory(241, 1); err != nil {
			panic(err)
		}
	})
	h, err := NewEntityHost(241, 1, 4, nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 2, QueueCap: 16}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 8}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 8}))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	defer h.Close(ctx)
	spec := courseSpec("rollback", "")
	program, err := gamephysics.CompileFlow(spec.Flow, spec.Regions, spec.Spawns, level.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	ref := domain.Ref{ID: "rollback", Epoch: 77}
	instance := domain.LevelInstance(ref)
	frame := func(tick uint64) gamephysics.Frame {
		return gamephysics.Frame{Epoch: ref.Epoch, Tick: tick, States: []physx.State{{ID: 1, Position: physx.Vec3{float32(tick), 0, 0}, Rotation: physx.Quat{0, 0, 0, 1}}}}
	}
	fact := func(sequence uint64, kind string, actor int64, region string) level.Event {
		fields := map[string]level.Value{"actor": level.Int(actor)}
		if region != "" {
			fields["region"] = level.String(region)
		}
		return level.Event{Instance: instance, Epoch: ref.Epoch, Source: domain.FactSource, Sequence: sequence, Kind: kind, Fields: fields}
	}
	enter := func(sequence uint64, actor int64, region string) level.Event {
		return fact(sequence, gamephysics.EventRegionEnter, actor, region)
	}
	snapshot := func() string {
		t.Helper()
		state, err := h.Level(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	if _, err = h.Level(ctx, ref); !errors.Is(err, domain.ErrMissing) {
		t.Fatalf("level of an unknown room: %v", err)
	}

	// 首帧：建流程、初始化并开始。
	settled, err := h.Commit(ctx, ref, domain.Tick{Frame: frame(1), Program: program})
	if err != nil || settled.View == nil || settled.View.Phase != level.Running || settled.View.Instance != instance || settled.View.Epoch != ref.Epoch || len(settled.Actions) != 0 {
		t.Fatalf("first tick: %+v %v", settled, err)
	}
	// 掉进 pit：一个待执行的重生，公开变量 falls=1。
	settled, err = h.Commit(ctx, ref, domain.Tick{Frame: frame(2), Facts: []level.Event{enter(1, 1, "pit")}})
	if err != nil || len(settled.Actions) != 1 || settled.Actions[0].Name != gamephysics.ActionRespawn || settled.Actions[0].Args["actor"] != level.Int(1) ||
		settled.View == nil || settled.View.Variables["global/falls"] != level.Int(1) {
		t.Fatalf("fall: %+v %v", settled, err)
	}
	first := settled.Actions[0]
	if want := instance + ":77:1"; first.ID != want {
		t.Fatalf("action ID %q, want %q (instance:epoch:sequence)", first.ID, want)
	}

	// 出错的一笔：第一条事实合法（会改流程状态），第二条的事件没有声明。两份状态都要回到提交之前。
	before := snapshot()
	bad := level.Event{Instance: instance, Epoch: ref.Epoch, Source: domain.FactSource, Sequence: 3, Kind: "bogus.event"}
	settled, err = h.Commit(ctx, ref, domain.Tick{Frame: frame(3), Facts: []level.Event{enter(2, 2, "pit"), bad}})
	if !errors.Is(err, domain.ErrLevel) || !errors.Is(err, level.ErrDefinition) {
		t.Fatalf("failing commit: %v", err)
	}
	if len(settled.Actions) != 0 || settled.View != nil {
		t.Fatalf("a failed commit handed out flow results: %+v", settled)
	}
	if after := snapshot(); after != before {
		t.Fatalf("level state survived the failed commit:\nbefore %s\nafter  %s", before, after)
	}
	if got, err := h.Snapshot(ctx, ref); err != nil || got.Tick != 2 {
		t.Fatalf("physics frame survived the failed commit: tick %d %v", got.Tick, err)
	}
	// 同一帧只带合法的那条再提交：帧 3 还没被用掉，事实 2 也没被记成已处理。
	settled, err = h.Commit(ctx, ref, domain.Tick{Frame: frame(3), Facts: []level.Event{enter(2, 2, "pit")}})
	if err != nil || len(settled.Actions) != 2 || settled.View.Variables["global/falls"] != level.Int(2) {
		t.Fatalf("retry after rollback: %+v %v", settled, err)
	}
	second := settled.Actions[1]

	// 什么都没发生的一帧：流程状态不变，不交回动作与视图。
	revision := settled.View.Revision
	settled, err = h.Commit(ctx, ref, domain.Tick{Frame: frame(4)})
	if err != nil || settled.Actions != nil || settled.View != nil || settled.StaleReceipts != 0 {
		t.Fatalf("idle tick: %+v %v", settled, err)
	}
	// 第一个动作的回执：剩下第二个待执行。
	receipt := func(e level.Effect, success bool) level.Receipt {
		return level.Receipt{Instance: e.Instance, Epoch: e.Epoch, ID: e.ID, Success: success}
	}
	settled, err = h.Commit(ctx, ref, domain.Tick{Frame: frame(5), Receipts: []level.Receipt{receipt(first, true)}})
	if err != nil || len(settled.Actions) != 1 || settled.Actions[0].ID != second.ID || settled.View.Revision <= revision {
		t.Fatalf("receipt: %+v %v", settled, err)
	}
	// 成员离开结束 run 阶段，第二个动作被取消；它的回执随后到达：作废，不算错。
	settled, err = h.Commit(ctx, ref, domain.Tick{Frame: frame(6), Facts: []level.Event{fact(3, gamephysics.EventMemberLeave, 1, "")}})
	if err != nil || len(settled.Actions) != 0 {
		t.Fatalf("stage end: %+v %v", settled, err)
	}
	before = snapshot()
	settled, err = h.Commit(ctx, ref, domain.Tick{Frame: frame(7), Receipts: []level.Receipt{receipt(second, true), {Instance: instance, Epoch: ref.Epoch, ID: "never-requested"}}})
	if err != nil || settled.StaleReceipts != 2 || settled.View != nil {
		t.Fatalf("stale receipts: %+v %v", settled, err)
	}
	if after := snapshot(); after != before {
		t.Fatal("stale receipts changed the level state")
	}

	// 另一个房间：进终点结算，同一笔里排在后面的事实不再投递（否则 Handle 会因为阶段不是 running 报错）。
	other := domain.Ref{ID: "finish", Epoch: 5}
	instance = domain.LevelInstance(other)
	if instance == domain.LevelInstance(ref) {
		t.Fatal("two rooms share a level instance name")
	}
	otherFrame := func(tick uint64) gamephysics.Frame {
		f := frame(tick)
		f.Epoch = other.Epoch
		return f
	}
	if _, err = h.Commit(ctx, other, domain.Tick{Frame: otherFrame(1), Program: program}); err != nil {
		t.Fatal(err)
	}
	ended := []level.Event{
		{Instance: instance, Epoch: other.Epoch, Source: domain.FactSource, Sequence: 1, Kind: gamephysics.EventRegionEnter, Fields: map[string]level.Value{"actor": level.Int(1), "region": level.String("goal")}},
		{Instance: instance, Epoch: other.Epoch, Source: domain.FactSource, Sequence: 2, Kind: gamephysics.EventRegionEnter, Fields: map[string]level.Value{"actor": level.Int(2), "region": level.String("pit")}},
	}
	settled, err = h.Commit(ctx, other, domain.Tick{Frame: otherFrame(2), Facts: ended})
	if err != nil || settled.View == nil || settled.View.Phase != level.Finishing || settled.View.Result != "clear" || len(settled.Actions) != 0 {
		t.Fatalf("finish: %+v %v", settled, err)
	}
	if _, ok := settled.View.Variables["global/falls"]; !ok || settled.View.Variables["global/falls"] != level.Int(0) {
		t.Fatalf("a fact after the finish was still handled: %+v", settled.View.Variables)
	}

	// 准备期：PrepareDelay 按帧号计时，到点的那一帧才进入 running；中间的帧不改流程状态。
	delayed, err := gamephysics.CompileFlow([]byte(strings.Replace(courseFlow, `"TimeDomain":"tick"`, `"TimeDomain":"tick","PrepareDelay":3`, 1)), spec.Regions, spec.Spawns, level.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	waiting := domain.Ref{ID: "delayed", Epoch: 8}
	var phases []level.Phase
	for tick := uint64(1); tick <= 5; tick++ {
		f := frame(tick)
		f.Epoch = waiting.Epoch
		commit := domain.Tick{Frame: f}
		if tick == 1 {
			commit.Program = delayed
		}
		if settled, err = h.Commit(ctx, waiting, commit); err != nil {
			t.Fatalf("delayed tick %d: %v", tick, err)
		}
		phase := level.Phase("-")
		if settled.View != nil {
			phase = settled.View.Phase
		}
		phases = append(phases, phase)
	}
	if want := []level.Phase{level.Preparing, "-", level.Running, "-", "-"}; !slices.Equal(phases, want) {
		t.Fatalf("phases per tick %v, want %v", phases, want)
	}

	// 没有流程的房间：只接受帧；带着事实或回执来是调用方的错。
	plain := domain.Ref{ID: "plain", Epoch: 6}
	plainFrame := frame(1)
	plainFrame.Epoch = plain.Epoch
	if settled, err = h.Commit(ctx, plain, domain.Tick{Frame: plainFrame}); err != nil || settled.View != nil || settled.Actions != nil {
		t.Fatalf("plain room: %+v %v", settled, err)
	}
	if _, err = h.Level(ctx, plain); !errors.Is(err, domain.ErrMissing) {
		t.Fatalf("level of a room without a flow: %v", err)
	}
	plainFrame.Tick = 2
	if _, err = h.Commit(ctx, plain, domain.Tick{Frame: plainFrame, Facts: ended[:1]}); !errors.Is(err, domain.ErrRequestInvalid) {
		t.Fatalf("facts for a room without a flow: %v", err)
	}
	if got, err := h.Snapshot(ctx, plain); err != nil || got.Tick != 1 {
		t.Fatalf("the refused commit still advanced the frame: %d %v", got.Tick, err)
	}
}
