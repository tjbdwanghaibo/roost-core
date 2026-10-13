package loadtest_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/admin"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"github.com/tjbdwanghaibo/roost-core/robot"
	"github.com/tjbdwanghaibo/roost-core/robot/loadtest"
	"github.com/tjbdwanghaibo/roost-core/robot/runner"
	"github.com/tjbdwanghaibo/roost-core/robot/scenario"
)

// fakeRunner completes after a short scripted delay with scripted stats.
type fakeRunner struct {
	delay   time.Duration
	stats   runner.Stats
	err     error
	stopped atomic.Bool
}

func (f *fakeRunner) Run(ctx context.Context) error {
	timer := time.NewTimer(f.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil
	case <-timer.C:
		return f.err
	}
}

func (f *fakeRunner) Stop(context.Context) error {
	f.stopped.Store(true)
	return nil
}

func (f *fakeRunner) Stats() runner.Stats { return f.stats }

func newManager(t *testing.T, fake *fakeRunner, thresholds []loadtest.Threshold, quantile time.Duration) *loadtest.Manager {
	t.Helper()
	return loadtest.New(loadtest.Config{
		AllowAdminStart: true,
		Profiles: map[string]loadtest.Profile{
			"smoke": {
				Run:        runner.Config{Count: 3, Scenario: "ping"},
				Thresholds: thresholds,
			},
		},
		RunnerFactory: func(string, string, loadtest.Profile) loadtest.Runner { return fake },
		Quantile: func(string, string, string, float64) time.Duration {
			return quantile
		},
	})
}

func waitDone(t *testing.T, m *loadtest.Manager) loadtest.RunSnapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		history := m.History(loadtest.HistoryRequest{Limit: 1})
		if len(history.Runs) > 0 {
			return history.Runs[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("run never finished")
	return loadtest.RunSnapshot{}
}

func TestManagerLifecycleAndSingleActiveRun(t *testing.T) {
	fake := &fakeRunner{delay: 100 * time.Millisecond, stats: runner.Stats{Started: 3, Success: 3}}
	m := newManager(t, fake, nil, 5*time.Millisecond)

	snapshot, err := m.Start(context.Background(), loadtest.StartRequest{Profile: "smoke"})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != loadtest.StateRunning {
		t.Fatalf("state = %s", snapshot.State)
	}
	// Only one run at a time.
	if _, err := m.Start(context.Background(), loadtest.StartRequest{Profile: "smoke"}); !errors.Is(err, loadtest.ErrRunActive) {
		t.Fatalf("second run accepted: %v", err)
	}
	done := waitDone(t, m)
	if done.State != loadtest.StateFinished || done.Stats.Success != 3 {
		t.Fatalf("done = %+v", done)
	}
	if done.QuantilesMS["p99"] != 5 {
		t.Fatalf("quantiles = %+v", done.QuantilesMS)
	}
	// After completion a new run is accepted again.
	if _, err := m.Start(context.Background(), loadtest.StartRequest{Profile: "smoke"}); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if _, err := m.Stop(context.Background(), loadtest.StopRequest{}); err != nil {
		t.Fatal(err)
	}
	if !fake.stopped.Load() {
		t.Fatal("stop not propagated to runner")
	}
}

func TestAdminCommandsAndReport(t *testing.T) {
	fake := &fakeRunner{delay: 20 * time.Millisecond, stats: runner.Stats{Started: 2, Success: 2}}
	m := newManager(t, fake, nil, 7*time.Millisecond)
	reg := admin.NewRegistry()
	if err := loadtest.RegisterAdminCommands(reg, m); err != nil {
		t.Fatal(err)
	}
	result, err := reg.Execute(context.Background(), admin.Command{
		Name:    loadtest.AdminCommandStart,
		Payload: []byte(`{"profile":"smoke"}`),
	})
	if err != nil || !result.OK {
		t.Fatalf("admin start: %+v %v", result, err)
	}
	waitDone(t, m)
	report, err := reg.Execute(context.Background(), admin.Command{Name: loadtest.AdminCommandReport})
	if err != nil || !report.OK {
		t.Fatalf("admin report: %+v %v", report, err)
	}
	markdown, _ := report.Data["report"].(map[string]any)["markdown"].(string)
	for _, want := range []string{"Load-test report", "| p50 | p90 | p95 | p99 |", "| 7ms | 7ms | 7ms | 7ms |"} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("report missing %q:\n%s", want, markdown)
		}
	}
}

func TestAdminStartGate(t *testing.T) {
	fake := &fakeRunner{delay: time.Millisecond}
	m := loadtest.New(loadtest.Config{
		Profiles:      map[string]loadtest.Profile{"smoke": {Run: runner.Config{Scenario: "ping"}}},
		RunnerFactory: func(string, string, loadtest.Profile) loadtest.Runner { return fake },
	})
	if _, err := m.StartAdmin(context.Background(), loadtest.StartRequest{Profile: "smoke"}); !errors.Is(err, loadtest.ErrAdminStartDisabled) {
		t.Fatalf("admin gate bypassed: %v", err)
	}
}

