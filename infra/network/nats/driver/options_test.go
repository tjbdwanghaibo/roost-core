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

// nats.url 可以由部署从环境变量注入（R11），连接串可能带 userinfo（nats://user:pass@host、nats://token@host）。
// 连接失败的错误与 “connected” 日志只带脱敏后的地址：口令与 token 不进错误、不进日志。
func TestConnectErrorDoesNotCarryURLCredentials(t *testing.T) {
	for _, url := range []string{
		"nats://roost:s3cret-pw@127.0.0.1:1",
		"nats://s3cret-pw@127.0.0.1:1",
		"nats://127.0.0.1:2, nats://roost:s3cret-pw@127.0.0.1:1",
	} {
		_, err := NewClient(fnats.DefaultConfig(url), ClientOptions{})
		if err == nil {
			t.Fatalf("%s: connected to a closed port", url)
		}
		if strings.Contains(err.Error(), "s3cret-pw") {
			t.Fatalf("connect error carries the credential: %v", err)
		}
		if !strings.Contains(err.Error(), "127.0.0.1:1") {
			t.Fatalf("connect error no longer says where it tried: %v", err)
		}
	}
}

func TestRedactedURLsHidesTheWholeUserinfo(t *testing.T) {
	for in, want := range map[string]string{
		"nats://roost:pw@n1:4222":                 "nats://***@n1:4222",
		"nats://token@n1:4222":                    "nats://***@n1:4222",
		"nats://n1:4222, nats://roost:pw@n2:4222": "nats://n1:4222,nats://***@n2:4222",
		"nats://127.0.0.1:4222":                   "nats://127.0.0.1:4222",
		"nats://roost:p%zz@n1:4222":               "***@n1:4222", // 解析不了的 URL 只留 '@' 之后
		"tls://roost:pw@n1:4443,tls://n2:4443":    "tls://***@n1:4443,tls://n2:4443",
	} {
		if got := RedactedURLs(in); got != want {
			t.Errorf("RedactedURLs(%q) = %q, want %q", in, got, want)
		}
	}
}

// 写在 URL 之外的账号（nats.username / password，R11）经 nats.go 的 UserInfo 选项交给连接。
func TestBuildNatsOptionsCarriesTheSeparateAccount(t *testing.T) {
	apply := func(cfg *fnats.Config) gonats.Options {
		options := gonats.GetDefaultOptions()
		for _, option := range buildNatsOptions(cfg, &natsLifecycleState{}, ClientOptions{}) {
			if err := option(&options); err != nil {
				t.Fatal(err)
			}
		}
		return options
	}
	cfg := fnats.DefaultConfig("nats://n1:4222")
	cfg.Username, cfg.Password = "roost", "pw"
	if options := apply(cfg); options.User != "roost" || options.Password != "pw" {
		t.Fatalf("user / password = %q / %q", options.User, options.Password)
	}
	if options := apply(fnats.DefaultConfig("nats://n1:4222")); options.User != "" || options.Password != "" {
		t.Fatalf("no account configured, got user %q", options.User)
	}
}
