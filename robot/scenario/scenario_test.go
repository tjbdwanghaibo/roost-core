package scenario_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/robot"
	"github.com/tjbdwanghaibo/roost-core/robot/scenario"
	"github.com/tjbdwanghaibo/roost-core/robot/session"
)

func newRecordingContext(log *[]string, fail map[string]error) *robot.Context {
	var mu sync.Mutex
	rb := robot.NewContext(robot.Config{Seed: 7})
	rb.RunAction = func(_ context.Context, _ *robot.Context, name string, param any) error {
		mu.Lock()
		*log = append(*log, name)
		mu.Unlock()
		if fail != nil {
			return fail[name]
		}
		return nil
	}
	return rb
}

func TestCombinators(t *testing.T) {
	var log []string
	boom := errors.New("boom")
	rb := newRecordingContext(&log, map[string]error{"bad": boom})

	// Sequence stops at the first error.
	err := scenario.Sequence(scenario.Action("a", nil), scenario.Action("bad", nil), scenario.Action("c", nil)).Run(context.Background(), rb)
	if !errors.Is(err, boom) || strings.Join(log, ",") != "a,bad" {
		t.Fatalf("sequence: %v %v", err, log)
	}

	// Selector falls through failures to the first success.
	log = nil
	if err := scenario.Selector(scenario.Action("bad", nil), scenario.Action("a", nil)).Run(context.Background(), rb); err != nil {
		t.Fatalf("selector: %v", err)
	}

	// BestEffort swallows the failure.
	if err := scenario.BestEffort(scenario.Action("bad", nil)).Run(context.Background(), rb); err != nil {
		t.Fatalf("best effort: %v", err)
	}

	// Retry retries and reports the last error.
	log = nil
	if err := scenario.Retry(3, scenario.Action("bad", nil)).Run(context.Background(), rb); !errors.Is(err, boom) || len(log) != 3 {
		t.Fatalf("retry: %v %v", err, log)
	}

	// Parallel joins errors.
	if err := scenario.Parallel(scenario.Action("a", nil), scenario.Action("bad", nil)).Run(context.Background(), rb); !errors.Is(err, boom) {
		t.Fatalf("parallel: %v", err)
	}

	// Cond branches on the predicate.
	log = nil
	flag := robot.Key[bool]("flag")
	flag.Set(rb, true)
	node := scenario.Cond(func(rb *robot.Context) bool { return flag.GetOr(rb, false) },
		scenario.Action("then", nil), scenario.Action("else", nil))
	if err := node.Run(context.Background(), rb); err != nil || strings.Join(log, ",") != "then" {
		t.Fatalf("cond: %v %v", err, log)
	}

	// Timeout propagates the deadline.
	start := time.Now()
	err = scenario.Timeout(50*time.Millisecond, scenario.Wait(time.Second)).Run(context.Background(), rb)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("timeout: %v after %v", err, time.Since(start))
	}
}

func TestRandomIsSeededPerRobot(t *testing.T) {
	pick := func(seed int64) string {
		var log []string
		rb := newRecordingContext(&log, nil)
		rb.Seed = seed
		node := scenario.Random(
			scenario.Weighted(1, scenario.Action("a", nil)),
			scenario.Weighted(1, scenario.Action("b", nil)),
			scenario.Weighted(1, scenario.Action("c", nil)),
		)
		for i := 0; i < 8; i++ {
			_ = node.Run(context.Background(), rb)
		}
		return strings.Join(log, ",")
	}
	if pick(42) != pick(42) {
		t.Fatal("same seed produced different pick sequences")
	}
}

const demoSpec = `
scenarios:
  - name: full_play
    node:
      sequence:
        - action: connect
        - action: buy
          param: {item_id: 7}
        - wait: 1ms
        - retry: {times: 2, node: {action: flaky}}
        - best_effort: {action: bad}
        - random:
            - {weight: 1, node: {action: farm}}
`