// 管理命令注册缺注册表 / 管理器时拒绝；nil 管理器的 Start / StartAdmin / Report 报错；无已完成运行时
// Report 拒绝、未知 RunID 拒绝；无 profile 名（且无 ActiveProfile）报 ErrProfileRequired，未知 profile
// 报 ErrProfileNotFound。
// 历史：U-0148
func TestManagerEntryPointsRefuseMissingPartsAndUnknownRuns(t *testing.T) {
	ctx := context.Background()
	fake := &fakeRunner{stats: runner.Stats{Started: 1, Success: 1}}
	m := newManager(t, fake, nil, time.Millisecond)
	if err := loadtest.RegisterAdminCommands(nil, m); err == nil || !strings.Contains(err.Error(), "admin registry is required") {
		t.Fatalf("RegisterAdminCommands(nil registry) = %v", err)
	}
	if err := loadtest.RegisterAdminCommands(admin.NewRegistry(), nil); err == nil || !strings.Contains(err.Error(), "manager is required") {
		t.Fatalf("RegisterAdminCommands(nil manager) = %v", err)
	}
	var none *loadtest.Manager
	if _, err := none.Start(ctx, loadtest.StartRequest{Profile: "smoke"}); err == nil || !strings.Contains(err.Error(), "manager is nil") {
		t.Fatalf("Start on a nil manager = %v", err)
	}
	if _, err := none.StartAdmin(ctx, loadtest.StartRequest{Profile: "smoke"}); err == nil || !strings.Contains(err.Error(), "manager is nil") {
		t.Fatalf("StartAdmin on a nil manager = %v", err)
	}
	if _, err := none.Report(loadtest.ReportRequest{}); err == nil || !strings.Contains(err.Error(), "manager is nil") {
		t.Fatalf("Report on a nil manager = %v", err)
	}
	if _, err := m.Report(loadtest.ReportRequest{}); err == nil || !strings.Contains(err.Error(), "no finished runs") {
		t.Fatalf("Report before any run = %v", err)
	}
	if _, err := m.Start(ctx, loadtest.StartRequest{}); !errors.Is(err, loadtest.ErrProfileRequired) {
		t.Fatalf("Start without a profile = %v", err)
	}
	if _, err := m.Start(ctx, loadtest.StartRequest{Profile: "nope"}); !errors.Is(err, loadtest.ErrProfileNotFound) || !strings.Contains(err.Error(), "available=") {
		t.Fatalf("Start with an unknown profile = %v", err)
	}
	if _, err := m.Start(ctx, loadtest.StartRequest{Profile: "smoke"}); err != nil {
		t.Fatal(err)
	}
	done := waitDone(t, m)
	if _, err := m.Report(loadtest.ReportRequest{RunID: "ghost"}); err == nil || !strings.Contains(err.Error(), `run "ghost" not in history`) {
		t.Fatalf("Report of an unknown run = %v", err)
	}
	if report, err := m.Report(loadtest.ReportRequest{RunID: done.RunID}); err != nil || report["markdown"] == "" {
		t.Fatalf("Report of the finished run = (%v, %v)", report, err)
	}
}

// 运行因 profile 的 Duration 到期而结束时停止原因记 duration（Duration 在 runner 内部生效，manager
// 必须识别这次到期，不能记成 completed）；在 Duration 之内自然跑完的运行仍记 completed。
// 历史：RR-20261005-NC-266

func TestARunCutByItsDurationStopsWithReasonDuration(t *testing.T) {
	scenarios := scenario.NewRegistry()
	scenarios.MustRegister(scenario.New("until-canceled", scenario.NodeFunc(func(ctx context.Context, _ *robot.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})))
	m := loadtest.New(loadtest.Config{
		Profiles: map[string]loadtest.Profile{
			"smoke": {Run: runner.Config{Count: 1, Scenario: "until-canceled", Duration: 100 * time.Millisecond}},
		},
		RunnerOptions: []runner.Option{runner.WithScenarioRegistry(scenarios)},
	})
	if _, err := m.Start(context.Background(), loadtest.StartRequest{Profile: "smoke"}); err != nil {
		t.Fatal(err)
	}
	run := waitDone(t, m)
	if run.StopReason != loadtest.StopReasonDuration {
		t.Fatalf("a run cut by its 100ms Duration ended %s/%s; want stop_reason=%s", run.State, run.StopReason, loadtest.StopReasonDuration)
	}
}

// 对照：在 Duration 之内自然跑完的运行仍记 completed。
func TestARunThatFinishesBeforeItsDurationIsCompleted(t *testing.T) {
	scenarios := scenario.NewRegistry()
	scenarios.MustRegister(scenario.New("instant", scenario.NodeFunc(func(context.Context, *robot.Context) error {
		return nil
	})))
	m := loadtest.New(loadtest.Config{
		Profiles: map[string]loadtest.Profile{
			"smoke": {Run: runner.Config{Count: 2, Scenario: "instant", Duration: time.Minute}},
		},
		RunnerOptions: []runner.Option{runner.WithScenarioRegistry(scenarios)},
	})
	if _, err := m.Start(context.Background(), loadtest.StartRequest{Profile: "smoke"}); err != nil {
		t.Fatal(err)
	}
	run := waitDone(t, m)
	if run.State != loadtest.StateFinished || run.StopReason != loadtest.StopReasonCompleted {
		t.Fatalf("a run that finished inside its Duration ended %s/%s; want finished/completed", run.State, run.StopReason)
	}
}

// 运行的 robot.runner.* 序列带 run 标签、跟着运行记录走：运行被挤出历史（HistoryLimit）时，Manager
// 删掉带它 run 标签的全部序列，/metrics 里不再有它，序列基数有上界，新运行的直方图不会因触到
// 每指标序列上限被丢弃。
// 历史：维护者第十二轮决定（metrics 按标签删除）

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
