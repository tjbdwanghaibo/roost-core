//go:build integration

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

	fetcd "github.com/tjbdwanghaibo/roost-core/infra/network/etcd"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
)

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// startEtcd launches a single-node etcd from PATH on free ports and returns
// its client endpoint. The process is killed with the test.
func startEtcd(t *testing.T) string {
	t.Helper()
	binary, err := exec.LookPath("etcd")
	if err != nil {
		t.Skip("etcd binary not on PATH; the real-etcd guard test is skipped (install etcd to run it)")
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
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	endpoint := clientURL[len("http://"):]
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", endpoint, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return endpoint
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("etcd did not start listening")
	return ""
}

// 对不存在的键 Get 必须是 ErrKeyNotFound，而不是一个空 KV；写入后再读得到值。`*clientv3.Client` 无法替身，在真实单节点 etcd 上验证。
func TestRealEtcdGetOfAMissingKeyIsNotFound(t *testing.T) {
	endpoint := startEtcd(t)
	client, err := NewClient(&fetcd.Config{Endpoints: []string{endpoint}, DialTimeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.Get(ctx, "/roost/it/missing"); !errors.Is(err, fetcd.ErrKeyNotFound) {
		t.Fatalf("Get of a missing key = %v, want ErrKeyNotFound", err)
	}
	if err := client.Put(ctx, "/roost/it/present", "v1"); err != nil {
		t.Fatal(err)
	}
	kv, err := client.Get(ctx, "/roost/it/present")
	if err != nil || kv == nil || kv.Value != "v1" {
		t.Fatalf("Get of a written key = (%+v, %v)", kv, err)
	}
}

// 第 5 笔演练偏差 3 的真机复现：租约在 etcd 一侧已不存在（进程暂停超过 lease_ttl 后恢复，这里用带外
// Revoke 代替等待过期，效果相同：租约与挂在上面的键都没了），停机时 Assembly.Close（kit EtcdMod
// 停机的唯一错误来源）对已不存在的租约 Revoke 得到 “requested lease not found”，必须视为注销已达成。
func TestRealEtcdCloseAfterLeaseVanishedIsClean(t *testing.T) {
	endpoint := startEtcd(t)
	asm, err := Assemble(&fetcd.Config{Endpoints: []string{endpoint}, DialTimeout: 3 * time.Second,
		ServicePrefix: "/roost/it/svc/", LeaseTTL: 5})
	if err != nil {
		t.Fatal(err)
	}
	asm.Discovery.SetRetryIntervals(time.Hour, time.Hour) // 停机前不重注册：Close 面对的是消失的那个租约
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := asm.Start(ctx, &fetcd.ServiceInfo{ServiceType: "game", Sid: 7, Addr: "127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	lease := asm.Discovery.currentLeaseID()
	if err := asm.Client.Revoke(ctx, int64(lease)); err != nil {
		t.Fatalf("out-of-band revoke: %v", err)
	}
	if _, err := asm.Client.Get(ctx, "/roost/it/svc/game/7"); !errors.Is(err, fetcd.ErrKeyNotFound) {
		t.Fatalf("registered key after its lease vanished: %v, want ErrKeyNotFound", err)
	}
	if err := asm.Close(ctx); err != nil {
		t.Fatalf("Close after the lease vanished = %v, want nil", err)
	}
}

// 竞选在 session 建好之后失败或被取消，必须即时撤销这次 session 的 lease，候选键 / 领导键随之删除，其他候选能马上当选
// （撤销不能用已取消的 session ctx，否则 lease 与键留到 TTL，残留领导键期间谁都选不上）。三个场景都走真实单节点 etcd
// （PATH 上的 etcd，没有则跳过）。

// failAfterCampaign runs the real campaign (the Txn writes the candidate key)
// and then reports err, or calls after first, so the cleanup branches that run
// on a live etcd key can be reached deterministically.
type failAfterCampaign struct {
	electionBackend
	after func()
	err   error
}

func (b *failAfterCampaign) Campaign(ctx context.Context, value string) error {
	if err := b.electionBackend.Campaign(ctx, value); err != nil {
		return err
	}
	if b.after != nil {
		b.after()
	}
	return b.err
}

func realElectionClient(t *testing.T, endpoint string) *clientv3.Client {
	t.Helper()
	cli, err := clientv3.New(clientv3.Config{Endpoints: []string{endpoint}, DialTimeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

func realElectionKeys(t *testing.T, cli *clientv3.Client, prefix string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := cli.Get(ctx, prefix, clientv3.WithPrefix(), clientv3.WithCountOnly())
	if err != nil {
		t.Fatal(err)
	}
	return resp.Count
}

func realLeaseTTL(t *testing.T, cli *clientv3.Client, lease clientv3.LeaseID) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := cli.TimeToLive(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	return resp.TTL
}

// recordingElection wraps the real session factory so the test learns the
// lease of the session the campaign created.
func recordingElection(cli *clientv3.Client, prefix string, backend func(electionBackend) electionBackend) (*election, *clientv3.LeaseID) {
	e := &election{cli: cli, prefix: prefix, leaderCh: make(chan struct{})}
	lease := new(clientv3.LeaseID)
	e.create = func(ctx context.Context) (electionSession, electionBackend, error) {
		s, b, err := e.createWithEtcd(ctx)
		if err != nil {
			return nil, nil, err
		}
		*lease = s.(*concurrency.Session).Lease()
		return s, backend(b), nil
	}
	return e, lease
}

func campaignWithin(t *testing.T, endpoint, prefix, value string, budget time.Duration) error {
	t.Helper()
	other := NewElectionFactory(realElectionClient(t, endpoint)).NewElection(prefix)
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	err := other.Campaign(ctx, value)
	if err == nil {
		_ = other.Resign(context.Background())
	}
	return err
}

// (a) elect.Campaign reports a non-context error after its Txn wrote the
// candidate key (e.g. SDK waitDeletes "lost watcher"); the caller is still
// waiting, so the Revoke finishes before Campaign returns.
func TestRealEtcdFailedCampaignRevokesItsLease(t *testing.T) {
	endpoint := startEtcd(t)
	cli := realElectionClient(t, endpoint)
	prefix := "/rr-20261004-06/failed/"
	e, lease := recordingElection(cli, prefix, func(b electionBackend) electionBackend {
		return &failAfterCampaign{electionBackend: b, err: errors.New("lost watcher waiting for delete")}
	})
	err := e.Campaign(context.Background(), "a")
	ttl, keys := realLeaseTTL(t, cli, *lease), realElectionKeys(t, cli, prefix)
	t.Logf("campaign_err=%v lease_ttl_after_failed_campaign=%d candidate_keys=%d", err, ttl, keys)
	if err == nil || e.IsLeader() {
		t.Fatal("failed campaign reported success")
	}
	if ttl != -1 || keys != 0 {
		t.Fatal("failed campaign left its lease and candidate key until TTL")
	}
	if oerr := campaignWithin(t, endpoint, prefix, "b", 2*time.Second); oerr != nil {
		t.Fatalf("another candidate could not be elected after the failure: %v", oerr)
	}
}

// (b) The real campaign wins, then the caller cancels before leadership is
// published: Campaign reports cancellation, so the leader key it wrote must
// not keep blocking everyone else.
func TestRealEtcdCanceledWinningCampaignLeavesNoPhantomLeader(t *testing.T) {
	endpoint := startEtcd(t)
	cli := realElectionClient(t, endpoint)
	prefix := "/rr-20261004-06/race/"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e, lease := recordingElection(cli, prefix, func(b electionBackend) electionBackend {
		return &failAfterCampaign{electionBackend: b, after: cancel}
	})
	err := e.Campaign(ctx, "a")
	if !errors.Is(err, context.Canceled) || e.IsLeader() {
		t.Fatalf("canceled winning campaign: err=%v is_leader=%v", err, e.IsLeader())
	}
	oerr := campaignWithin(t, endpoint, prefix, "b", 3*time.Second)
	ttl := realLeaseTTL(t, cli, *lease)
	t.Logf("first_campaign_err=%v other_campaign_err=%v first_lease_ttl=%d", err, oerr, ttl)
	if oerr != nil {
		t.Fatal("first candidate reported failure but its leader key still blocks every other candidate")
	}
	if ttl != -1 {
		t.Fatal("canceled winning campaign left its lease until TTL")
	}
}

// (d) The caller cancels while waiting behind another leader. The SDK deletes
// the candidate key itself, but the lease must be revoked too; otherwise every
// timed-out retry of a standby leaves one more orphan lease for a full TTL.
func TestRealEtcdCanceledWaitingCampaignRevokesItsLease(t *testing.T) {
	endpoint := startEtcd(t)
	cli := realElectionClient(t, endpoint)
	prefix := "/rr-20261004-06/waiting/"
	leader := NewElectionFactory(realElectionClient(t, endpoint)).NewElection(prefix)
	if err := leader.Campaign(context.Background(), "leader"); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = leader.Resign(context.Background()) }()
	e, lease := recordingElection(cli, prefix, func(b electionBackend) electionBackend { return b })
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := e.Campaign(ctx, "standby")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting campaign: %v", err)
	}
	// The caller is gone, so the Revoke runs after Campaign returned; give it
	// a bounded window well below the 60s TTL.
	deadline := time.Now().Add(2 * time.Second)
	ttl := realLeaseTTL(t, cli, *lease)
	for ttl != -1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		ttl = realLeaseTTL(t, cli, *lease)
	}
	keys := realElectionKeys(t, cli, prefix)
	t.Logf("campaign_err=%v standby_lease_ttl=%d keys=%d", err, ttl, keys)
	if ttl != -1 || keys != 1 {
		t.Fatal("canceled waiting campaign left an orphan lease until TTL")
	}
}

// 真实单节点 etcd 上 Campaign 成功后 SIGSTOP etcd，Resign(500ms)：Resign 在预算后返回 ctx 错误、本地领导权结束；etcd 恢复后
// election 持有的 Revoke 完成，另一候选能当选；健康 etcd 上 Resign 返回 nil，lease 已撤销、键已删除。

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

// 真实单节点 etcd 上注册后 SIGSTOP etcd，Assembly.Close(500ms)：Close 在预算后返回 ctx 错误并保留连接与登记；恢复后重试撤销
// 成功、键立即删除，之后才关闭连接。

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
