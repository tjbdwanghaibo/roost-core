//go:build integration

package redis

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
)

// 业务时间只许前进（docs/feature/BUSINESS-TIME-MONOTONIC-2026-10-06.md）在真实 Redis 上：App 经
// kitredis.SingletonStore 读写部署级高水位 <singleton.key_prefix>:business_time（不过期的键）。
// 先以 +24h 偏移启动一次（写下高水位），再把偏移改回 0 启动：被拒绝，键原样保留。
//
//	REDIS_ADDR=127.0.0.1:6379 go test -tags integration ./wiring/redis/ -run BusinessTime

type businessTimeProbe struct{ served chan struct{} }

func (p *businessTimeProbe) Name() app.ServiceName    { return "game" }
func (p *businessTimeProbe) Init(*app.Registry) error { return nil }
func (p *businessTimeProbe) Serve(ctx context.Context) error {
	close(p.served)
	<-ctx.Done()
	return nil
}
func (p *businessTimeProbe) Shutdown(context.Context) error { return nil }

func runBusinessTimeApp(t *testing.T, addr, prefix, offset string, stopAfterServe bool) error {
	t.Helper()
	dir := t.TempDir()
	config := fmt.Sprintf(`sid: 1
log:
  file: false
  stdout: false
redis:
  addr: %s
singleton:
  enabled: true
  key_prefix: %s
time:
  logic_offset: %s
`, addr, prefix, offset)
	path := filepath.Join(dir, "config.game.yaml")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	probe := &businessTimeProbe{served: make(chan struct{})}
	a := app.New("roost-test", "0.0.0").Singleton(SingletonStore)
	a.RegisterServer("game", probe)
	a.RootCmd().SetArgs([]string{"game", "--config", path})
	result := make(chan error, 1)
	go func() { result <- a.Execute() }()
	select {
	case <-probe.served:
		if !stopAfterServe {
			t.Fatal("the service started serving")
		}
		process, _ := os.FindProcess(os.Getpid())
		_ = process.Signal(os.Interrupt)
		return <-result
	case err := <-result:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("run neither served nor returned")
		return nil
	}
}

func TestBusinessTimeHighWaterMarkOnRealRedis(t *testing.T) {
	store, _ := singletonRedisStore(t)
	addr := os.Getenv("REDIS_ADDR")
	prefix := "roost:test:kitredis-business-time:" + rand.Text()
	key := prefix + ":business_time"
	cleanupSingletonKeys(t, store, key, prefix+":game:1")

	// 第一次：+24h 偏移，写下高水位（真实时间 + 24h），不过期。
	if err := runBusinessTimeApp(t, addr, prefix, "24h", true); err != nil {
		t.Fatalf("first run with +24h: %v", err)
	}
	values, err := store.Get(context.Background(), []string{key})
	if err != nil || values[0] == nil {
		t.Fatalf("high-water mark after the +24h run = %q, %v; want it written", values, err)
	}
	mark := values[0]
	unixMs, err := strconv.ParseInt(string(mark[:13]), 10, 64)
	if err != nil || time.UnixMilli(unixMs).Before(time.Now().Add(24*time.Hour-time.Minute)) {
		t.Fatalf("high-water mark %q, want real time + 24h", mark)
	}
	// 第二次：偏移改回 0，拒绝启动，键不变。
	err = runBusinessTimeApp(t, addr, prefix, "0s", false)
	if !errors.Is(err, app.ErrBusinessTimeMovedBack) {
		t.Fatalf("second run with the offset back at 0: %v, want ErrBusinessTimeMovedBack", err)
	}
	after, err := store.Get(context.Background(), []string{key})
	if err != nil || string(after[0]) != string(mark) {
		t.Fatalf("high-water mark after the refusal = %q, %v; want %q untouched", after, err, mark)
	}
}
