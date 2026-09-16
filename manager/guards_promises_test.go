package manager

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
)

type stopFailingManager struct {
	name string
	err  error
}

func (m *stopFailingManager) Name() string                          { return m.name }
func (m *stopFailingManager) Start(*app.Registry) error             { return nil }
func (m *stopFailingManager) Stop()                                 {}
func (m *stopFailingManager) StopWithContext(context.Context) error { return m.err }

// U-0150 · C2 · gap map kit `manager` 4/10：Provide 拒绝 nil 注册表；自依赖是环；Start 失败回滚时若已启动
// 的管理器停止也失败，两个错误都要报出来。`engine.go（原 kit manager_mod.go:131）`（Start 途中被 Stop 抢先且回滚失败）需要
// 并发编排，留待。
func TestEngineReportsRollbackFailuresAndRefusesCycles(t *testing.T) {
	if err := NewEngine().Provide(nil); err == nil || !strings.Contains(err.Error(), "registry is nil") {
		t.Fatalf("Provide(nil) = %v", err)
	}
	if _, err := Order([]app.IManager{orderManager{name: "a", dependsOn: []string{"a"}}}); err == nil || !strings.Contains(err.Error(), `dependency cycle at "a"`) {
		t.Fatalf("self-dependency = %v", err)
	}
	stopErr := errors.New("flush failed")
	startErr := errors.New("bind failed")
	journal := []string{}
	mod := NewEngine(&stopFailingManager{name: "first", err: stopErr}, &fakeManager{name: "second", startErr: startErr, journal: &journal})
	if err := mod.Provide(app.NewRegistry(viper.New())); err != nil {
		t.Fatal(err)
	}
	err := mod.Start()
	if !errors.Is(err, startErr) || !errors.Is(err, stopErr) || !strings.Contains(err.Error(), "rollback") {
		t.Fatalf("Start with a failing rollback = %v, want both the start and the stop error", err)
	}
}
