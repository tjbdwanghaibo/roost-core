//go:build physx && cgo

package physicsroom

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/gameplay/level"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
	domain "github.com/tjbdwanghaibo/roost-core/service/physicsroom"
)

func levelHost(t *testing.T) *EntityHost {
	t.Helper()
	hostKindOnce.Do(func() {
		if err := entity.RegisterEntityKindCategory(241, 1); err != nil {
			panic(err)
		}
	})
	host, err := NewEntityHost(241, 1, 2, nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 16}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 8}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 8}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { host.Close(context.Background()) })
	return host
}

type recordedTick struct {
	tick    domain.Tick
	settled domain.Settled
}

// 真实 PhysX 房间 + 房间 Entity + 真实流程：玩家被推下窄道，掉进 pit 后回到路过时记下的检查点，再走到终点结算。
// 把这次运行里每一笔提交录下来，在两个全新的 Entity 上各重放一遍：每一笔交回的动作与视图、最终的流程状态都逐字节相同。
func TestLevelReplayIsDeterministic(t *testing.T) {
	ctx := context.Background()
	host := levelHost(t)
	var mu sync.Mutex
	var recorded []recordedTick
	cfg := domain.DefaultConfig(func(ctx context.Context, ref domain.Ref, tick domain.Tick) (domain.Settled, error) {
		settled, err := host.Commit(ctx, ref, tick)
		if err == nil {
			mu.Lock()
			recorded = append(recorded, recordedTick{tick, settled})
			mu.Unlock()
		}
		return settled, err
	})
	cfg.Closed = func(domain.Ref) {} // 房间结束后保留 Entity，下面还要读它的流程状态
	cfg.InputTTL = time.Second
	svc, err := domain.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Shutdown(ctx)
	instance, err := svc.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := svc.Create(ctx, courseSpec("replay", instance))
	if err != nil {
		t.Fatal(err)
	}
	binding := gateway.Binding{PlayerID: 1, SessionID: "session", ConnectionNonce: "0123456789abcdef0123456789abcdef", BindID: "1123456789abcdef0123456789abcdef", Gate: gateway.ProcessIdentity{ServerID: 1, Incarnation: "0123456789abcdef"}, Game: gateway.ProcessIdentity{ServerID: 2, Incarnation: "1123456789abcdef"}}
	member, err := svc.Join(ctx, ref, binding, 1)
	if err != nil {
		t.Fatal(err)
	}
	// latest 返回最近一笔提交里玩家 1 的状态，以及到目前为止流程公开过的最新视图。
	latest := func() (physx.State, *level.View) {
		mu.Lock()
		defer mu.Unlock()
		var body physx.State
		var view *level.View
		if len(recorded) == 0 {
			return body, nil
		}
		for _, s := range recorded[len(recorded)-1].tick.Frame.States {
			if s.ID == 1 {
				body = s
			}
		}
		for i := len(recorded) - 1; i >= 0 && view == nil; i-- {
			view = recorded[i].settled.View
		}
		return body, view
	}
	sequence := uint64(0)
	push := func(x float32, until func(physx.State, *level.View) bool) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			sequence++
			if err := svc.Submit(ctx, member, domain.Input{Sequence: sequence, Acceleration: physx.Vec3{x, 0, 0}}); err != nil {
				t.Fatal(err)
			}
			if body, view := latest(); until(body, view) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		body, view := latest()
		t.Fatalf("condition not reached: body %+v view %+v", body, view)
	}
	// 往 -x：路过检查点区域，冲出窄道掉下去，被传送回检查点。
	push(-8, func(body physx.State, _ *level.View) bool { return body.Teleports == 1 })
	body, view := latest()
	if view == nil || view.Variables["global/falls"] != level.Int(1) {
		t.Fatalf("falls not counted: %+v", view)
	}
	if body.Position[1] < 0 || body.Position[0] > -1 || body.Position[0] < -3 {
		t.Fatalf("respawned at %v, want the checkpoint near x=-2", body.Position)
	}
	// 往 +x：走到终点。
	push(8, func(_ physx.State, view *level.View) bool { return view != nil && view.Phase == level.Finishing })
	if _, view = latest(); view.Result != "clear" {
		t.Fatalf("result %q", view.Result)
	}
	if err = svc.Destroy(ctx, ref); err != nil {
		t.Fatal(err)
	}
	original, err := host.Level(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(original)
	mu.Lock()
	ticks := recorded
	mu.Unlock()

	// 录下来的内容本身要讲得通：事实编号递增，重生的回执成功，检查点在掉落之前记下。
	var kinds []string
	var lastFact uint64
	receipts := 0
	for _, r := range ticks {
		for _, fact := range r.tick.Facts {
			if fact.Sequence <= lastFact || fact.Source != domain.FactSource {
				t.Fatalf("tick %d: fact %+v after sequence %d", r.tick.Frame.Tick, fact, lastFact)
			}
			lastFact = fact.Sequence
			if fact.Fields["actor"] == level.Int(1) {
				kinds = append(kinds, fact.Kind+":"+fact.Fields["region"].Text)
			}
		}
		for _, receipt := range r.tick.Receipts {
			receipts++
			if !receipt.Success {
				t.Fatalf("tick %d: action %s failed", r.tick.Frame.Tick, receipt.ID)
			}
		}
	}
	t.Logf("recorded %d ticks, %d facts, %d receipts; player 1: %v", len(ticks), lastFact, receipts, kinds)
	order := map[string]int{}
	for i, kind := range kinds {
		if _, seen := order[kind]; !seen {
			order[kind] = i
		}
	}
	cp, cpOK := order["region.enter:cp"]
	pit, pitOK := order["region.enter:pit"]
	goal, goalOK := order["region.enter:goal"]
	if !cpOK || !pitOK || !goalOK || cp > pit || pit > goal {
		t.Fatalf("player 1 facts out of the expected order: %v", kinds)
	}
	if receipts < 3 { // 至少：路过检查点一次、重生一次、传送落进检查点区域后又记一次
		t.Fatalf("only %d receipts", receipts)
	}

	for run := range 2 {
		fresh := levelHost(t)
		for _, r := range ticks {
			settled, err := fresh.Commit(ctx, ref, r.tick)
			if err != nil {
				t.Fatalf("replay %d tick %d: %v", run, r.tick.Frame.Tick, err)
			}
			if !reflect.DeepEqual(settled, r.settled) {
				t.Fatalf("replay %d tick %d settled differently:\n got %+v\nwant %+v", run, r.tick.Frame.Tick, settled, r.settled)
			}
		}
		state, err := fresh.Level(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := json.Marshal(state); string(got) != string(want) {
			t.Fatalf("replay %d ended in a different level state:\n got %s\nwant %s", run, got, want)
		}
	}
}
