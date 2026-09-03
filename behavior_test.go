package ai

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	coreflow "github.com/tjbdwanghaibo/roost-core/actionflow"
	coreai "github.com/tjbdwanghaibo/roost-core/ai"
)

type treeData struct {
	tick     int64
	hp       int64
	log      []string
	launched int64
}

type treeCtx = BehaviorContext[treeData]

func nowOf(ctx *treeCtx) int64 { return ctx.NowTick }

func logAction(name string, result Status) Node[treeCtx] {
	return &FuncNode[treeCtx]{Run: func(ctx *treeCtx) Status {
		ctx.Data.log = append(ctx.Data.log, name)
		return result
	}}
}

func TestNodesGuardCooldownRepeatParallel(t *testing.T) {
	data := &treeData{hp: 100}
	ctx := &treeCtx{Data: data}
	guard := &Guard[treeCtx]{
		Check: func(ctx *treeCtx) bool { return ctx.Data.hp < 50 },
		Child: logAction("flee", StatusSuccess),
	}
	if guard.Tick(ctx) != StatusFailure || len(data.log) != 0 {
		t.Fatal("guard let a false predicate through")
	}
	data.hp = 30
	if guard.Tick(ctx) != StatusSuccess || data.log[0] != "flee" {
		t.Fatal("guard blocked a true predicate")
	}

	cooldown := &Cooldown[treeCtx]{Child: logAction("slam", StatusSuccess), Ticks: 10, Now: nowOf}
	ctx.NowTick = 100
	if cooldown.Tick(ctx) != StatusSuccess {
		t.Fatal("first use blocked")
	}
	ctx.NowTick = 105
	if cooldown.Tick(ctx) != StatusFailure {
		t.Fatal("cooldown not enforced")
	}
	ctx.NowTick = 110
	if cooldown.Tick(ctx) != StatusSuccess {
		t.Fatal("cooldown did not elapse")
	}

	repeat := &Repeat[treeCtx]{Child: logAction("hit", StatusSuccess), Count: 3}
	ticks := 0
	for status := StatusRunning; status == StatusRunning; ticks++ {
		status = repeat.Tick(ctx)
	}
	if ticks != 3 {
		t.Fatalf("repeat completed in %d ticks, want 3", ticks)
	}

	parallel := &Parallel[treeCtx]{Policy: ParallelRequireAll, Children: []Node[treeCtx]{
		logAction("left", StatusSuccess),
		&FuncNode[treeCtx]{Run: func(ctx *treeCtx) Status {
			if ctx.NowTick >= 112 {
				return StatusSuccess
			}
			return StatusRunning
		}},
	}}
	ctx.NowTick = 111
	if parallel.Tick(ctx) != StatusRunning {
		t.Fatal("parallel finished early")
	}
	ctx.NowTick = 112
	if parallel.Tick(ctx) != StatusSuccess {
		t.Fatal("parallel did not complete")
	}
	// The already-succeeded child must not have been re-ticked.
	count := 0
	for _, entry := range ctx.Data.log {
		if entry == "left" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("parallel re-ticked a finished child %d times", count)
	}
}

func TestBehaviorStrategyDrivesTreeThroughController(t *testing.T) {
	data := &treeData{}
	tick := int64(0)
	launch := &TaskflowAction[treeData]{
		Launch: func(ctx *treeCtx) (int64, error) {
			ctx.Data.launched++
			return ctx.Data.launched, nil
		},
		Succeeded: func(reason coreflow.ActionReason) bool { return reason == coreflow.NewActionReason("done") },
	}
	root := &Sequence[treeCtx]{Children: []Node[treeCtx]{
		&Condition[treeCtx]{Check: func(ctx *treeCtx) bool { return ctx.Data.hp > 0 }},
		launch,
	}}
	var results []Status
	strategy := NewBehaviorStrategy(root, BehaviorStrategyOptions[treeData]{
		Name: "test_tree", Data: data,
		NowTick:  func() int64 { return tick },
		OnResult: func(status Status) { results = append(results, status) },
	})
	controller := NewController(ControllerHooks{})
	if err := controller.SetStrategy(strategy); err != nil {
		t.Fatal(err)
	}
	data.hp = 100
	controller.Tick(time.Unix(1, 0)) // launches action 1, tree Running
	if data.launched != 1 || len(results) != 0 {
		t.Fatalf("launched=%d results=%v", data.launched, results)
	}
	controller.Tick(time.Unix(2, 0)) // still waiting
	if data.launched != 1 {
		t.Fatal("action relaunched while in flight")
	}
	controller.OnActionEnd(1, coreflow.ActionKind(1), coreflow.NewActionReason("done"))
	controller.Tick(time.Unix(3, 0)) // consumes the completion, tree Success
	if !reflect.DeepEqual(results, []Status{StatusSuccess}) {
		t.Fatalf("results = %v", results)
	}
	// Failure mapping: a non-"done" reason fails the tree.
	controller.Tick(time.Unix(4, 0)) // relaunch (action 2)
	controller.OnActionEnd(2, coreflow.ActionKind(1), coreflow.NewActionReason("cancelled"))
	controller.Tick(time.Unix(5, 0))
	if !reflect.DeepEqual(results, []Status{StatusSuccess, StatusFailure}) {
		t.Fatalf("results = %v", results)
	}
	// Strategy replacement resets cleanly (transactional SetStrategy).
	if err := controller.SetStrategy(nil); err != nil {
		t.Fatal(err)
	}
}

