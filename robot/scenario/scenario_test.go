package scenario_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/cube-core/robot"
	"github.com/tjbdwanghaibo/cube-core/robot/scenario"
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

func TestSpecRejectsBrokenDocuments(t *testing.T) {
	for name, spec := range map[string]string{
		"unknown key":    "scenarios:\n  - name: x\n    node: {action: a}\n    typo: 1\n",
		"two node kinds": "scenarios:\n  - name: x\n    node: {action: a, wait: 1s}\n",
		"empty node":     "scenarios:\n  - name: x\n    node: {}\n",
		"bad duration":   "scenarios:\n  - name: x\n    node: {wait: soon}\n",
		"no name":        "scenarios:\n  - node: {action: a}\n",
		"duplicate":      "scenarios:\n  - name: x\n    node: {action: a}\n  - name: X\n    node: {action: a}\n",
	} {
		if _, err := scenario.ParseSpec([]byte(spec)); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}
