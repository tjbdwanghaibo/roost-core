//go:build integration

// RR-20261004-08（W-2026-10-04-02）：真实 NATS 上，连接 drain 超出停止预算时
// Assembly.Close 已经硬关闭连接并返回 ctx 错误；NatsMod 旧实现仍保留 m.asm，
// 重试时 Client.DrainWithContext 对已关闭的连接拿到 ErrConnectionClosed，
// 重试永远失败、引用永不置空。承诺：资源已经释放时停止要能收敛——之后的
// 停止返回 nil 且 m.asm 置空。
package nats

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	natsdriver "github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
)

func TestRealNatsModStopAfterConnectionDrainBudgetConverges(t *testing.T) {
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Skip("ROOST_DATAENGINE_IT_NATS_URL is not set; run through scripts/integration/dataengine-env.sh")
	}
	asm, err := natsdriver.Assemble(fnats.DefaultConfig(url), natsdriver.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		releaseHandler()
		asm.Client.Close()
	})

	// 不经过 Bus 的直连订阅：它的 handler 阻塞时，连接 drain 只能等它或被预算截断。
	subject := fmt.Sprintf("roost.it.rr20261004_08.%d", time.Now().UnixNano())
	if _, err := asm.Client.Subscribe(subject, func(*fnats.Msg) {
		enterOnce.Do(func() { close(entered) })
		<-release
	}); err != nil {
		t.Fatal(err)
	}
	// 两条：第一条占住 handler，第二条留在订阅的待处理队列里，drain 必须等它。
	for i := 0; i < 2; i++ {
		if err := asm.Client.Publish(subject, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("subscription handler never entered; not a counterexample")
	}

	m := &NatsMod{asm: asm}
	budget, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	first := stopModWithin(t, m, budget, "budgeted stop")
	t.Logf("first_stop_err=%v asm_retained=%v connection_up=%v", first, m.asm != nil, asm.Connected())
	if !errors.Is(first, context.DeadlineExceeded) {
		t.Fatalf("a stop whose connection drain ran out of budget must report it: %v", first)
	}
	if asm.Connected() {
		t.Fatal("connection still up after the drain budget ran out; not the hard-close path")
	}

	releaseHandler()
	retry := stopModWithin(t, m, context.Background(), "retry stop")
	t.Logf("retry_err=%v asm_retained=%v", retry, m.asm != nil)
	if retry != nil || m.asm != nil {
		t.Fatal("stop after the connection was already closed hard never converges")
	}
	// 修后契约：硬关闭是可识别的终态，第一次停止就释放引用。
	if !errors.Is(first, natsdriver.ErrClosedUndrained) {
		t.Fatalf("hard close after the drain budget is not reported as terminal: %v", first)
	}
}