func TestTaskflowActionInterruptHook(t *testing.T) {
	interrupted := int64(0)
	action := &TaskflowAction[treeData]{
		Launch:      func(*treeCtx) (int64, error) { return 42, nil },
		OnInterrupt: func(id int64) { interrupted = id },
	}
	guardOpen := true
	root := &Guard[treeCtx]{Check: func(*treeCtx) bool { return guardOpen }, Child: action}
	ctx := &treeCtx{Data: &treeData{}}
	if root.Tick(ctx) != StatusRunning {
		t.Fatal("action did not launch")
	}
	guardOpen = false
	if root.Tick(ctx) != StatusFailure || interrupted != 42 {
		t.Fatalf("interrupt hook not fired: %d", interrupted)
	}
}

const wireTreeJSON = `{
  "schema": "roost.ai/v1",
  "root": {
    "node": "selector",
    "children": [
      {"node": "sequence", "children": [
        {"node": "condition", "name": "hp_below", "args": {"threshold": 50}},
        {"node": "cooldown", "ticks": 5, "child": {"node": "action", "kind": "log", "args": {"name": "heal"}}}
      ]},
      {"node": "guard",
       "condition": {"node": "condition", "name": "hp_below", "args": {"threshold": 200}},
       "child": {"node": "repeat", "count": 2, "child": {"node": "action", "kind": "log", "args": {"name": "attack"}}}}
    ]
  }
}`

