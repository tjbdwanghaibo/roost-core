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