func TestSpecInterpreterRunsEquivalentTree(t *testing.T) {
	scenarios, err := scenario.ParseSpec([]byte(demoSpec))
	if err != nil {
		t.Fatal(err)
	}
	if len(scenarios) != 1 || scenarios[0].Name() != "full_play" {
		t.Fatalf("scenarios = %v", scenarios)
	}
	var log []string
	flakyLeft := 1
	rb := newRecordingContext(&log, nil)
	base := rb.RunAction
	rb.RunAction = func(ctx context.Context, rb *robot.Context, name string, param any) error {
		if name == "flaky" {
			if flakyLeft > 0 {
				flakyLeft--
				_ = base(ctx, rb, name, param)
				return errors.New("flaky once")
			}
		}
		if name == "bad" {
			_ = base(ctx, rb, name, param)
			return errors.New("always bad")
		}
		if name == "buy" {
			if m, ok := param.(map[string]any); !ok || m["item_id"] != 7 {
				t.Fatalf("param passthrough broken: %v", param)
			}
		}
		return base(ctx, rb, name, param)
	}
	if err := scenarios[0].Run(context.Background(), rb); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(log, ",")
	if !strings.HasPrefix(joined, "connect,buy,flaky,flaky,bad,") {
		t.Fatalf("execution order = %s", joined)
	}
}

type namedScenario string

func (n namedScenario) Name() string                              { return string(n) }
func (n namedScenario) Run(context.Context, *robot.Context) error { return nil }

func expectText(t *testing.T, err error, text string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), text) {
		t.Fatalf("error = %v, want it to contain %q", err, text)
	}
}

// 场景按（归一化后的）名字被 spec 与管理命令寻址：无名或重名的场景会不可达或有歧义，注册时拒绝。
func TestScenarioRegistryRefusesNamelessAndDuplicateScenarios(t *testing.T) {
	r := scenario.NewRegistry()
	expectText(t, r.Register(nil), "scenario is nil")
	expectText(t, r.Register(namedScenario("  ")), "name is required")
	if err := r.Register(namedScenario("Raid")); err != nil {
		t.Fatal(err)
	}
	expectText(t, r.Register(namedScenario("raid")), `duplicate "raid"`)
}

