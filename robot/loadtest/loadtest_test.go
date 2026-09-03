package loadtest_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/admin"
	"github.com/tjbdwanghaibo/roost-core/robot/loadtest"
	"github.com/tjbdwanghaibo/roost-core/robot/runner"
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

func TestThresholdViolationFailsRun(t *testing.T) {
	fake := &fakeRunner{delay: 20 * time.Millisecond, stats: runner.Stats{Started: 10, Success: 8, Failure: 2}}
	m := newManager(t, fake, []loadtest.Threshold{
		{Metric: "error_rate", Max: 0.10}, // actual 0.2 -> violated
		{Metric: "p99", Max: 1.0},         // actual 5ms -> pass
	}, 5*time.Millisecond)
	if _, err := m.Start(context.Background(), loadtest.StartRequest{Profile: "smoke"}); err != nil {
		t.Fatal(err)
	}
	done := waitDone(t, m)
	if done.State != loadtest.StateFailed || done.StopReason != loadtest.StopReasonThreshold {
		t.Fatalf("threshold verdict = %+v", done)
	}
	violated := 0
	for _, result := range done.Thresholds {
		if result.Violated {
			violated++
		}
	}
	if violated != 1 {
		t.Fatalf("thresholds = %+v", done.Thresholds)
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
