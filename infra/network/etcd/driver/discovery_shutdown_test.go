package driver

import (
	"context"
	"errors"
	"fmt"
	fetcd "github.com/tjbdwanghaibo/roost-core/infra/network/etcd"
	"sync"
	"testing"
	"time"

	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestDiscoverySuppressesLeaseLostWarningAfterDeregister(t *testing.T) {
	d := NewDiscovery(nil, "/roost/", 5)
	d.key = "/roost/game/1"
	d.markStopping()

	if d.shouldWarnLeaseLost() {
		t.Fatalf("expected graceful deregister to suppress lease lost warning")
	}
}

func TestDiscoveryWarnsWhenLeaseLostUnexpectedly(t *testing.T) {
	d := NewDiscovery(nil, "/roost/", 5)
	d.key = "/roost/game/1"

	if !d.shouldWarnLeaseLost() {
		t.Fatalf("expected unexpected lease loss to warn")
	}
}

func TestDiscoveryReregistersAfterUnexpectedLeaseLoss(t *testing.T) {
	d := NewDiscovery(nil, "/roost/", 5)
	d.retryMinInterval = time.Millisecond
	d.retryMaxInterval = time.Millisecond

	var mu sync.Mutex
	attempts := 0
	firstLost := make(chan struct{})
	secondLost := make(chan struct{})
	registeredAgain := make(chan struct{})
	d.registerOnce = func(context.Context, *fetcd.ServiceInfo) (discoveryRegistration, error) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		switch attempts {
		case 1:
			return discoveryRegistration{leaseID: clientv3.LeaseID(101), key: "/roost/game/1", keepaliveDone: firstLost}, nil
		case 2:
			close(registeredAgain)
			return discoveryRegistration{leaseID: clientv3.LeaseID(102), key: "/roost/game/1", keepaliveDone: secondLost}, nil
		default:
			return discoveryRegistration{}, errors.New("unexpected extra registration")
		}
	}

	if err := d.Register(context.Background(), &fetcd.ServiceInfo{ServiceType: "game", Sid: 1}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	close(firstLost)

	select {
	case <-registeredAgain:
	case <-time.After(15 * time.Second):
		t.Fatal("expected Discovery to register again after lease loss")
	}

	if got := d.currentLeaseID(); got != clientv3.LeaseID(102) {
		t.Fatalf("lease id = %d, want 102", got)
	}
	close(secondLost)
	_ = d.Deregister(context.Background())
}

func TestDiscoveryDoesNotReregisterAfterDeregister(t *testing.T) {
	d := NewDiscovery(nil, "/roost/", 5)
	d.retryMinInterval = time.Millisecond
	d.retryMaxInterval = time.Millisecond

	lost := make(chan struct{})
	calls := make(chan struct{}, 2)
	d.registerOnce = func(context.Context, *fetcd.ServiceInfo) (discoveryRegistration, error) {
		calls <- struct{}{}
		return discoveryRegistration{leaseID: clientv3.LeaseID(201), key: "/roost/game/1", keepaliveDone: lost}, nil
	}

	if err := d.Register(context.Background(), &fetcd.ServiceInfo{ServiceType: "game", Sid: 1}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_ = d.Deregister(context.Background())
	close(lost)

	select {
	case <-calls:
		// initial registration
	default:
		t.Fatal("expected initial registration call")
	}
	select {
	case <-calls:
		t.Fatal("did not expect registration retry after deregister")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestDiscoveryRejectsDuplicateRegisterWithoutLeakingFirstRegistration(t *testing.T) {
	d := NewDiscovery(nil, "/roost/", 5)
	keepaliveDone := make(chan struct{})
	calls := 0
	d.registerOnce = func(context.Context, *fetcd.ServiceInfo) (discoveryRegistration, error) {
		calls++
		return discoveryRegistration{
			leaseID:       clientv3.LeaseID(301),
			key:           "/roost/game/1",
			keepaliveDone: keepaliveDone,
			cancel:        func() {},
		}, nil
	}
	info := &fetcd.ServiceInfo{ServiceType: "game", Sid: 1}
	if err := d.Register(context.Background(), info); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := d.Register(context.Background(), info); err == nil {
		t.Fatal("duplicate Register must be rejected")
	}
	if calls != 1 {
		t.Fatalf("register attempts=%d, want 1", calls)
	}
	if err := d.Deregister(context.Background()); err != nil {
		t.Fatalf("Deregister: %v", err)
	}
}

func TestServiceWatcherCloseCancelsUnderlyingWatchAndWaitsForLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	watch := make(clientv3.WatchChan)
	sw := newServiceWatcher(ctx, watch, cancel)
	if err := sw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("Close did not cancel the context used by the etcd watch")
	}
	select {
	case _, ok := <-sw.EventChan():
		if ok {
			t.Fatal("event channel remained open after Close")
		}
	default:
		t.Fatal("Close returned before the watcher loop stopped")
	}
	if err := sw.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// leaseLostDiscovery 注册一次后让 keepalive 结束（模拟进程暂停超过 lease_ttl 后恢复：etcd 已让
// 租约过期），等注册循环记下 lease lost 并开始重注册才返回；重注册一直失败，于是停机时
// Deregister 拿到的仍是那个已过期的租约——第 5 笔演练里 P1 恢复后 156ms 内就停机，正是这个状态。
func leaseLostDiscovery(t *testing.T, revoke func(context.Context, clientv3.LeaseID) error) *Discovery {
	t.Helper()
	d := NewDiscovery(nil, "/roost/", 10)
	d.SetRetryIntervals(time.Millisecond, 20*time.Millisecond)
	lost := make(chan struct{})
	retrying := make(chan struct{})
	var retryOnce sync.Once
	attempts := 0
	d.registerOnce = func(context.Context, *fetcd.ServiceInfo) (discoveryRegistration, error) {
		attempts++
		if attempts == 1 {
			return discoveryRegistration{leaseID: clientv3.LeaseID(401), key: "/roost/game/1", keepaliveDone: lost, cancel: func() {}}, nil
		}
		retryOnce.Do(func() { close(retrying) })
		return discoveryRegistration{}, errors.New("etcd unavailable")
	}
	d.revokeLease = revoke
	if err := d.Register(context.Background(), &fetcd.ServiceInfo{ServiceType: "game", Sid: 1}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	close(lost)
	select {
	case <-retrying:
	case <-time.After(10 * time.Second):
		t.Fatal("registration loop did not start re-registering after the lease was lost")
	}
	return d
}

// 第 5 笔演练偏差 3：租约已过期时 Revoke 返回 “requested lease not found”。租约不存在，挂在它上面的
// 键也已被 etcd 删除，注销的目标已经达成，Deregister 必须返回 nil，不能记成 Mod 停机失败。
// clientv3 返回的是 rpctypes.ErrLeaseNotFound；也覆盖未经转换的 gRPC 状态与被包裹的形式。
func TestDiscoveryDeregisterTreatsLeaseNotFoundAsDeregistered(t *testing.T) {
	for name, notFound := range map[string]error{
		"clientv3":   rpctypes.ErrLeaseNotFound,
		"grpcStatus": rpctypes.ErrGRPCLeaseNotFound,
		"wrapped":    fmt.Errorf("revoke: %w", rpctypes.ErrLeaseNotFound),
		"wrappedRPC": fmt.Errorf("revoke: %w", rpctypes.ErrGRPCLeaseNotFound),
	} {
		t.Run(name, func(t *testing.T) {
			var revoked []clientv3.LeaseID
			d := leaseLostDiscovery(t, func(_ context.Context, id clientv3.LeaseID) error {
				revoked = append(revoked, id)
				return notFound
			})
			if err := d.Deregister(context.Background()); err != nil {
				t.Fatalf("Deregister after the lease expired = %v, want nil", err)
			}
			if len(revoked) != 1 || revoked[0] != clientv3.LeaseID(401) {
				t.Fatalf("revoked leases = %v, want [401]", revoked)
			}
			if id, key := d.currentLeaseID(), d.currentKey(); id != 0 || key != "" {
				t.Fatalf("registration after Deregister = (%d, %q), want cleared", id, key)
			}
		})
	}
}

// Assembly.Close 是 kit EtcdMod.StopWithContext 唯一的错误来源：租约已不存在时整个停机返回 nil。
func TestAssemblyCloseTreatsLeaseNotFoundAsDeregistered(t *testing.T) {
	d := leaseLostDiscovery(t, func(context.Context, clientv3.LeaseID) error { return rpctypes.ErrLeaseNotFound })
	if err := (&Assembly{Discovery: d}).Close(context.Background()); err != nil {
		t.Fatalf("Assembly.Close after the lease expired = %v, want nil", err)
	}
}

// 其他 Revoke 错误照旧报告并保留 errors.Is 语义；登记保留，之后的 Deregister 还能再撤销同一个租约。
func TestDiscoveryDeregisterStillReportsOtherRevokeErrors(t *testing.T) {
	fail := true
	d := leaseLostDiscovery(t, func(context.Context, clientv3.LeaseID) error {
		if fail {
			return rpctypes.ErrTimeout
		}
		return nil
	})
	err := d.Deregister(context.Background())
	if !errors.Is(err, rpctypes.ErrTimeout) {
		t.Fatalf("Deregister = %v, want ErrTimeout", err)
	}
	if got := d.currentLeaseID(); got != clientv3.LeaseID(401) {
		t.Fatalf("lease after a failed revoke = %d, want 401 kept for retry", got)
	}
	fail = false
	if err := d.Deregister(context.Background()); err != nil {
		t.Fatalf("second Deregister = %v, want nil", err)
	}
}

// lease lost 之后重注册与停机并发：重注册在 Deregister 取消旧 keepalive 之后才完成并装上新登记。
// Deregister 必须撤销新租约，并取消新租约的 keepalive（否则 Revoke 失败时它会一直续期到连接关闭）。
func TestDiscoveryDeregisterStopsRegistrationThatCompletedDuringShutdown(t *testing.T) {
	d := NewDiscovery(nil, "/roost/", 10)
	d.SetRetryIntervals(time.Millisecond, time.Millisecond)
	lost := make(chan struct{})
	oldKeepaliveCancelled := make(chan struct{})
	retrying := make(chan struct{})
	newKeepaliveCancelled := make(chan struct{})
	var cancelOldOnce, cancelNewOnce sync.Once
	attempts := 0
	d.registerOnce = func(ctx context.Context, _ *fetcd.ServiceInfo) (discoveryRegistration, error) {
		attempts++
		if attempts == 1 {
			return discoveryRegistration{leaseID: 501, key: "/roost/game/1", keepaliveDone: lost,
				cancel: func() { cancelOldOnce.Do(func() { close(oldKeepaliveCancelled) }) }}, nil
		}
		close(retrying)
		<-oldKeepaliveCancelled // Deregister 已执行 cancelKeepalive
		return discoveryRegistration{leaseID: 502, key: "/roost/game/1", keepaliveDone: make(chan struct{}),
			cancel: func() { cancelNewOnce.Do(func() { close(newKeepaliveCancelled) }) }}, nil
	}
	var revoked []clientv3.LeaseID
	d.revokeLease = func(_ context.Context, id clientv3.LeaseID) error {
		revoked = append(revoked, id)
		return nil
	}
	if err := d.Register(context.Background(), &fetcd.ServiceInfo{ServiceType: "game", Sid: 1}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	close(lost)
	<-retrying
	if err := d.Deregister(context.Background()); err != nil {
		t.Fatalf("Deregister: %v", err)
	}
	if len(revoked) != 1 || revoked[0] != 502 {
		t.Fatalf("revoked leases = %v, want [502] (the current registration)", revoked)
	}
	select {
	case <-newKeepaliveCancelled:
	default:
		t.Fatal("keepalive of the registration that completed during shutdown was not cancelled")
	}
}
