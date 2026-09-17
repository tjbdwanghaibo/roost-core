package manager

import (
	"context"
	"errors"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
)

// U-0220 · C2 · RR-20260916-07：Start 一旦取走待启动列表，Register 就必须按 ErrRegisterAfterStart 拒绝。
// 旧实现用 "started != nil" 判断启动是否开始，而第一个管理器还在慢 Start 时 started 仍是 nil，
// 晚到的管理器被接纳进 managers 却永远不会启动、也不会被 Stop——通过 Manager(name) 还能看见它。
func TestRegisterDuringTheFirstStartIsRefused(t *testing.T) {
	slow := &gatedManager{name: "slow", entered: make(chan struct{}), release: make(chan struct{})}
	e := NewEngine(slow)
	if err := e.Provide(app.NewRegistry(viper.New())); err != nil {
		t.Fatal(err)
	}
	startDone := make(chan struct{})
	go func() { _ = e.Start(); close(startDone) }()
	waitFor(t, slow.entered)

	late := &gatedManager{name: "late"}
	err := e.Register(late)
	close(slow.release)
	waitFor(t, startDone)
	_ = e.Stop(context.Background())
	if !errors.Is(err, ErrRegisterAfterStart) {
		t.Fatalf("Register during the first Start = %v (late starts=%d stops=%d, managers=%d); want ErrRegisterAfterStart",
			err, late.starts.Load(), late.stops.Load(), len(e.Managers()))
	}
	if _, listed := e.Manager("late"); listed {
		t.Fatal("the refused manager is still listed")
	}
}
