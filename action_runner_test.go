package actionflow

import (
	"errors"
	"strings"
	"testing"
	"time"

	coreflow "github.com/tjbdwanghaibo/roost-core/actionflow"
)

type runnerTestAction struct {
	label       string
	done        bool
	panicOnTick bool
	startErr    error
	starts      int
	cancels     int
}

func (*runnerTestAction) Kind() coreflow.ActionKind { return 1 }
func (a *runnerTestAction) Start(*coreflow.ActionContext) error {
	a.starts++
	return a.startErr
}
func (a *runnerTestAction) Tick(*coreflow.ActionContext) (bool, coreflow.ActionResult) {
	if a.panicOnTick {
		panic("tick failed")
	}
	return a.done, coreflow.ActionResult{Status: coreflow.ActionStatusSuccess}
}
func (a *runnerTestAction) Cancel(*coreflow.ActionContext, string) { a.cancels++ }

func newRunnerForTest(t *testing.T, ended *[]coreflow.ActionReason, reported *[]error) *ActionRunner {
	t.Helper()
	registry := NewRegistry()
	if err := registry.RegisterAction(1, func(param any) (coreflow.Action, error) {
		action, ok := param.(*runnerTestAction)
		if !ok {
			return nil, errors.New("invalid action")
		}
		return action, nil
	}); err != nil {
		t.Fatal(err)
	}
	runner, err := NewActionRunner(ActionRunnerConfig{
		Registry: registry,
		GroupForKind: func(coreflow.ActionKind) (coreflow.ActionGroup, bool) {
			return 1, true
		},
		Hooks: ActionRunnerHooks{
			OnEnded: func(_ ActionSnapshot, reason coreflow.ActionReason) {
				*ended = append(*ended, reason)
			},
			OnError: func(err error) { *reported = append(*reported, err) },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func TestActionRunnerExplicitStartPrecedesQueue(t *testing.T) {
	var ended []coreflow.ActionReason
	var reported []error
	runner := newRunnerForTest(t, &ended, &reported)
	old := &runnerTestAction{label: "old"}
	queued := &runnerTestAction{label: "queued"}
	replacement := &runnerTestAction{label: "replacement", done: true}
	if _, err := runner.Start(1, old, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Enqueue(1, queued, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Start(1, replacement, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := runner.Current(1); got != replacement {
		t.Fatalf("current action = %p, want replacement %p", got, replacement)
	}
	if queued.starts != 0 || replacement.starts != 1 || old.cancels != 1 {
		t.Fatalf("unexpected lifecycle: queued starts=%d replacement starts=%d old cancels=%d", queued.starts, replacement.starts, old.cancels)
	}
	if len(ended) != 1 || ended[0].Result.Status != coreflow.ActionStatusCanceled {
		t.Fatalf("replacement reason must be canceled: %+v", ended)
	}
	if err := runner.Tick(1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := runner.Current(1); got != queued || queued.starts != 1 {
		t.Fatalf("queued action was not started after replacement completed: current=%p starts=%d", got, queued.starts)
	}
	if len(reported) != 0 {
		t.Fatalf("unexpected errors: %v", reported)
	}
}

func TestActionRunnerEnqueueStartsIdleGroup(t *testing.T) {
	var ended []coreflow.ActionReason
	var reported []error
	runner := newRunnerForTest(t, &ended, &reported)
	action := &runnerTestAction{label: "queued"}
	id, err := runner.Enqueue(1, action, 0)
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 || runner.Current(1) != action || action.starts != 1 || runner.QueueLength(1) != 0 {
		t.Fatalf("idle enqueue did not start: id=%d current=%v starts=%d queued=%d", id, runner.Current(1), action.starts, runner.QueueLength(1))
	}
}

func TestActionRunnerQueuedStartFailureContinuesQueue(t *testing.T) {
	var ended []coreflow.ActionReason
	var reported []error
	runner := newRunnerForTest(t, &ended, &reported)
	runner.Freeze(1)
	failed := &runnerTestAction{startErr: errors.New("start")}
	next := &runnerTestAction{}
	if _, err := runner.Enqueue(1, failed, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Enqueue(1, next, 0); err != nil {
		t.Fatal(err)
	}
	if err := runner.Recover(1, time.Now()); !errors.Is(err, failed.startErr) {
		t.Fatalf("recover error = %v", err)
	}
	if runner.Current(1) != next || failed.cancels != 1 || next.starts != 1 {
		t.Fatalf("queue did not continue: current=%v failed cancels=%d next starts=%d", runner.Current(1), failed.cancels, next.starts)
	}
	if len(ended) != 1 || ended[0].Result.Status != coreflow.ActionStatusFailed || len(reported) != 1 {
		t.Fatalf("failed queue item was not observed: ended=%+v reported=%v", ended, reported)
	}
}

func TestActionRunnerRecoversTickPanic(t *testing.T) {
	var ended []coreflow.ActionReason
	var reported []error
	runner := newRunnerForTest(t, &ended, &reported)
	action := &runnerTestAction{panicOnTick: true}
	if _, err := runner.Start(1, action, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := runner.Tick(1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if runner.Current(1) != nil {
		t.Fatal("panicking action must be removed")
	}
	if len(ended) != 1 || ended[0].Result.Status != coreflow.ActionStatusFailed {
		t.Fatalf("panic must end as failure: %+v", ended)
	}
	if len(reported) != 1 || !strings.Contains(reported[0].Error(), "tick panic") {
		t.Fatalf("panic must be reported: %v", reported)
	}
}

func TestRegistrySealAndBuilderPanic(t *testing.T) {
	registry := NewRegistry()
	if err := registry.RegisterAction(1, func(any) (coreflow.Action, error) { panic("builder") }); err != nil {
		t.Fatal(err)
	}
	registry.Seal()
	if err := registry.RegisterMission(1, func() coreflow.Mission { return nil }); !errors.Is(err, ErrRegistrySealed) {
		t.Fatalf("register after seal: %v", err)
	}
	if _, err := registry.BuildAction(1, nil); err == nil || !strings.Contains(err.Error(), "builder") {
		t.Fatalf("builder panic not converted to error: %v", err)
	}
}

func BenchmarkActionRunnerTick(b *testing.B) {
	registry := NewRegistry()
	action := &runnerTestAction{}
	if err := registry.RegisterAction(1, func(any) (coreflow.Action, error) { return action, nil }); err != nil {
		b.Fatal(err)
	}
	runner, err := NewActionRunner(ActionRunnerConfig{
		Registry: registry,
		GroupForKind: func(coreflow.ActionKind) (coreflow.ActionGroup, bool) {
			return 1, true
		},
	})
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now()
	if _, err := runner.Start(1, nil, 0, now); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if err := runner.Tick(1, now); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkActionRunnerLifecycle(b *testing.B) {
	registry := NewRegistry()
	if err := registry.RegisterAction(1, func(any) (coreflow.Action, error) {
		return &runnerTestAction{done: true}, nil
	}); err != nil {
		b.Fatal(err)
	}
	runner, err := NewActionRunner(ActionRunnerConfig{
		Registry: registry,
		GroupForKind: func(coreflow.ActionKind) (coreflow.ActionGroup, bool) {
			return 1, true
		},
	})
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now()
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := runner.Start(1, nil, 0, now); err != nil {
			b.Fatal(err)
		}
		if err := runner.Tick(1, now); err != nil {
			b.Fatal(err)
		}
	}
}
