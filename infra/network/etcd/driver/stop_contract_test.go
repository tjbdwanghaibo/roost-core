package driver

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fetcd "github.com/tjbdwanghaibo/roost-core/infra/network/etcd"
	"github.com/tjbdwanghaibo/roost-core/internal/stopcontract"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Discovery.Deregister 套共用停机契约骨架（维护者决定 A3）：卡住的工作是 lease 丢失后不响应取消的重注册；“资源”是当前租约——
// 注册循环退出之后才撤销，撤销后登记清空。
func TestDiscoveryDeregisterStopContract(t *testing.T) {
	d := NewDiscovery(nil, "/roost/", 5)
	d.retryMinInterval, d.retryMaxInterval = time.Millisecond, time.Millisecond
	lost, retrying, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var attempts atomic.Int32
	d.registerOnce = func(context.Context, *fetcd.ServiceInfo) (discoveryRegistration, error) {
		switch attempts.Add(1) {
		case 1:
			return discoveryRegistration{leaseID: 101, key: "/roost/game/1", keepaliveDone: lost, cancel: func() {}}, nil
		case 2:
			close(retrying)
			<-release
			return discoveryRegistration{}, errors.New("setup failed")
		default:
			return discoveryRegistration{}, errors.New("unexpected registration")
		}
	}
	var revokeMu sync.Mutex
	var revoked []clientv3.LeaseID
	d.revokeLease = func(_ context.Context, id clientv3.LeaseID) error {
		revokeMu.Lock()
		defer revokeMu.Unlock()
		revoked = append(revoked, id)
		return nil
	}
	stopcontract.Check(t, stopcontract.Hooks{
		Start: func(testing.TB) {
			if err := d.Register(context.Background(), &fetcd.ServiceInfo{ServiceType: "game", Sid: 1}); err != nil {
				t.Fatal(err)
			}
		},
		Block: func(testing.TB) {
			close(lost)
			select {
			case <-retrying:
			case <-time.After(5 * time.Second):
				t.Fatal("registration loop did not retry after the lease was lost")
			}
		},
		Stop:    d.Deregister,
		Release: func() { close(release) },
		Released: func() bool {
			revokeMu.Lock()
			defer revokeMu.Unlock()
			return slices.Contains(revoked, 101) && !d.hasRegistration()
		},
	})
	revokeMu.Lock()
	defer revokeMu.Unlock()
	if len(revoked) != 1 {
		t.Fatalf("revoked leases = %v, want [101] exactly once", revoked)
	}
}

// Assembly.Close 套同一骨架：卡住的工作是不响应的租约撤销（直到放行）；“资源”是 etcd client——撤销成功之后才关闭。
func TestAssemblyCloseStopContract(t *testing.T) {
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
	release := make(chan struct{})
	d.revokeLease = func(ctx context.Context, _ clientv3.LeaseID) error {
		if err := asm.Client.cli.Ctx().Err(); err != nil {
			return errors.New("grpc: the client connection is closing")
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	stopcontract.Check(t, stopcontract.Hooks{
		Stop:     asm.Close,
		Release:  func() { close(release) },
		Released: func() bool { return asm.Client.cli.Ctx().Err() != nil },
	})
	if d.currentLeaseID() != 0 {
		t.Fatal("lease kept after a successful Close")
	}
}

// 驱动 Close 的统一口径：重复 Close 幂等返回 nil，第一次的错误只报一次；并发 Close 的后到者等第一个做完；Close 之后的其他
// 调用快速返回已关闭错误（fetcd.ErrClosed），不交给 clientv3 的重试拦截器阻塞到调用方截止时间。
//
// 不可达地址、DialTimeout 0（clientv3.New 不拨号），不需要真实 etcd。
func closeContractClient(t *testing.T) *Client {
	t.Helper()
	cfg := fetcd.DefaultConfig([]string{"127.0.0.1:1"})
	cfg.DialTimeout = 0
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClientCloseErrorIsReportedOnceThenNil(t *testing.T) {
	c := closeContractClient(t)
	// 底层客户端先被关过一次：clientv3 第二次 Close 返回 context.Canceled，用它制造“第一次 Close 出错”。
	_ = c.cli.Close()
	if err := c.Close(); err == nil {
		t.Fatal("first Close on a client closed underneath = nil, want the clientv3 error reported once")
	}
	for attempt := 2; attempt <= 3; attempt++ {
		if err := c.Close(); err != nil {
			t.Fatalf("Close #%d = %v, want nil: the first error is reported once", attempt, err)
		}
	}
}

func TestClientConcurrentCloseAllReturnNil(t *testing.T) {
	c := closeContractClient(t)
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) { defer wg.Done(); errs[i] = c.Close() }(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Close #%d = %v, want nil (all = %v)", i, err, errs)
		}
	}
}

func TestClientCallsAfterCloseFailFastWithErrClosed(t *testing.T) {
	c := closeContractClient(t)
	if err := c.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	// 截止时间远大于断言窗口：旧实现会一直阻塞到这里。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	calls := map[string]func() error{
		"Get":              func() error { _, err := c.Get(ctx, "k"); return err },
		"GetWithPrefix":    func() error { _, err := c.GetWithPrefix(ctx, "p/"); return err },
		"Put":              func() error { return c.Put(ctx, "k", "v") },
		"PutWithLease":     func() error { return c.PutWithLease(ctx, "k", "v", 1) },
		"Delete":           func() error { return c.Delete(ctx, "k") },
		"DeleteWithPrefix": func() error { _, err := c.DeleteWithPrefix(ctx, "p/"); return err },
		"Txn": func() error {
			_, err := c.Txn(ctx, fetcd.Cmp{Key: "k", Target: fetcd.CmpVersion, Op: fetcd.CmpEqual, Value: 0}, nil, nil)
			return err
		},
		"Grant":     func() error { _, err := c.Grant(ctx, 5); return err },
		"KeepAlive": func() error { _, err := c.KeepAlive(ctx, 1); return err },
		"Revoke":    func() error { return c.Revoke(ctx, 1) },
	}
	for name, call := range calls {
		done := make(chan error, 1)
		go func() { done <- call() }()
		select {
		case err := <-done:
			if !errors.Is(err, fetcd.ErrClosed) {
				t.Errorf("%s after Close = %v, want fetcd.ErrClosed", name, err)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("%s after Close still blocked after 2s, want an immediate fetcd.ErrClosed", name)
		}
	}
}

// Deregister 三步停机：先取消注册循环与 keepalive（幂等），在调用方 ctx 内等循环退出（重注册可能卡在不受 loop ctx 约束的
// 步骤里），超时返回 ctx 错误并保留登记，重试再等。

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
