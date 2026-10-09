package ops

import (
	"context"

	"errors"

	"net/http"
	"net/http/httptest"

	"testing"
)

func requestWithHeaders(headers map[string]string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/admin/execute", nil)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	return request
}

// Init refuses admin_enabled without a token, but an Server assembled
// directly must not authorize a request that simply omits the header.
func TestOpsAdminAuthorizationRejectsEverythingWithoutAConfiguredToken(t *testing.T) {
	mod := &Server{}
	for _, header := range []map[string]string{
		{},
		{"X-Admin-Token": ""},
		{"Authorization": "Bearer "},
		{"Authorization": ""},
	} {
		if mod.authorized(requestWithHeaders(header)) {
			t.Fatalf("empty configured token authorized %v", header)
		}
	}
}

// The admin endpoint executes every registered admin command, so its token is
// a credential — compared in constant time, accepted from either header form,
// and never satisfied by an empty configured token. Authorization must name the
// Bearer scheme (case-insensitive, RFC 7235): a bare token there used to pass
// and is refused since the maintainers' round-12 decision (N01b O-P1).
func TestOpsAdminAuthorizationAcceptsOnlyTheExactToken(t *testing.T) {
	mod := &Server{adminToken: "s3cret-token"}
	for name, header := range map[string]map[string]string{
		"dedicated header":    {"X-Admin-Token": "s3cret-token"},
		"bearer":              {"Authorization": "Bearer s3cret-token"},
		"lowercase bearer":    {"Authorization": "bearer s3cret-token"},
		"bearer with padding": {"Authorization": "  Bearer   s3cret-token  "},
	} {
		if !mod.authorized(requestWithHeaders(header)) {
			t.Fatalf("%s: valid token rejected", name)
		}
	}
	for name, header := range map[string]map[string]string{
		"no header":          {},
		"empty header":       {"X-Admin-Token": ""},
		"wrong token":        {"X-Admin-Token": "s3cret-tokeN"},
		"prefix of token":    {"X-Admin-Token": "s3cret"},
		"token plus suffix":  {"X-Admin-Token": "s3cret-token-extra"},
		"wrong scheme":       {"Authorization": "Basic s3cret-token"},
		"bearer no token":    {"Authorization": "Bearer "},
		"bare authorization": {"Authorization": "s3cret-token"},
		"padded bare token":  {"Authorization": "  s3cret-token "},
	} {
		if mod.authorized(requestWithHeaders(header)) {
			t.Fatalf("%s: authorized when it must not be", name)
		}
	}
}

func TestServerStopWithContextUsesCallerContext(t *testing.T) {
	mod := &Server{server: &http.Server{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := mod.StopWithContext(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("StopWithContext err = %v, want context canceled", err)
	}
}

func TestHTTPWriteTimeoutKeepsAdminMargin(t *testing.T) {
	m := New()
	if err := m.Configure(Config{Enabled: true, Addr: "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	if want := defaultAdminTimeout + adminWriteMargin; m.server.WriteTimeout < want {
		t.Fatalf("write timeout=%v want≥%v", m.server.WriteTimeout, want)
	}
}
