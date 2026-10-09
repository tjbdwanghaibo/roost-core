package manager

import (
	"context"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/internal/stopcontract"
)

// A3 / RR-20261005-NC-170：Engine.Stop 套共用停机契约骨架。卡住的工作是一个还没排空的 manager，
// “资源”是它依赖的 manager——只能在它停完之后被停。
func TestEngineStopContract(t *testing.T) {
	dependent := &drainingManager{name: "dependent", dependsOn: []string{"dependency"}, initiated: make(chan struct{}), drained: make(chan struct{})}
	dependency := &dependencyManager{name: "dependency", dependent: dependent}
	var engine *Engine
	stopcontract.Check(t, stopcontract.Hooks{
		Start: func(testing.TB) { engine, _ = newStartedEngine(t, dependency, dependent) },
		Stop:  func(ctx context.Context) error { return engine.Stop(ctx) },
		Release: func() {
			close(dependent.drained)
		},
		Released: func() bool { return dependency.stops.Load() > 0 },
	})
	if dependency.stoppedUnderUsage.Load() || dependency.stops.Load() != 1 {
		t.Fatalf("dependency stops=%d stoppedUnderUsage=%v, want one stop after the dependent drained", dependency.stops.Load(), dependency.stoppedUnderUsage.Load())
	}
}
