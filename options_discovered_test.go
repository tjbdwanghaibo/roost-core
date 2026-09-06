package nats

import (
	"testing"

	fnats "github.com/tjbdwanghaibo/roost-core/nats"

	gonats "github.com/nats-io/nats.go"
)

func applyOptions(t *testing.T, opts []gonats.Option) gonats.Options {
	t.Helper()
	resolved := gonats.GetDefaultOptions()
	for _, opt := range opts {
		if err := opt(&resolved); err != nil {
			t.Fatal(err)
		}
	}
	return resolved
}

// Behind a proxy, NAT, or a fault injector the client must not follow cluster
// gossip to the members' real addresses; the operator's URL is the path.
// Default behaviour stays as before so flat-network deployments keep failover
// through discovery.
func TestIgnoreDiscoveredServersIsAnOptInThatReachesTheConnection(t *testing.T) {
	cfg := fnats.DefaultConfig("nats://127.0.0.1:24222")
	if got := applyOptions(t, buildNatsOptions(cfg, &natsLifecycleState{}, clientOptions{})); got.IgnoreDiscoveredServers {
		t.Fatal("discovered servers must be followed by default")
	}
	if got := applyOptions(t, buildNatsOptions(cfg, &natsLifecycleState{}, clientOptions{ignoreDiscoveredServers: true})); !got.IgnoreDiscoveredServers {
		t.Fatal("nats.ignore_discovered_servers=true did not reach the connection options")
	}
}
