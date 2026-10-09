package gate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/network/etcd"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

type discoveryRecorder struct {
	etcd.IDiscovery
	infos                      []*etcd.ServiceInfo
	registered                 *etcd.ServiceInfo
	registerErr, deregisterErr error
	removes                    int
}

func (d *discoveryRecorder) Discover(context.Context, string) ([]*etcd.ServiceInfo, error) {
	return d.infos, nil
}
func (d *discoveryRecorder) Register(_ context.Context, info *etcd.ServiceInfo) error {
	if d.registerErr != nil {
		return d.registerErr
	}
	d.registered = info
	return nil
}
func (d *discoveryRecorder) Deregister(context.Context) error { d.removes++; return d.deregisterErr }

type flushRecorder struct {
	fnats.RawClient
	flushed bool
}

func (c *flushRecorder) FlushContext(context.Context) error { c.flushed = true; return nil }
func TestDiscoveryFixedRouteAndRegistrationOwnership(t *testing.T) {
	id := gateway.ProcessIdentity{ServerID: 22, Incarnation: "game000000000001"}
	d := &discoveryRecorder{infos: []*etcd.ServiceInfo{{Sid: 23, Metadata: map[string]string{IngressReadyMetadata: "true", IncarnationMetadata: id.Incarnation}}, {Sid: 22, Metadata: map[string]string{IngressReadyMetadata: "false", IncarnationMetadata: id.Incarnation}}}}
	resolve := FixedGameResolver(d, "game")
	if _, err := resolve(context.Background(), 22); !errors.Is(err, gateway.ErrTransportUnavailable) {
		t.Fatalf("wrong SID fallback=%v", err)
	}
	d.infos = append(d.infos, &etcd.ServiceInfo{Sid: 22, Metadata: map[string]string{IngressReadyMetadata: "true", IncarnationMetadata: id.Incarnation}})
	if actual, err := resolve(context.Background(), 22); err != nil || actual != id {
		t.Fatalf("identity=%+v err=%v", actual, err)
	}
	d.infos = append(d.infos, &etcd.ServiceInfo{Sid: 22, Metadata: map[string]string{IngressReadyMetadata: "true", IncarnationMetadata: "game000000000002"}})
	if _, err := resolve(context.Background(), 22); err == nil {
		t.Fatal("guessed among conflicting incarnations")
	}
	raw := &flushRecorder{}
	deps := connections{identity: id, client: raw, discovery: d}
	if err := deps.register("game", "", time.Second); err != nil {
		t.Fatal(err)
	}
	if !raw.flushed || d.registered.Sid != 22 || d.registered.Metadata[IncarnationMetadata] != id.Incarnation || !deps.registered {
		t.Fatal("registered before role inbox ready or identity missing")
	}
	d.deregisterErr = context.DeadlineExceeded
	if err := deps.deregister(context.Background()); !errors.Is(err, context.DeadlineExceeded) || !deps.registered {
		t.Fatalf("failed deregister released ownership: %v", err)
	}
	d.deregisterErr = nil
	if err := deps.deregister(context.Background()); err != nil || deps.registered {
		t.Fatalf("retry deregister=%v", err)
	}
	if err := deps.deregister(context.Background()); err != nil || d.removes != 2 {
		t.Fatalf("duplicate removal=%d err=%v", d.removes, err)
	}
	d.registerErr = errors.New("already registered")
	if err := deps.register("game", "", time.Second); err == nil || deps.registered {
		t.Fatal("took ownership of another registration")
	}
}
