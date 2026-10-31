package runner_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/robot"
	"github.com/tjbdwanghaibo/roost-core/robot/runner"
	"github.com/tjbdwanghaibo/roost-core/robot/scenario"
)

func TestLaunchErrorCancelsAlreadyRunningRobots(t *testing.T) {
	entered := make(chan struct{})
	registry := scenario.NewRegistry()
	registry.MustRegister(scenario.New("hold", scenario.NodeFunc(func(ctx context.Context, _ *robot.Context) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	})))
	failed := errors.New("identity unavailable")
	r := runner.New(runner.Config{Count: 2, Scenario: "hold"}, runner.WithScenarioRegistry(registry), runner.WithIdentityProvider(func(index int) (int64, error) {
		if index == 2 {
			<-entered
			return 0, failed
		}
		return int64(index), nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	select {
	case err := <-done:
		if !errors.Is(err, failed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("startup failure did not cancel the previously launched robot")
	}
	if r.Stats().Online != 0 {
		t.Fatal(r.Stats())
	}
}

func TestArrivalCapacityAndRunOwnership(t *testing.T) {
	registry := scenario.NewRegistry()
	registry.MustRegister(scenario.New("hold", scenario.NodeFunc(func(ctx context.Context, _ *robot.Context) error { <-ctx.Done(); return ctx.Err() })))
	r := runner.New(runner.Config{Executor: runner.ExecutorArrivalRate, Rate: 1000, MaxConcurrent: 2, Scenario: "hold", Duration: 80 * time.Millisecond}, runner.WithScenarioRegistry(registry))
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats := r.Stats()
	if stats.PeakOnline != 2 || stats.Online != 0 || stats.DroppedCapacity == 0 || stats.Started != 2 {
		t.Fatalf("unbounded arrival: %+v", stats)
	}
	if stats.Scheduled != stats.Started+stats.DroppedCapacity+stats.DroppedSchedule+stats.LaunchFailed {
		t.Fatalf("lost arrivals: %+v", stats)
	}
	if err := r.Run(context.Background()); err == nil {
		t.Fatal("runner reuse accepted")
	}
	if err := r.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStopWaitsForSchedulerAndRobotCleanup(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	registry := scenario.NewRegistry()
	registry.MustRegister(scenario.New("cleanup", scenario.NodeFunc(func(ctx context.Context, _ *robot.Context) error {
		close(entered)
		<-ctx.Done()
		<-release
		return ctx.Err()
	})))
	r := runner.New(runner.Config{Scenario: "cleanup"}, runner.WithScenarioRegistry(registry))
	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background()) }()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Stop(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop lost cleanup: %v", err)
	}
	close(release)
	if err := r.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if r.Stats().Online != 0 {
		t.Fatal(r.Stats())
	}
}
