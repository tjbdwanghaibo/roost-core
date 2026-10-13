package driver

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	gonats "github.com/nats-io/nats.go"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

var errNatsDisconnectForTest = errors.New("disconnect failed")

func TestHandleNatsDisconnectLogsExpectedCloseAsInfo(t *testing.T) {
	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(restore) })

	var called atomic.Bool
	cfg := fnats.DefaultConfig("nats://test")
	cfg.OnDisconnect = func(error) { called.Store(true) }
	state := &natsLifecycleState{}
	state.draining.Store(true)

	handleNatsDisconnect(state, cfg, nil)

	logs := buf.String()
	if strings.Contains(logs, `"level":"ERROR"`) {
		t.Fatalf("expected graceful disconnect not to log ERROR, got %s", logs)
	}
	if !strings.Contains(logs, `"level":"INFO"`) {
		t.Fatalf("expected graceful disconnect to log INFO, got %s", logs)
	}
	if !called.Load() {
		t.Fatalf("OnDisconnect was not called")
	}
}

func TestHandleNatsDisconnectLogsUnexpectedErrorAsError(t *testing.T) {
	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(restore) })

	handleNatsDisconnect(&natsLifecycleState{}, fnats.DefaultConfig("nats://test"), errNatsDisconnectForTest)

	if !strings.Contains(buf.String(), `"level":"ERROR"`) {
		t.Fatalf("expected unexpected disconnect to log ERROR, got %s", buf.String())
	}
}

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
	if got := applyOptions(t, buildNatsOptions(cfg, &natsLifecycleState{}, ClientOptions{})); got.IgnoreDiscoveredServers {
		t.Fatal("discovered servers must be followed by default")
	}
	if got := applyOptions(t, buildNatsOptions(cfg, &natsLifecycleState{}, ClientOptions{IgnoreDiscoveredServers: true})); !got.IgnoreDiscoveredServers {
		t.Fatal("nats.ignore_discovered_servers=true did not reach the connection options")
	}
}
