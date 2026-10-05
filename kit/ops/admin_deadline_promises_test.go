package ops

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/admin"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
)

// N02 观察 O1 / N01 Ops 留项：/admin/execute 交给命令的 ctx 带 ops.admin_timeout 的期限，Ops 的 HTTP
// 写超时比它长，配合 ctx 的命令到期返回后，运维一定拿到一个明确的“超时、结果未知”回复（504），
// 而不是传输错误。
//
// 旧行为：命令拿到的是请求 ctx，没有期限；写超时固定 15s。超过 15s 的命令照常执行完，回复写不出去，
// 客户端只看到 EOF：命令执行了没有、执行到哪一步都不知道，重试可能重复执行。
func TestOpsAdminCommandRunsUnderTheConfiguredDeadline(t *testing.T) {
	cfg := viper.New()
	cfg.Set("ops.admin_enabled", true)
	cfg.Set("ops.admin_token", "secret-token")
	cfg.Set("ops.admin_timeout", "100ms")
	m := NewOpsMod()
	if err := m.Init(cfg); err != nil {
		t.Fatalf("Init: %v", err)
	}
	registry := app.NewRegistry(viper.New())
	if err := m.Provide(registry); err != nil {
		t.Fatalf("Provide: %v", err)
	}
	commands := app.MustLookup[*admin.Registry](registry, mods.ModAdmin)
	deadlines := make(chan time.Duration, 1)
	if err := commands.Register(admin.CommandDef{Name: "slow", Handler: func(ctx context.Context, _ admin.Command) (admin.Result, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			deadlines <- -1
			return admin.Result{}, nil
		}
		deadlines <- time.Until(deadline)
		<-ctx.Done() // 配合 ctx 的长命令
		return admin.Result{}, ctx.Err()
	}}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/execute", bytes.NewBufferString(`{"name":"slow","trace_id":"t-1"}`))
	req.Header.Set("X-Admin-Token", "secret-token")
	rec := httptest.NewRecorder()
	m.handleAdminExecute(rec, req)

	if remaining := <-deadlines; remaining < 0 || remaining > 100*time.Millisecond {
		t.Fatalf("admin command deadline remaining = %v, want ≤ ops.admin_timeout (100ms); -1 means the command ran without a deadline", remaining)
	}
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d body=%s, want 504 for a command that ran out of ops.admin_timeout (its outcome is unknown)", rec.Code, rec.Body.String())
	}
}
