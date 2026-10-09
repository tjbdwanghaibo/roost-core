package loadtest_test

// 维护者第十二轮决定（metrics 按标签删除；N12 观察 O2）：loadtest 给 robot.runner.* 序列加了
// run 标签，每次运行新增一组序列。旧实现从不删除，长期运行的控制面每跑一次多一组，约一千次后触到
// 每指标序列上限，新运行的直方图被丢弃、阈值无从判定。现在运行的序列跟着运行记录走：运行被挤出
// 历史（HistoryLimit）时，Manager 删掉带它 run 标签的全部序列，/metrics 里不再有它，基数有上界。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"github.com/tjbdwanghaibo/roost-core/robot"
	"github.com/tjbdwanghaibo/roost-core/robot/loadtest"
	"github.com/tjbdwanghaibo/roost-core/robot/runner"
	"github.com/tjbdwanghaibo/roost-core/robot/scenario"
)

func TestRunSeriesLeaveTheRegistryWithTheRunRecord(t *testing.T) {
	old := metrics.DefaultRegistry()
	reg := metrics.NewRegistry()
	metrics.SetDefaultRegistry(reg)
	t.Cleanup(func() { metrics.SetDefaultRegistry(old) })

	scenarios := scenario.NewRegistry()
	scenarios.MustRegister(scenario.New("instant", scenario.NodeFunc(func(context.Context, *robot.Context) error {
		return nil
	})))
	const historyLimit = 2
	m := loadtest.New(loadtest.Config{
		HistoryLimit: historyLimit,
		Profiles: map[string]loadtest.Profile{
			"smoke": {Run: runner.Config{Count: 2, Scenario: "instant"}},
		},
		RunnerOptions: []runner.Option{runner.WithScenarioRegistry(scenarios)},
	})

	var runIDs []string
	seriesAfter := map[int]int{}
	for i := 1; i <= 6; i++ {
		started, err := m.Start(context.Background(), loadtest.StartRequest{Profile: "smoke"})
		if err != nil {
			t.Fatal(err)
		}
		waitForRun(t, m, started.RunID)
		runIDs = append(runIDs, started.RunID)
		seriesAfter[i] = reg.SeriesCount()
	}

	text := string(metrics.PrometheusText(reg.Snapshot()))
	kept := runIDs[len(runIDs)-historyLimit:]
	for _, runID := range runIDs[:len(runIDs)-historyLimit] {
		if strings.Contains(text, `run="`+runID+`"`) {
			t.Errorf("run %s left the history but its series are still exported:\n%s", runID, grepLines(text, runID))
		}
	}
	for _, runID := range kept {
		if !strings.Contains(text, `run="`+runID+`"`) {
			t.Errorf("run %s is still in the history but its series are gone; recent runs must stay scrapable", runID)
		}
	}
	// 基数有上界：历史满了之后，每次运行加一组、删一组。
	if seriesAfter[6] != seriesAfter[historyLimit+1] || seriesAfter[6] > seriesAfter[historyLimit]*2 {
		t.Fatalf("series count kept growing across runs: %v (history limit %d)", seriesAfter, historyLimit)
	}
}

func waitForRun(t *testing.T, m *loadtest.Manager, runID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, run := range m.History(loadtest.HistoryRequest{Limit: 50}).Runs {
			if run.RunID == runID {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("run %s never finished", runID)
}

func grepLines(text, needle string) string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, needle) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
