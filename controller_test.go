package ai

import (
	"errors"
	"sync"
	"testing"
	"time"

	coreai "github.com/tjbdwanghaibo/cube-core/ai"
	coreflow "github.com/tjbdwanghaibo/cube-core/taskflow"
)

type controllerTestStrategy struct {
	name      string
	initErr   error
	stopCount int
	tickCount int
}

func (s *controllerTestStrategy) Name() string                    { return s.name }
func (s *controllerTestStrategy) Init(*coreai.Context) error      { return s.initErr }
func (s *controllerTestStrategy) Tick(*coreai.Context, time.Time) { s.tickCount++ }
func (*controllerTestStrategy) OnActionEnd(*coreai.Context, int64, coreflow.ActionKind, coreflow.ActionReason) {
}
func (*controllerTestStrategy) OnMissionEnd(*coreai.Context, coreflow.Mission, coreflow.ActionReason) {
}
func (*controllerTestStrategy) CanStopByNext(coreai.Strategy) bool { return true }
func (s *controllerTestStrategy) Stop(*coreai.Context, string)     { s.stopCount++ }

func TestControllerFailedInitKeepsPreviousStrategy(t *testing.T) {
	endedActions := 0
	controller := NewController(ControllerHooks{EndActions: func(coreflow.ActionReason) { endedActions++ }})
	previous := &controllerTestStrategy{name: "previous"}
	if err := controller.SetStrategy(previous); err != nil {
		t.Fatal(err)
	}
	failed := &controllerTestStrategy{name: "failed", initErr: errors.New("init")}
	if err := controller.SetStrategy(failed); !errors.Is(err, ErrStrategyInit) {
		t.Fatalf("SetStrategy error = %v", err)
	}
	if controller.Strategy() != previous {
		t.Fatal("failed replacement must keep previous strategy")
	}
	if previous.stopCount != 0 || endedActions != 0 || failed.stopCount != 1 {
		t.Fatalf("unexpected rollback lifecycle: previous stops=%d ended=%d failed stops=%d", previous.stopCount, endedActions, failed.stopCount)
	}
}

func TestBehaviorTreeReadyDoesNotAdvanceSequence(t *testing.T) {
	calls := 0
	ready := &FuncNode[int]{Run: func(*int) Status { return StatusReady }}
	next := &FuncNode[int]{Run: func(*int) Status { calls++; return StatusSuccess }}
	tree := &Sequence[int]{Children: []Node[int]{ready, next}}
	if status := tree.Tick(nil); status != StatusRunning {
		t.Fatalf("status = %v, want running", status)
	}
	if calls != 0 {
		t.Fatal("sequence advanced past ready child")
	}
}

func TestBlackboardConcurrentAccess(t *testing.T) {
	board := NewBlackboard()
	var wait sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func(value int) {
			defer wait.Done()
			for index := 0; index < 100; index++ {
				board.Set("key", value)
				_, _ = board.Get("key")
				_ = board.Snapshot()
			}
		}(worker)
	}
	wait.Wait()
}
