package etcd

import (
	"context"
	fetcd "github.com/tjbdwanghaibo/cube-core/etcd"
	"errors"
	"sync"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestDiscoverySuppressesLeaseLostWarningAfterDeregister(t *testing.T) {
	d := newDiscovery(nil, "/cube/", 5)
	d.key = "/cube/game/1"
	d.markStopping()

	if d.shouldWarnLeaseLost() {
		t.Fatalf("expected graceful deregister to suppress lease lost warning")
	}
}

func TestDiscoveryWarnsWhenLeaseLostUnexpectedly(t *testing.T) {
	d := newDiscovery(nil, "/cube/", 5)
	d.key = "/cube/game/1"

	if !d.shouldWarnLeaseLost() {
		t.Fatalf("expected unexpected lease loss to warn")
	}
}

func TestDiscoveryReregistersAfterUnexpectedLeaseLoss(t *testing.T) {
	d := newDiscovery(nil, "/cube/", 5)
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
			return discoveryRegistration{leaseID: clientv3.LeaseID(101), key: "/cube/game/1", keepaliveDone: firstLost}, nil
		case 2:
			close(registeredAgain)
			return discoveryRegistration{leaseID: clientv3.LeaseID(102), key: "/cube/game/1", keepaliveDone: secondLost}, nil
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
	case <-time.After(time.Second):
		t.Fatal("expected discovery to register again after lease loss")
	}

	if got := d.currentLeaseID(); got != clientv3.LeaseID(102) {
		t.Fatalf("lease id = %d, want 102", got)
	}
	close(secondLost)
	_ = d.Deregister(context.Background())
}

func TestDiscoveryDoesNotReregisterAfterDeregister(t *testing.T) {
	d := newDiscovery(nil, "/cube/", 5)
	d.retryMinInterval = time.Millisecond
	d.retryMaxInterval = time.Millisecond

	lost := make(chan struct{})
	calls := make(chan struct{}, 2)
	d.registerOnce = func(context.Context, *fetcd.ServiceInfo) (discoveryRegistration, error) {
		calls <- struct{}{}
		return discoveryRegistration{leaseID: clientv3.LeaseID(201), key: "/cube/game/1", keepaliveDone: lost}, nil
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
