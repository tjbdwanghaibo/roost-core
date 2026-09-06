package webroute

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/httpserver"
)

// Routes are the external HTTP surface; a route without a handler, with a
// relative path, or with a method the server does not route would be a hole
// that only shows up when the first request arrives. Each refusal is pinned
// by message (the earlier test only checked err != nil).
func TestRegistrarRefusesEachInvalidRouteByMessage(t *testing.T) {
	registrar := NewRegistrar(httpserver.NewEngine())
	noop := func(http.ResponseWriter, *http.Request) {}
	cases := []struct {
		name, method, path string
		handler            http.HandlerFunc
		want               string
	}{
		{"nil handler", http.MethodPost, "/events", nil, "route handler is required"},
		{"empty path", http.MethodPost, "  ", noop, `invalid route path ""`},
		{"relative path", http.MethodPost, "events", noop, `invalid route path "events"`},
		{"unsupported method", http.MethodPut, "/events", noop, `unsupported method "PUT"`},
		{"unknown method", "FETCH", "/events", noop, `unsupported method "FETCH"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := registrar.Register(tc.method, tc.path, tc.handler)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Register = %v, want %q", err, tc.want)
			}
		})
	}
	if err := registrar.Register(" post ", " /events ", noop); err != nil {
		t.Fatalf("method and path are normalized before validation: %v", err)
	}
	if err := registrar.Register(http.MethodPost, "/events", noop); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate = %v", err)
	}
	var nilRegistrar *Registrar
	if err := nilRegistrar.Register(http.MethodGet, "/x", noop); err == nil || !strings.Contains(err.Error(), "registrar is required") {
		t.Fatalf("nil registrar = %v", err)
	}
}
