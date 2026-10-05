package ops

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/admin"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/internal/stopcontract"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
)

func TestOpsStartServesOnTheBoundAddress(t *testing.T) {
	m := newStartableOpsMod(t, "127.0.0.1:0")
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.StopWithContext(context.Background()) })
	addr := m.listenAddr()
	if addr == "" || strings.HasSuffix(addr, ":0") {
		t.Fatalf("listen address = %q, want the bound port", addr)
	}
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz right after Start: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz = %d", resp.StatusCode)
	}
	if want := defaultAdminTimeout + adminWriteMargin; m.server.WriteTimeout < want {
		t.Fatalf("write timeout = %s, want ≥ admin timeout + margin %s so a timed-out command still gets its 504", m.server.WriteTimeout, want)
	}
}

// A3 停机契约骨架套 OpsMod（RR-20261004-NC-04 的停止入口）：卡住的工作是一个不配合 ctx 的 admin 命令。
// Ops 不持有 health / admin / metrics 这些依赖，App 在 StopWithContext 返回 nil 之后才停它们
// （stopcontract.CallerReleases）。
func TestOpsStopContract(t *testing.T) {
	var m *OpsMod
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	responded := make(chan error, 1)
	var stop func(context.Context) error
	var released func() bool
	stopcontract.Check(t, stopcontract.Hooks{
		Start: func(testing.TB) {
			cfg := viper.New()
			cfg.Set("ops.enabled", true)
			cfg.Set("ops.addr", "127.0.0.1:0")
			cfg.Set("ops.admin_enabled", true)
			cfg.Set("ops.admin_token", "secret-token")
			m = NewOpsMod()
			if err := m.Init(cfg); err != nil {
				t.Fatal(err)
			}
			registry := app.NewRegistry(viper.New())
			if err := m.Provide(registry); err != nil {
				t.Fatal(err)
			}
			if err := app.MustLookup[*admin.Registry](registry, mods.ModAdmin).Register(admin.CommandDef{
				Name: "stuck",
				Handler: func(context.Context, admin.Command) (admin.Result, error) {
					entered <- struct{}{}
					<-gate // 不配合 ctx
					return admin.Result{}, nil
				},
			}); err != nil {
				t.Fatal(err)
			}
			if err := m.Start(); err != nil {
				t.Fatal(err)
			}
			stop, released = stopcontract.CallerReleases(m.StopWithContext)
		},
		Block: func(testing.TB) {
			go func() {
				req, _ := http.NewRequest(http.MethodPost, "http://"+m.listenAddr()+"/admin/execute", bytes.NewBufferString(`{"name":"stuck"}`))
				req.Header.Set("X-Admin-Token", "secret-token")
				resp, err := http.DefaultClient.Do(req)
				if err == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}
				responded <- err
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("admin command never started")
			}
		},
		Stop:     func(ctx context.Context) error { return stop(ctx) },
		Release:  func() { close(gate) },
		Released: func() bool { return released() },
	})
	if err := <-responded; err != nil {
		t.Fatalf("the admitted admin request lost its reply: %v", err)
	}
}
