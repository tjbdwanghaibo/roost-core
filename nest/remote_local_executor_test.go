package nest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/fctx"
)

// bindingRemoteManager 记录 NewEngine 交给 Remote Manager 的本地执行入口（LocalExecutorBinder）。
type bindingRemoteManager struct {
	stagedRemoteManager
	mu  sync.Mutex
	run func(func()) error
}

func (m *bindingRemoteManager) BindLocalExecutor(run func(func()) error) {
	m.mu.Lock()
	m.run = run
	m.mu.Unlock()
}

func (m *bindingRemoteManager) localExecutor() func(func()) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.run
}

// RR-20260926-37/39：Remote 后台收尾不属于任何 Nest 消息。NewEngine 把与 DataEngine 驱逐（RR-30）同一个 RunLocal
// 交给 Remote Manager：从非快 worker 调用时在快池执行并等待，panic 作为错误返回；停机后拒绝投递、不执行 fn。
func TestNestBindsRunLocalIntoRemoteManager(t *testing.T) {
	manager := &bindingRemoteManager{}
	mgr := NewEngine(NestOptionWithGetter(newMockGetter()), NestOptionWithRemoteEntityManager(manager), NestOptionWithWorkerNumAndMsgCap(1, 1, 16))
	run := manager.localExecutor()
	if run == nil {
		t.Fatal("NewEngine did not bind its RunLocal into the remote manager")
	}
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	onFast := make(chan bool, 1)
	done := make(chan error, 1)
	go func() { done <- run(func() { onFast <- fctx.InFastWorker() }) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("local step did not run")
	}
	if !<-onFast {
		t.Fatal("local step did not run on a fast worker")
	}
	if err := run(func() { panic(errors.New("boom")) }); err == nil {
		t.Fatal("panic in a local step was not reported")
	}
	if err := mgr.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	ran := false
	if err := run(func() { ran = true }); !errors.Is(err, ErrNestStopped) || ran {
		t.Fatalf("local step after shutdown: err=%v ran=%v, want ErrNestStopped without running", err, ran)
	}
}
