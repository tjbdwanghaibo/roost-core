//go:build integration

// RR-20261005-NC-173：真实单节点 etcd 上注册后 SIGSTOP etcd，Assembly.Close(500ms)。旧行为：撤销超时后仍关闭
// client，etcd 恢复后用新 ctx 重试 Close，Revoke 在已关闭的连接上必然失败、键留到 TTL。承诺：Close 在预算后返回
// ctx 错误并保留连接与登记；恢复后重试撤销成功、键立即删除，之后才关闭连接。
package driver

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	fetcd "github.com/tjbdwanghaibo/roost-core/infra/network/etcd"
)

func TestRealEtcdAssemblyCloseRetriesTheRevokeAfterAFrozenBudget(t *testing.T) {
	endpoint, cmd := startStoppableEtcd(t)
	cfg := fetcd.DefaultConfig([]string{endpoint})
	cfg.ServicePrefix = "/rr-20261005-nc173/service/"
	cfg.LeaseTTL = 60
	asm, err := Assemble(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = asm.Client.Close() })
	startCtx, cancelStart := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelStart()
	if err := asm.Start(startCtx, &fetcd.ServiceInfo{ServiceType: "game", Sid: 173}); err != nil {
		t.Fatal(err)
	}
	observer := realElectionClient(t, endpoint)
	if keys := realElectionKeys(t, observer, cfg.ServicePrefix); keys != 1 {
		t.Fatalf("registered keys = %d, want 1", keys)
	}

	if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	started := time.Now()
	result := make(chan error, 1)
	go func() { result <- asm.Close(ctx) }()
	select {
	case err = <-result:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Signal(syscall.SIGCONT)
		t.Fatal("Close(500ms) against a frozen etcd still blocked after 5s")
	}
	elapsed := time.Since(started)
	clientOpen := asm.Client.cli.Ctx().Err() == nil
	_ = cmd.Process.Signal(syscall.SIGCONT)
	t.Logf("budgeted Close against a frozen etcd: err=%v elapsed=%s client_open=%v", err, elapsed.Round(time.Millisecond), clientOpen)
	if !errors.Is(err, context.DeadlineExceeded) || elapsed > 2*time.Second {
		t.Fatal("Close did not honour its budget")
	}

	retryCtx, cancelRetry := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRetry()
	retryErr := asm.Close(retryCtx)
	keys := realElectionKeys(t, observer, cfg.ServicePrefix)
	t.Logf("retry Close after the thaw: err=%v keys=%d client_open=%v", retryErr, keys, asm.Client.cli.Ctx().Err() == nil)
	if !clientOpen || retryErr != nil || keys != 0 {
		t.Fatal("the timed-out Close released the client, or the retry could not revoke the lease")
	}
}
