//go:build integration && physx && cgo

package physicsroom

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/chunksync"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
)

func TestRealNATSRoomLifecycle(t *testing.T) {
	url := os.Getenv("ROOST_PHYSICS_TEST_NATS_URL")
	if url == "" {
		t.Skip("private NATS URL not configured")
	}
	a, err := driver.Assemble(fnats.DefaultConfig(url), driver.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Client.Close)
	prefix := fmt.Sprintf("roost.physics.test.n%d", time.Now().UnixNano())
	makeBus := func(sid int32) *bus.Bus {
		b := bus.New(a.Client, a.RPC, nil, bus.Config{Sid: sid, SvcType: ServiceType, Prefix: prefix, WorkerNum: 2, QueueCap: 16})
		if err := b.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := b.StopWithContext(ctx); err != nil {
				t.Error(err)
			}
		})
		return b
	}
	owner, caller := makeBus(7801), makeBus(7802)
	s := newTestService(t, testConfig())
	if err := RegisterHandlers(owner, s); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = a.Client.FlushContext(ctx); err != nil {
		t.Fatal(err)
	}
	client, err := NewOwnerClient(caller, 7801, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := client.Describe(ctx)
	if err != nil || instance != s.instance {
		t.Fatalf("owner identity: %s %v", instance, err)
	}
	ref, err := client.Create(ctx, testSpec("rpc", s))
	if err != nil {
		t.Fatal(err)
	}
	m, err := client.Join(ctx, ref, testBinding(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Submit(ctx, m, Input{Sequence: 1}); err != nil {
		t.Fatal(err)
	}
	d, err := client.Poll(ctx, m, 0)
	if err != nil {
		t.Fatal(err)
	}
	var replica chunksync.Replica
	if err = replica.Apply(d.Payload); err != nil {
		t.Fatal(err)
	}
	if replica.Epoch != ref.Epoch || len(replica.Objects) != 1 {
		t.Fatalf("bad snapshot: %+v", replica)
	}
	if _, err = client.Poll(ctx, m, d.Sequence); err != nil {
		t.Fatal(err)
	}
	if err = client.Leave(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err = client.Destroy(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Poll(ctx, m, d.Sequence); err == nil {
		t.Fatal("destroyed room still served")
	}
}
