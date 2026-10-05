package driver

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fetcd "github.com/tjbdwanghaibo/roost-core/etcd"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// RR-20261005-NC-173：Deregister 等注册循环退出时不看调用方 ctx（裸 <-done）；重注册卡在不受 loop ctx
// 约束的步骤里（setup 失败后的 revokeSetupLease 自带 5s 截止）时，停机越过预算。三步停机：先取消循环与
// keepalive（幂等），在 ctx 内等循环退出，超时返回 ctx 错误并保留登记，重试再等。

func TestDiscoveryDeregisterWaitsForTheRegistrationLoopWithinItsContext(t *testing.T) {
	d := NewDiscovery(nil, "/roost/", 5)
	d.retryMinInterval, d.retryMaxInterval = time.Millisecond, time.Millisecond
	lost := make(chan struct{})
	retrying, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var attempts atomic.Int32
	d.registerOnce = func(context.Context, *fetcd.ServiceInfo) (discoveryRegistration, error) {
		switch attempts.Add(1) {
		case 1:
			return discoveryRegistration{leaseID: 101, key: "/roost/game/1", keepaliveDone: lost, cancel: func() {}}, nil
		case 2:
			// 重注册中途不响应 loop 的取消（例如 Put 失败后带自身截止的 revokeSetupLease）。
			close(retrying)
			<-release
			return discoveryRegistration{}, errors.New("setup failed")
		default:
			return discoveryRegistration{}, errors.New("unexpected registration")
		}
	}
	var revoked []clientv3.LeaseID
	var revokeMu sync.Mutex
	d.revokeLease = func(_ context.Context, id clientv3.LeaseID) error {
		revokeMu.Lock()
		revoked = append(revoked, id)
		revokeMu.Unlock()
		return nil
	}
	if err := d.Register(context.Background(), &fetcd.ServiceInfo{ServiceType: "game", Sid: 1}); err != nil {
		t.Fatal(err)
	}
	close(lost)
	select {
	case <-retrying:
	case <-time.After(5 * time.Second):
		t.Fatal("registration loop did not retry after the lease was lost")
	}

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	first := make(chan error, 1)
	go func() { first <- d.Deregister(expired) }()
	select {
	case err := <-first:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Deregister = %v, want context.Canceled while the loop is still running", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Deregister ignored its context while waiting for the registration loop")
	}

	releaseOnce.Do(func() { close(release) })
	if err := d.Deregister(context.Background()); err != nil {
		t.Fatalf("retry Deregister = %v", err)
	}
	revokeMu.Lock()
	defer revokeMu.Unlock()
	if len(revoked) != 1 || revoked[0] != 101 {
		t.Fatalf("revoked leases = %v, want [101] exactly once", revoked)
	}
	if d.currentLeaseID() != 0 || d.hasRegistration() {
		t.Fatal("registration kept after a successful Deregister")
	}
}

// Assembly.Close 在 Deregister 失败（预算内没撤销租约）时仍关闭 client，重试的 Revoke 必然失败。
func TestAssemblyCloseKeepsTheClientUntilDeregisterSucceeds(t *testing.T) {
	cfg := fetcd.DefaultConfig([]string{"127.0.0.1:1"})
	cfg.DialTimeout = 200 * time.Millisecond
	asm, err := Assemble(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = asm.Client.Close() })
	d := asm.Discovery
	d.mu.Lock()
	d.leaseID, d.key = 7, "/roost/game/1"
	d.mu.Unlock()
	var calls atomic.Int32
	d.revokeLease = func(ctx context.Context, _ clientv3.LeaseID) error {
		if err := asm.Client.cli.Ctx().Err(); err != nil {
			return errors.New("grpc: the client connection is closing")
		}
		if calls.Add(1) == 1 {
			<-ctx.Done() // etcd 无响应：撤销耗尽本次预算
			return ctx.Err()
		}
		return nil
	}
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := asm.Close(expired); !errors.Is(err, context.Canceled) {
		t.Fatalf("first Close = %v, want context.Canceled", err)
	}
	if asm.Client.cli.Ctx().Err() != nil {
		t.Error("Close closed the etcd client although the lease was not revoked")
	}
	if err := asm.Close(context.Background()); err != nil {
		t.Fatalf("retry Close = %v (calls=%d)", err, calls.Load())
	}
	if asm.Client.cli.Ctx().Err() == nil {
		t.Fatal("client still open after a successful Close")
	}
	if d.currentLeaseID() != 0 {
		t.Fatal("lease kept after a successful Close")
	}
}
