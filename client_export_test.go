package redis

import (
	"testing"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

// NewClient exists so a caller outside the kit can exercise real Redis
// semantics — Lua, pipelines — that an in-memory double reimplements rather
// than executes. It must refuse a configuration that cannot connect anywhere
// rather than hand back a client that fails on first use.
func TestNewClientRequiresAnAddress(t *testing.T) {
	if _, err := NewClient(nil); err == nil {
		t.Fatal("a nil configuration was accepted")
	}
	if _, err := NewClient(&fredis.Config{}); err == nil {
		t.Fatal("a configuration with no address was accepted")
	}
	client, err := NewClient(fredis.DefaultConfig("127.0.0.1:6379"))
	if err != nil {
		t.Fatalf("a single-node configuration was rejected: %v", err)
	}
	if client == nil {
		t.Fatal("NewClient returned a nil client with no error")
	}
	_ = client.Close()

	cluster, err := NewClient(&fredis.Config{ClusterAddrs: []string{"127.0.0.1:7000"}})
	if err != nil {
		t.Fatalf("a cluster configuration was rejected: %v", err)
	}
	_ = cluster.Close()
}
