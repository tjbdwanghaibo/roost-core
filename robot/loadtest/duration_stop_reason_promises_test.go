package loadtest_test

// RR-20261005-NC-266（N12 观察 O11）：运行因 profile 的 Duration 到期而结束时，停止原因记 duration。
//
// Duration 只在 runner 内部生效（Runner.Run 在 manager 交来的 ctx 上再套 WithTimeout），manager 的
// stopReasonFromContext 读的是自己的 run ctx，看不到这次到期，于是记成 completed——好像所有机器人都
// 正常跑完了。N12 真实网关上 `-duration 1s` 那次就是 `stop_reason=completed`。

import (
	"context"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/robot"
	"github.com/tjbdwanghaibo/roost-core/robot/loadtest"
	"github.com/tjbdwanghaibo/roost-core/robot/runner"
	"github.com/tjbdwanghaibo/roost-core/robot/scenario"
)

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

// 控制：在 Duration 之内自然跑完的运行仍记 completed。
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
