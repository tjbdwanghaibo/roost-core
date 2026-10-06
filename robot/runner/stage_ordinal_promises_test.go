package runner_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/robot/action"
	"github.com/tjbdwanghaibo/roost-core/robot/runner"
	"github.com/tjbdwanghaibo/roost-core/robot/scenario"
)

// RR-20261006-09：Stage 先缩后扩时，新机器人的序号（以及由序号算出的 PlayerID）只增不回收。
// 旧实现扩容从 launched+1 起编号，而缩容把 launched 减回去，于是扩回来的机器人复用刚被停掉的
// 序号——旧机器人可能还在收尾（关会话、发登出），两个机器人同时用一个 PlayerID（N12 观察 O9）。
func TestStageRegrowDoesNotReuseOrdinals(t *testing.T) {
	var mu sync.Mutex
	var ordinals []int
	scenarios := scenario.NewRegistry()
	r := runner.New(runner.Config{
		Executor: runner.ExecutorLooping,
		Count:    3,
		Scenario: "idle",
		Stages: []runner.Stage{
			{Target: 1, Duration: 30 * time.Millisecond},
			{Target: 3, Duration: 30 * time.Millisecond},
		},
	},
		runner.WithScenarioRegistry(scenarios),
		runner.WithActionRegistry(action.NewRegistry()),
		runner.WithIdentityProvider(func(index int) (int64, error) {
			mu.Lock()
			ordinals = append(ordinals, index)
			mu.Unlock()
			return int64(1000 + index), nil
		}),
	)
	scenarios.MustRegister(scenario.New("idle", scenario.Wait(5*time.Millisecond)))
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	seen := map[int]bool{}
	for _, ordinal := range ordinals {
		if seen[ordinal] {
			t.Fatalf("robot ordinal %d was handed out twice across the stages (launch order %v); "+
				"a regrown stage must not reuse the ordinal (and player id) of a robot it just stopped", ordinal, ordinals)
		}
		seen[ordinal] = true
	}
	if len(ordinals) != 5 {
		t.Fatalf("launched %d robots (%v), want 5: three, shrink to one, two more", len(ordinals), ordinals)
	}
}
