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

// A3 / RR-20261005-NC-173：Discovery.Deregister 套共用停机契约骨架。卡住的工作是 lease 丢失后不响应
// 取消的重注册；“资源”是当前租约——注册循环退出之后才撤销，撤销后登记清空。
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

// A3 / RR-20261005-NC-173：Assembly.Close 套同一骨架。卡住的工作是不响应的租约撤销（直到放行）；
// “资源”是 etcd client——撤销成功之后才关闭。
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
