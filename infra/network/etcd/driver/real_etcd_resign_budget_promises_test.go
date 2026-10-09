//go:build integration

// RR-20261005-NC-93：真实单节点 etcd 上，Campaign 成功后 SIGSTOP etcd，Resign(500ms)。旧行为：SDK
// Session.Close 的 Revoke 用 session context 加 TTL（60s），Resign 阻塞 20s 以上。承诺：Resign 在预算后
// 返回 ctx 错误、本地领导权结束；etcd 恢复后 election 持有的 Revoke 完成，另一候选能当选；健康 etcd 上
// Resign 返回 nil，lease 已撤销、键已删除。
package driver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// startStoppableEtcd is startEtcd that also returns the process, so a test
// can freeze it with SIGSTOP.
func startStoppableEtcd(t *testing.T) (string, *exec.Cmd) {
	t.Helper()
	binary, err := exec.LookPath("etcd")
	if err != nil {
		t.Skip("etcd binary not on PATH; the real-etcd test is skipped (install etcd to run it)")
	}
	clientURL := fmt.Sprintf("http://127.0.0.1:%d", freePort(t))
	peerURL := fmt.Sprintf("http://127.0.0.1:%d", freePort(t))
	cmd := exec.Command(binary, "--name", "roost-it", "--data-dir", t.TempDir(),
		"--listen-client-urls", clientURL, "--advertise-client-urls", clientURL,
		"--listen-peer-urls", peerURL, "--initial-advertise-peer-urls", peerURL,
		"--initial-cluster", "roost-it="+peerURL, "--log-level", "error")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGCONT)
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	endpoint := clientURL[len("http://"):]
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.DialTimeout("tcp", endpoint, 200*time.Millisecond); err == nil {
			_ = conn.Close()
			return endpoint, cmd
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("etcd did not start listening")
	return "", nil
}

func TestRealEtcdResignHonoursTheCallerBudget(t *testing.T) {
	endpoint, cmd := startStoppableEtcd(t)
	cli := realElectionClient(t, endpoint)
	prefix := "/rr-20261005-nc93/frozen/"
	e := NewElectionFactory(cli).NewElection(prefix)
	if err := e.Campaign(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	started := time.Now()
	go func() { result <- e.Resign(ctx) }()
	var err error
	select {
	case err = <-result:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Signal(syscall.SIGCONT)
		t.Fatal("Resign(500ms) against a frozen etcd still blocked after 5s")
	}
	elapsed := time.Since(started)
	_ = cmd.Process.Signal(syscall.SIGCONT)
	t.Logf("Resign(500ms) against a frozen etcd: err=%v elapsed=%s is_leader=%v", err, elapsed.Round(time.Millisecond), e.IsLeader())
	if !errors.Is(err, context.DeadlineExceeded) || elapsed > 2*time.Second || e.IsLeader() {
		t.Fatal("Resign did not honour its budget, or kept the local leadership")
	}
	// After the thaw the Revoke the election owns (or the TTL) frees the prefix.
	if err := campaignWithin(t, endpoint, prefix, "b", 10*time.Second); err != nil {
		t.Fatalf("another candidate after the frozen Resign: %v", err)
	}
}

func TestRealEtcdResignRevokesItsLease(t *testing.T) {
	endpoint := startEtcd(t)
	cli := realElectionClient(t, endpoint)
	prefix := "/rr-20261005-nc93/healthy/"
	e, lease := recordingElection(cli, prefix, func(b electionBackend) electionBackend { return b })
	if err := e.Campaign(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := e.Resign(ctx)
	ttl, keys := realLeaseTTL(t, cli, *lease), realElectionKeys(t, cli, prefix)
	t.Logf("healthy Resign: err=%v lease_ttl=%d keys=%d", err, ttl, keys)
	if err != nil || ttl != -1 || keys != 0 {
		t.Fatal("Resign on a healthy etcd left its lease or keys behind")
	}
	if err := campaignWithin(t, endpoint, prefix, "b", 2*time.Second); err != nil {
		t.Fatalf("another candidate after Resign: %v", err)
	}
}
