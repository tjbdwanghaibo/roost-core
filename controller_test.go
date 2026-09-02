package ai

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	coreflow "github.com/tjbdwanghaibo/cube-core/actionflow"
	coreai "github.com/tjbdwanghaibo/cube-core/ai"
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

// Race-freedom is necessary but not sufficient: a Blackboard that dropped
// writes, or returned a Snapshot aliasing live state, would have passed the
// previous version of this test since it discarded every return value.
func TestBlackboardConcurrentAccessKeepsEveryWorkersWrite(t *testing.T) {
	board := NewBlackboard()
	const workers, rounds = 8, 100
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func(id int) {
			defer wait.Done()
			key := fmt.Sprintf("worker_%d", id)
			for index := 0; index < rounds; index++ {
				board.Set(key, index)
				// Reading a neighbour's key concurrently is what the race
				// detector is here for.
				_, _ = board.Get(fmt.Sprintf("worker_%d", (id+1)%workers))
				_ = board.Snapshot()
			}
		}(worker)
	}
	wait.Wait()

	for worker := 0; worker < workers; worker++ {
		key := fmt.Sprintf("worker_%d", worker)
		value, ok := board.Get(key)
		if !ok {
			t.Fatalf("%s is missing after concurrent writes", key)
		}
		if value != rounds-1 {
			t.Fatalf("%s = %v, want the last write %d", key, value, rounds-1)
		}
	}

	// Snapshot must be a copy: mutating it must not reach the board.
	snapshot := board.Snapshot()
	snapshot["worker_0"] = "tampered"
	if value, _ := board.Get("worker_0"); value == "tampered" {
		t.Fatal("Snapshot aliases live state; it must return a copy")
	}
}
