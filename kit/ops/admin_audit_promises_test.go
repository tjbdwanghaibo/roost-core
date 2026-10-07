package ops

import (
	"bytes"
	"context"
	"errors"
	"github.com/tjbdwanghaibo/roost-core/admin"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminRefusalIsAuditedWithoutToken(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(old)
	m := &OpsMod{adminEnabled: true, adminToken: "valid-secret"}
	r := httptest.NewRequest(http.MethodPost, "/admin/execute", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer do-not-log-secret")
	w := httptest.NewRecorder()
	m.handleAdminExecute(w, r)
	if w.Code != 401 || !strings.Contains(buf.String(), "unauthorized") || !strings.Contains(buf.String(), "remote_addr") {
		t.Fatalf("refusal code=%d audit=%s", w.Code, buf.String())
	}
	if strings.Contains(buf.String(), "do-not-log-secret") {
		t.Fatal("token leaked")
	}
}
func TestAdminSeparatesUnknownCommandFromHandlerFailure(t *testing.T) {
	m := &OpsMod{adminEnabled: true, adminToken: "b7", commands: admin.NewRegistry()}
	_ = m.commands.Register(admin.CommandDef{Name: "fail", Handler: func(context.Context, admin.Command) (admin.Result, error) {
		return admin.Result{}, errors.New("internal")
	}})
	for name, want := range map[string]int{"missing": 404, "fail": 500} {
		r := httptest.NewRequest(http.MethodPost, "/admin/execute", strings.NewReader(`{"name":"`+name+`"}`))
		r.Header.Set("Authorization", "Bearer b7")
		w := httptest.NewRecorder()
		m.handleAdminExecute(w, r)
		if w.Code != want {
			t.Errorf("%s code=%d want=%d", name, w.Code, want)
		}
	}
}
