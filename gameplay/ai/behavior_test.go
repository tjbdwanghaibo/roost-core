package ai

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	coreflow "github.com/tjbdwanghaibo/roost-core/gameplay/actionflow"
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
			strategy.Tick(&Context{}, time.Time{})
		}
		return data.log
	}
	if !reflect.DeepEqual(run(), run()) {
		t.Fatal("decision log differs across identical runs")
	}
}

// guard 谓词只接受无状态形状（条件与纯逻辑组合）：动作叶子当谓词会在每次检查时产生副作用
// 且无人复位，组装时即拒绝。
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

// 无根树的策略不累积动作完成通知：Tick 把它们丢弃。
func TestBehaviorStrategyNilRootDropsPendingCompletions(t *testing.T) {
	strategy := NewBehaviorStrategy[treeData](nil, BehaviorStrategyOptions[treeData]{})
	for i := 0; i < 100; i++ {
		strategy.OnActionEnd(nil, int64(i), coreflow.ActionKind(1), coreflow.NewActionReason("done"))
		strategy.Tick(&Context{}, time.Time{})
	}
	if len(strategy.pending) != 0 {
		t.Fatalf("pending grew to %d", len(strategy.pending))
	}
}

func expectTreeErr(t *testing.T, err error, sentinel error, text string) {
	t.Helper()
	if err == nil || !errors.Is(err, sentinel) || !strings.Contains(err.Error(), text) {
		t.Fatalf("error = %v, want %v containing %q", err, sentinel, text)
	}
}

// Registrations are addressed by name from documents; a nameless or
// duplicated factory would make a document bind to the wrong behaviour.
func TestTreeRegistryRefusesNamelessAndDuplicateFactories(t *testing.T) {
	registry := NewRegistry[treeCtx](nowOf)
	factory := func(json.RawMessage) (Node[treeCtx], error) {
		return &Condition[treeCtx]{Check: func(*treeCtx) bool { return true }}, nil
	}
	expectTreeErr(t, registry.RegisterCondition("", factory), ErrTreeInvalid, "condition registration requires a name and factory")
	expectTreeErr(t, registry.RegisterCondition("ok", nil), ErrTreeInvalid, "condition registration requires a name and factory")
	if err := registry.RegisterCondition("ok", factory); err != nil {
		t.Fatal(err)
	}
	expectTreeErr(t, registry.RegisterCondition("ok", factory), ErrTreeInvalid, `duplicate condition "ok"`)
	expectTreeErr(t, registry.RegisterAction("", factory), ErrTreeInvalid, "action registration requires a kind and factory")
	if err := registry.RegisterAction("go", factory); err != nil {
		t.Fatal(err)
	}
	expectTreeErr(t, registry.RegisterAction("go", factory), ErrTreeInvalid, `duplicate action "go"`)
}

// 树文档的每条拒绝规则按错误类别、消息与 JSON 路径各钉一行：去掉任何一条规则
// 恰好让一行变红，不会被相邻规则顺带接住。
func TestParseTreeRefusesEachDefectByMessageAndPath(t *testing.T) {
	registry := wireRegistry(t)
	leaf := `{"node":"condition","name":"hp_below","args":{}}`
	deep := leaf
	for i := 0; i <= maxTreeDepth; i++ {
		deep = `{"node":"inverter","child":` + deep + `}`
	}
	cases := []struct{ name, document, text string }{
		{"schema", `{"schema":"roost.ai/v9","root":` + leaf + `}`, `"roost.ai/v9"`},
		{"root required", `{"schema":"roost.ai/v1"}`, "$.root is required"},
		{"missing node kind", `{"schema":"roost.ai/v1","root":{"name":"x"}}`, `$.root: missing "node"`},
		{"unknown node kind", `{"schema":"roost.ai/v1","root":{"node":"paralel"}}`, `$.root: unknown node "paralel"`},
		{"unknown action", `{"schema":"roost.ai/v1","root":{"node":"action","kind":"fly","args":{}}}`, `$.root: unknown action "fly"`},
		{"unknown condition", `{"schema":"roost.ai/v1","root":{"node":"sequence","children":[{"node":"condition","name":"nope","args":{}}]}}`, `$.root.children[0]: unknown condition "nope"`},
		{"guard without condition", `{"schema":"roost.ai/v1","root":{"node":"guard","child":` + leaf + `}}`, "$.root.condition is required"},
		{"repeat without child", `{"schema":"roost.ai/v1","root":{"node":"repeat","count":2}}`, "child is required"},
		{"cooldown ticks not positive", `{"schema":"roost.ai/v1","root":{"node":"cooldown","ticks":0,"child":` + leaf + `}}`, "$.root.ticks must be positive"},
		{"unknown parallel policy", `{"schema":"roost.ai/v1","root":{"node":"parallel","policy":"most","children":[` + leaf + `]}}`, `$.root.policy: unknown policy "most"`},
		{"unknown field", `{"schema":"roost.ai/v1","root":{"node":"sequence","childs":[]}}`, `$.root: json: unknown field "childs"`},
		{"empty children", `{"schema":"roost.ai/v1","root":{"node":"sequence","children":[]}}`, "$.root.children must not be empty"},
		{"inverter without child", `{"schema":"roost.ai/v1","root":{"node":"inverter"}}`, "$.root.child is required"},
		{"unknown condition at root", `{"schema":"roost.ai/v1","root":{"node":"condition","name":"nope","args":{}}}`, `$.root: unknown condition "nope"`},
		{"too deep", `{"schema":"roost.ai/v1","root":` + deep + `}`, "exceeds depth 64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseTree([]byte(tc.document), registry)
			sentinel := ErrTreeInvalid
			if tc.name == "schema" {
				sentinel = ErrTreeSchema
			}
			expectTreeErr(t, err, sentinel, tc.text)
		})
	}
}

