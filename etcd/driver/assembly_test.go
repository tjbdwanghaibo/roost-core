package driver

import (
	"context"
	"testing"
	"time"

	fetcd "github.com/tjbdwanghaibo/roost-core/etcd"
)

// P3b: Assemble is what the Mod publishes. It must refuse a configuration
// without endpoints, build discovery and elections on the client's connection,
// carry the configured retry intervals into discovery, and make Ping / Start
// report an unreachable cluster instead of hanging or succeeding vacuously.
func TestAssembleBuildsDiscoveryAndElectionsOnOneConnection(t *testing.T) {
	if _, err := Assemble(nil); err == nil {
		t.Fatal("nil configuration was assembled")
	}
	if _, err := Assemble(&fetcd.Config{}); err == nil {
		t.Fatal("configuration without endpoints was assembled")
	}
	cfg := fetcd.DefaultConfig([]string{"127.0.0.1:1"})
	cfg.DialTimeout = 200 * time.Millisecond
	cfg.RegisterRetryMinInterval, cfg.RegisterRetryMaxInterval = 3*time.Second, 9*time.Second
	asm, err := Assemble(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = asm.Close(context.Background()) }()
	if asm.Client == nil || asm.Discovery == nil || asm.Election == nil {
		t.Fatalf("assembly incomplete: %+v", asm)
	}
	if asm.Discovery.cli != asm.Client.cli || asm.Election.cli != asm.Client.cli {
		t.Fatal("discovery or elections are not on the client's connection")
	}
	if asm.Discovery.retryMinInterval != 3*time.Second || asm.Discovery.retryMaxInterval != 9*time.Second {
		t.Fatalf("retry intervals = %v / %v", asm.Discovery.retryMinInterval, asm.Discovery.retryMaxInterval)
	}
	var _ fetcd.IEtcd = asm.Client
	var _ fetcd.IDiscovery = asm.Discovery
	var _ fetcd.IElectionFactory = asm.Election

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := asm.Ping(ctx); err == nil {
		t.Fatal("Ping against a closed port succeeded")
	}
	if err := asm.Start(ctx, &fetcd.ServiceInfo{ServiceType: "game", Sid: 1}); err == nil {
		t.Fatal("Start against a closed port succeeded")
	}
	var none *Assembly
	if err := none.Ping(context.Background()); err == nil {
		t.Fatal("nil assembly Ping returned no error")
	}
	if err := none.Close(context.Background()); err != nil {
		t.Fatalf("nil assembly Close = %v", err)
	}
}
