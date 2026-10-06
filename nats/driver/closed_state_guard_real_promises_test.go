//go:build integration

package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/nats"
)

// REFACTOR-2026-10-06-nats-driver-closed-state 的真实 NATS 验收：排空被预算截断、硬关之后，nats.go 的
// 排空协程会把状态翻回 DRAINING_PUBS、最长约 5s 内 IsConnected 为 true（RR-20261006-26）。就在这段
// 窗口里把守卫表整张跑一遍：每个导出方法照样按驱动自己的已关闭状态回答（fnats.ErrClosed / nil / false），
// 与 nats.go 此刻报什么无关。需要 ROOST_DATAENGINE_IT_NATS_URL（scripts/mirror-local.sh 的私有环境）。
func TestRealNatsEveryExportedMethodAnswersFromTheDriverStateAfterAnUndrainedClose(t *testing.T) {
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Skip("ROOST_DATAENGINE_IT_NATS_URL is not set; run against a private environment (scripts/mirror-local.sh)")
	}
	a, err := Assemble(fnats.DefaultConfig(url), ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Client.Close)
	js, err := NewJetStreamClient(a.Client)
	if err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("natsstate.%d", time.Now().UnixNano())
	sub, err := a.Client.Subscribe(prefix+".idle", closedStateHandler)
	if err != nil {
		t.Fatal(err)
	}
	// 卡住的直连订阅：排空只能等它或被预算截断。
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseHandler)
	if _, err := a.Client.Subscribe(prefix+".stuck", func(*fnats.Msg) {
		enterOnce.Do(func() { close(entered) })
		<-release
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := a.Client.Publish(prefix+".stuck", []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("subscription handler never entered; not a real-dependency run")
	}
	budget, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := a.Close(budget); !errors.Is(err, ErrClosedUndrained) {
		t.Fatalf("budgeted Assembly.Close = %v, want ErrClosedUndrained", err)
	}
	// 等 nats.go 的排空协程把状态翻回 DRAINING_PUBS（RR-20261006-26 实测在硬关后约 10ms 内）。没等到就不是
	// 这条用例要的场景，按失败处理，不在别的状态上“通过”。
	flipped := false
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if a.Client.conn.IsDraining() && a.Client.conn.IsConnected() {
			flipped = true
			break
		}
	}
	if !flipped {
		t.Fatal("nats.go never flipped back to DRAINING_PUBS after the hard close; the window this test targets did not occur")
	}

	ctx, cancelCalls := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelCalls()
	started := time.Now()
	env := &closedStateEnv{ctx: ctx, cancel: cancelCalls, asm: a, js: js, sub: sub.(*subscription)}
	checkEveryMethodAfterClose(t, env, "an undrained Assembly.Close", func() {
		// 非关闭类方法全部答完时 nats.go 仍在翻回的状态里，才说明它们是在这段窗口里按驱动状态回答的。
		if !a.Client.conn.IsConnected() {
			t.Errorf("nats.go left the flipped DRAINING_PUBS state before the table finished (%s); the window was not covered", time.Since(started))
		}
		t.Logf("non-closing methods answered in %s while nats.go still reports IsConnected = true", time.Since(started).Round(time.Millisecond))
	})
	releaseHandler()
}