type refusingStrategy struct{ controllerTestStrategy }

func (*refusingStrategy) CanStopByNext(Strategy) bool { return false }

// SetStrategy 拒绝 nil 控制器、切换中重入、当前策略拒绝被替换三种情形，被拒的切换不扰动
// 当前策略；ParseTree 无注册表、时间节点无时钟、谓词过深、inverter 谓词缺 child 各报 ErrTreeInvalid。
// 历史：U-0148
func TestSetStrategyRefusesNilReentrantAndRejectedSwitches(t *testing.T) {
	var none *Controller
	if err := none.SetStrategy(&controllerTestStrategy{name: "x"}); !errors.Is(err, ErrStrategyInit) {
		t.Fatalf("SetStrategy on a nil controller = %v", err)
	}
	controller := NewController(ControllerHooks{})
	controller.switching = true
	if err := controller.SetStrategy(&controllerTestStrategy{name: "x"}); !errors.Is(err, ErrReentrantSwitch) {
		t.Fatalf("SetStrategy while switching = %v", err)
	}
	controller.switching = false
	holder := &refusingStrategy{controllerTestStrategy{name: "holder"}}
	if err := controller.SetStrategy(holder); err != nil {
		t.Fatal(err)
	}
	if err := controller.SetStrategy(&controllerTestStrategy{name: "next"}); !errors.Is(err, ErrStrategyRejected) {
		t.Fatalf("SetStrategy over a strategy that refuses to stop = %v", err)
	}
	if controller.Strategy() != holder || holder.stopCount != 0 {
		t.Fatal("a rejected switch disturbed the current strategy")
	}
}

func TestParseTreeRefusesMissingRegistryClockAndMalformedPredicates(t *testing.T) {
	leaf := `{"node":"condition","name":"hp_below","args":{}}`
	if _, err := ParseTree[treeCtx]([]byte(`{"schema":"roost.ai/v1","root":`+leaf+`}`), nil); !errors.Is(err, ErrTreeInvalid) || !strings.Contains(err.Error(), "registry is required") {
		t.Fatalf("ParseTree(nil registry) = %v", err)
	}
	clockless := NewRegistry[treeCtx](nil)
	if _, err := ParseTree([]byte(`{"schema":"roost.ai/v1","root":{"node":"cooldown","ticks":3,"child":`+leaf+`}}`), clockless); !errors.Is(err, ErrTreeInvalid) || !strings.Contains(err.Error(), "no tick clock") {
		t.Fatalf("cooldown without a registry clock = %v", err)
	}
	registry := wireRegistry(t)
	deep := leaf
	for i := 0; i <= maxTreeDepth; i++ {
		deep = `{"node":"inverter","child":` + deep + `}`
	}
	if _, err := ParseTree([]byte(`{"schema":"roost.ai/v1","root":{"node":"guard","condition":`+deep+`,"child":`+leaf+`}}`), registry); !errors.Is(err, ErrTreeInvalid) || !strings.Contains(err.Error(), "exceeds depth") || !strings.Contains(err.Error(), "condition") {
		t.Fatalf("guard predicate nested too deep = %v", err)
	}
	if _, err := ParseTree([]byte(`{"schema":"roost.ai/v1","root":{"node":"guard","condition":{"node":"inverter"},"child":`+leaf+`}}`), registry); !errors.Is(err, ErrTreeInvalid) || !strings.Contains(err.Error(), "condition.child is required") {
		t.Fatalf("inverter predicate without a child = %v", err)
	}
	if _, err := ParseTree([]byte(`{"schema":"roost.ai/v1","root":{"node":"guard","condition":{"node":"inverter","child":`+leaf+`},"child":`+leaf+`}}`), registry); err != nil {
		t.Fatalf("valid inverter predicate refused: %v", err)
	}
}
