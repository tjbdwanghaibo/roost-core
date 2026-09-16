package manager

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
)

// abortingManager starts fine, but by the time it returns a shutdown has been
// requested (as a SIGTERM during a slow start would), and its own stop fails.
type abortingManager struct {
	mod     *Engine
	stopErr error
}

func (m *abortingManager) Name() string { return "aborting" }
func (m *abortingManager) Start(*app.Registry) error {
	m.mod.mu.Lock()
	m.mod.stopping = true
	m.mod.mu.Unlock()
	return nil
}
func (m *abortingManager) Stop()                                 {}
func (m *abortingManager) StopWithContext(context.Context) error { return m.stopErr }

// U-0152 · C2 · engine.go（原 kit manager_mod.go:131）（U-0150 留待）：启动途中收到关停，已启动的管理器回滚失败时，
// 错误要同时说明"被关停中止"和回滚的原因；后面的管理器不再启动。
func TestStartAbortedByShutdownReportsARollbackFailure(t *testing.T) {
	stopErr := errors.New("flush failed")
	aborting := &abortingManager{stopErr: stopErr}
	journal := []string{}
	mod := NewEngine(aborting, &fakeManager{name: "second", journal: &journal})
	aborting.mod = mod
	if err := mod.Provide(app.NewRegistry(viper.New())); err != nil {
		t.Fatal(err)
	}
	err := mod.Start()
	if !errors.Is(err, stopErr) || !strings.Contains(err.Error(), "start aborted by shutdown") || !strings.Contains(err.Error(), "rollback") {
		t.Fatalf("Start aborted with a failing rollback = %v", err)
	}
	if len(journal) != 0 {
		t.Fatalf("a manager started after shutdown was requested: %v", journal)
	}
}