func wireRegistry(t *testing.T) *Registry[treeCtx] {
	t.Helper()
	registry := NewRegistry[treeCtx](nowOf)
	if err := registry.RegisterCondition("hp_below", func(args json.RawMessage) (Node[treeCtx], error) {
		var params struct {
			Threshold int64 `json:"threshold"`
		}
		if err := json.Unmarshal(args, &params); err != nil {
			return nil, err
		}
		return &Condition[treeCtx]{Check: func(ctx *treeCtx) bool { return ctx.Data.hp < params.Threshold }}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterAction("log", func(args json.RawMessage) (Node[treeCtx], error) {
		var params struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(args, &params); err != nil {
			return nil, err
		}
		return logAction(params.Name, StatusSuccess), nil
	}); err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestParseTreeAssemblesAndRuns(t *testing.T) {
	root, err := ParseTree([]byte(wireTreeJSON), wireRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	data := &treeData{hp: 100} // not below 50: falls through to the attack branch
	ctx := &treeCtx{Data: data, NowTick: 1}
	if status := root.Tick(ctx); status != StatusRunning { // repeat round 1
		t.Fatalf("status = %v", status)
	}
	if status := root.Tick(ctx); status != StatusSuccess { // repeat round 2
		t.Fatalf("status = %v", status)
	}
	if !reflect.DeepEqual(data.log, []string{"attack", "attack"}) {
		t.Fatalf("log = %v", data.log)
	}
	// Low hp prefers the heal branch, and its cooldown gates the second use.
	data.hp, data.log = 30, nil
	ctx.NowTick = 10
	if root.Tick(ctx) != StatusSuccess || data.log[0] != "heal" {
		t.Fatalf("heal branch: %v", data.log)
	}
	ctx.NowTick = 12
	root.Tick(ctx) // cooldown blocks heal; attack branch runs instead
	if len(data.log) != 2 || data.log[1] != "attack" {
		t.Fatalf("cooldown fallthrough: %v", data.log)
	}
}

func TestParseTreeFailFast(t *testing.T) {
	registry := wireRegistry(t)
	cases := map[string]string{
		"unknown schema": `{"schema":"roost.ai/v9","root":{"node":"sequence","children":[{"node":"condition","name":"hp_below","args":{}}]}}`,
		"unknown node":   `{"schema":"roost.ai/v1","root":{"node":"paralel","children":[]}}`,
		"unknown field":  `{"schema":"roost.ai/v1","root":{"node":"sequence","childs":[]}}`,
		"empty children": `{"schema":"roost.ai/v1","root":{"node":"sequence","children":[]}}`,
		"missing child":  `{"schema":"roost.ai/v1","root":{"node":"inverter"}}`,
		"unknown leaf":   `{"schema":"roost.ai/v1","root":{"node":"condition","name":"nope","args":{}}}`,
		"bad ticks":      `{"schema":"roost.ai/v1","root":{"node":"cooldown","ticks":0,"child":{"node":"condition","name":"hp_below","args":{}}}}`,
		"unknown policy": `{"schema":"roost.ai/v1","root":{"node":"parallel","policy":"most","children":[{"node":"condition","name":"hp_below","args":{}}]}}`,
	}
	for name, document := range cases {
		if _, err := ParseTree([]byte(document), registry); err == nil {
			t.Fatalf("%s accepted", name)
		} else if !errors.Is(err, ErrTreeInvalid) && !errors.Is(err, ErrTreeSchema) {
			t.Fatalf("%s: unexpected error class: %v", name, err)
		}
	}
	// Diagnostics carry the JSON path.
	_, err := ParseTree([]byte(`{"schema":"roost.ai/v1","root":{"node":"sequence","children":[{"node":"condition","name":"nope","args":{}}]}}`), registry)
	if err == nil || !strings.Contains(err.Error(), "$.root.children[0]") {
		t.Fatalf("diagnostic lacks path: %v", err)
	}
}

func TestBehaviorStrategyDeterministicDecisions(t *testing.T) {
	run := func() []string {
		root, err := ParseTree([]byte(wireTreeJSON), wireRegistry(t))
		if err != nil {
			t.Fatal(err)
		}
		data := &treeData{hp: 60}
		tick := int64(0)
		strategy := NewBehaviorStrategy(root, BehaviorStrategyOptions[treeData]{Data: data, NowTick: func() int64 { return tick }})
		if err := strategy.Init(nil); err != nil {
			t.Fatal(err)
		}
		for tick = 1; tick <= 40; tick++ {
			data.hp -= 3
			strategy.Tick(&coreai.Context{}, time.Time{})
		}
		return data.log
	}
	if !reflect.DeepEqual(run(), run()) {
		t.Fatal("decision log differs across identical runs")
	}
}

// Regression (review): a guard predicate accepted arbitrary nodes — an
// action leaf as predicate would fire side effects on every check with no
// reset. Predicates are now restricted to stateless shapes at assembly.
func TestParseTreeRejectsStatefulGuardPredicates(t *testing.T) {
	registry := wireRegistry(t)
	bad := `{"schema":"roost.ai/v1","root":{"node":"guard",
	  "condition":{"node":"action","kind":"log","args":{"name":"boom"}},
	  "child":{"node":"condition","name":"hp_below","args":{"threshold":1}}}}`
	if _, err := ParseTree([]byte(bad), registry); err == nil || !strings.Contains(err.Error(), "not a valid guard predicate") {
		t.Fatalf("action predicate accepted: %v", err)
	}
	good := `{"schema":"roost.ai/v1","root":{"node":"guard",
	  "condition":{"node":"inverter","child":{"node":"selector","children":[
	    {"node":"condition","name":"hp_below","args":{"threshold":1}},
	    {"node":"condition","name":"hp_below","args":{"threshold":2}}]}},
	  "child":{"node":"condition","name":"hp_below","args":{"threshold":100}}}}`
	if _, err := ParseTree([]byte(good), registry); err != nil {
		t.Fatalf("pure logic predicate rejected: %v", err)
	}
}

// Regression (review): a nil-root strategy accumulated action completions
// forever; Tick now drops them.
func TestBehaviorStrategyNilRootDropsPendingCompletions(t *testing.T) {
	strategy := NewBehaviorStrategy[treeData](nil, BehaviorStrategyOptions[treeData]{})
	for i := 0; i < 100; i++ {
		strategy.OnActionEnd(nil, int64(i), coreflow.ActionKind(1), coreflow.NewActionReason("done"))
		strategy.Tick(&coreai.Context{}, time.Time{})
	}
	if len(strategy.pending) != 0 {
		t.Fatalf("pending grew to %d", len(strategy.pending))
	}
}