// 声明式场景文档的每条拒绝规则按消息各钉一行：去掉任何一条规则恰好让一行变红。
func TestSpecRefusesEachBrokenDocumentByMessage(t *testing.T) {
	cases := map[string]struct{ spec, text string }{
		"no scenarios":   {"scenarios: []\n", "spec has no scenarios"},
		"no name":        {"scenarios:\n  - node: {action: a}\n", "spec scenario without a name"},
		"duplicate":      {"scenarios:\n  - name: x\n    node: {action: a}\n  - name: X\n    node: {action: a}\n", `duplicate spec scenario "x"`},
		"empty node":     {"scenarios:\n  - name: x\n    node: {}\n", "empty node"},
		"two node kinds": {"scenarios:\n  - name: x\n    node: {action: a, wait: 1s}\n", "exactly one node kind"},
		"unknown key":    {"scenarios:\n  - name: x\n    node: {action: a}\n    typo: 1\n", "parse spec"},
		"bad duration":   {"scenarios:\n  - name: x\n    node: {wait: soon}\n", `wait: time: invalid duration "soon"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := scenario.ParseSpec([]byte(tc.spec))
			expectText(t, err, tc.text)
		})
	}
}

const timedSpec = `
scenarios:
  - name: timed
    node:
      sequence:
        - action: connect
        - retry:
            times: 5
            node:
              sequence:
                - wait: 5ms
                - action: flaky
        - selector:
            - action: bad
            - wait: 1ms
        - action: bad
        - action: never
  - name: single
    node:
      action: connect
`

// YAML 场景的每个顶层步骤自动计时：名字带序号与子树里的动作，Retry 次数与 Wait 时间计入所在步骤；
// 失败的那一步也记一个失败样本，之后没跑的步骤不出现。根不是 sequence 时整棵树算一步。
func TestSpecTimesEachTopLevelStep(t *testing.T) {
	scenarios, err := scenario.ParseSpec([]byte(timedSpec))
	if err != nil {
		t.Fatal(err)
	}
	var log []string
	rb := newRecordingContext(&log, map[string]error{"bad": errors.New("always bad")})
	base := rb.RunAction
	flakyLeft := 2
	rb.RunAction = func(ctx context.Context, rb *robot.Context, name string, param any) error {
		if name == "flaky" && flakyLeft > 0 {
			flakyLeft--
			return errors.New("not yet")
		}
		return base(ctx, rb, name, param)
	}
	recorder := &session.Recorder{}
	rb.Recorder = recorder
	if err := scenarios[0].Run(context.Background(), rb); err == nil || !strings.Contains(err.Error(), "always bad") {
		t.Fatalf("timed run = %v", err)
	}
	steps := recorder.StepSnapshot()
	names := make([]string, 0, len(steps))
	for _, s := range steps {
		names = append(names, s.Name)
	}
	if strings.Join(names, ";") != "01 connect;02 retry(wait 5ms,flaky);03 selector(bad|wait 1ms);04 bad" {
		t.Fatalf("step names = %q", names)
	}
	if s := steps[0]; s.Samples != 1 || s.OK != 1 || s.RetryAttemptsMax != 0 || s.WaitMeanMS != 0 {
		t.Fatalf("connect step = %+v", s)
	}
	if s := steps[1]; s.Samples != 1 || s.OK != 1 || s.RetryAttemptsMax != 3 || s.WaitMeanMS < 15 || s.MaxMS < s.WaitMeanMS {
		t.Fatalf("retry step = %+v", s)
	}
	if s := steps[2]; s.OK != 1 || s.WaitMeanMS < 1 {
		t.Fatalf("selector step = %+v", s)
	}
	if s := steps[3]; s.Samples != 1 || s.Failed != 1 || s.OK != 0 {
		t.Fatalf("failed step = %+v", s)
	}

	single := &session.Recorder{}
	rb.Recorder = single
	if err := scenarios[1].Run(context.Background(), rb); err != nil {
		t.Fatal(err)
	}
	if steps := single.StepSnapshot(); len(steps) != 1 || steps[0].Name != "01 connect" || steps[0].OK != 1 {
		t.Fatalf("single-node scenario steps = %+v", steps)
	}
}

// 代码搭的场景用 Step 命名；嵌套的 Step 各自计时，内层的 Wait 与 Retry 也算进外层。
// 没有 Recorder 的机器人照常执行、不计时。
func TestNestedStepsAccumulateIntoEveryEnclosingStep(t *testing.T) {
	var log []string
	rb := newRecordingContext(&log, nil)
	recorder := &session.Recorder{}
	rb.Recorder = recorder
	tree := scenario.Step("outer", scenario.Sequence(
		scenario.Wait(2*time.Millisecond),
		scenario.Step("inner", scenario.Retry(2, scenario.Wait(3*time.Millisecond))),
	))
	if err := tree.Run(context.Background(), rb); err != nil {
		t.Fatal(err)
	}
	steps := recorder.StepSnapshot()
	if len(steps) != 2 || steps[0].Name != "inner" || steps[1].Name != "outer" {
		t.Fatalf("steps = %+v", steps)
	}
	if inner := steps[0]; inner.RetryAttemptsMax != 1 || inner.WaitMeanMS < 3 || inner.WaitMeanMS >= 5 {
		t.Fatalf("inner = %+v", inner)
	}
	if outer := steps[1]; outer.RetryAttemptsMax != 1 || outer.WaitMeanMS < 5 || outer.MaxMS < outer.WaitMeanMS {
		t.Fatalf("outer = %+v", outer)
	}
	rb.Recorder = nil
	if err := tree.Run(context.Background(), rb); err != nil {
		t.Fatal(err)
	}
}

const groupedSpec = `
scenarios:
  - name: grouped
    node:
      sequence:
        - action: connect
        - group: saga
          retry:
            times: 3
            node:
              sequence:
                - wait: 4ms
                - action: poll
        - group: " Saga "
          wait: 3ms
        - wait: 2ms
  - name: single
    node:
      group: io
      action: connect
`

// YAML 顶层步骤可以写 group：步骤名不变，报告里带上组名，组内步骤的耗时累计到本次场景运行
// （robot.Context.GroupTimes），不在组里的步骤不算。group 只能写在顶层步骤上。
func TestSpecGroupsTopLevelSteps(t *testing.T) {
	scenarios, err := scenario.ParseSpec([]byte(groupedSpec))
	if err != nil {
		t.Fatal(err)
	}
	var log []string
	rb := newRecordingContext(&log, nil)
	recorder := &session.Recorder{}
	rb.Recorder = recorder
	start := time.Now()
	if err := scenarios[0].Run(context.Background(), rb); err != nil {
		t.Fatal(err)
	}
	wall := time.Since(start)
	steps := recorder.StepSnapshot()
	var got []string
	for _, s := range steps {
		got = append(got, s.Name+"="+s.Group)
	}
	if strings.Join(got, ";") != "01 connect=;02 retry(wait 4ms,poll)=saga;03 wait 3ms=saga;04 wait 2ms=" {
		t.Fatalf("steps = %q", got)
	}
	times := rb.GroupTimes()
	if len(times) != 1 || times["saga"] < 7*time.Millisecond || times["saga"]+2*time.Millisecond > wall {
		t.Fatalf("group times = %v (run took %v)", times, wall)
	}

	single := robot.NewContext(robot.Config{Recorder: &session.Recorder{}})
	single.RunAction = rb.RunAction
	if err := scenarios[1].Run(context.Background(), single); err != nil {
		t.Fatal(err)
	}
	if _, ok := single.GroupTimes()["io"]; !ok {
		t.Fatalf("single-node root group lost: %v", single.GroupTimes())
	}

	for name, spec := range map[string]string{
		"nested":        "scenarios:\n  - name: x\n    node:\n      sequence:\n        - retry: {times: 1, node: {group: saga, action: a}}\n",
		"sequence root": "scenarios:\n  - name: x\n    node:\n      group: saga\n      sequence:\n        - action: a\n",
	} {
		if _, err := scenario.ParseSpec([]byte(spec)); err == nil || !strings.Contains(err.Error(), "group") {
			t.Errorf("%s group accepted: %v", name, err)
		}
	}
}

// 代码搭的场景用 StepInGroup；同组嵌套只按最外层算一次，不同组各算各的。
func TestNestedSameGroupStepsCountOnce(t *testing.T) {
	var log []string
	rb := newRecordingContext(&log, nil)
	rb.Recorder = &session.Recorder{}
	tree := scenario.Sequence(
		scenario.StepInGroup("saga", "outer", scenario.Sequence(
			scenario.Wait(3*time.Millisecond),
			scenario.StepInGroup("SAGA", "inner", scenario.Wait(10*time.Millisecond)),
		)),
		scenario.StepInGroup("io", "other", scenario.Wait(time.Millisecond)),
	)
	start := time.Now()
	if err := tree.Run(context.Background(), rb); err != nil {
		t.Fatal(err)
	}
	wall := time.Since(start)
	times := rb.GroupTimes()
	if times["saga"] < 13*time.Millisecond || times["saga"]+time.Millisecond > wall || times["io"] < time.Millisecond {
		t.Fatalf("group times = %v (run took %v)", times, wall)
	}
	for _, s := range rb.Recorder.StepSnapshot() {
		if want := map[string]string{"inner": "saga", "outer": "saga", "other": "io"}[s.Name]; s.Group != want {
			t.Fatalf("step %s group = %q, want %q", s.Name, s.Group, want)
		}
	}
}

// nil 注册表不能注册；声明式树里的空节点报 "node is required"。
// 历史：U-0149
func TestRegistryAndSpecRefuseNilNodes(t *testing.T) {
	var none *scenario.Registry
	if err := none.Register(nil); err == nil || !strings.Contains(err.Error(), "registry is nil") {
		t.Fatalf("Register on a nil registry = %v", err)
	}
	_, err := scenario.ParseSpec([]byte("scenarios:\n  - name: x\n    node:\n      sequence:\n        - action: a\n        - ~\n"))
	if err == nil || !strings.Contains(err.Error(), "node is required") {
		t.Fatalf("ParseSpec with a null sequence element = %v", err)
	}
}
