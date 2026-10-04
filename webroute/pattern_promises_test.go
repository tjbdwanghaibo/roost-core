package webroute

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/httpserver"
)

// RR-20261004-NC-07：非法模式返回 error，不碰业务路由；安装失败不能提前占 seen。
func TestRegisterRejectsMalformedPatternsBeforeInstallation(t *testing.T) {
	for _, path := range []string{"/broken/{id", "/broken/{id:[}", "/broken/*/tail"} {
		t.Run(path, func(t *testing.T) {
			engine := httpserver.NewEngine()
			reg := NewRegistrar(engine)
			var err error
			var escaped any
			func() {
				defer func() { escaped = recover() }()
				err = reg.Register("GET", path, func(http.ResponseWriter, *http.Request) {})
			}()
			if escaped != nil || err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("error=%v panic=%v", err, escaped)
			}
			if len(reg.seen) != 0 {
				t.Fatalf("invalid pattern occupied seen=%v", reg.seen)
			}
			if err := reg.Register("GET", "/healthy", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, httptest.NewRequest("GET", "/healthy", nil))
			if w.Code != 204 {
				t.Fatalf("healthy route status=%d", w.Code)
			}
		})
	}
}

type failingRouteInstaller struct {
	calls   int
	failure error
}

func (r *failingRouteInstaller) Get(string, http.HandlerFunc) {
	r.calls++
	if r.failure != nil {
		panic(r.failure)
	}
}
func (r *failingRouteInstaller) Post(path string, h http.HandlerFunc) { r.Get(path, h) }

func TestRegisterInstallationPanicDoesNotReservePair(t *testing.T) {
	cause := errors.New("installer failed")
	routes := &failingRouteInstaller{failure: cause}
	reg := NewRegistrar(routes)
	var err error
	var escaped any
	func() {
		defer func() { escaped = recover() }()
		err = reg.Register("POST", "/retry", func(http.ResponseWriter, *http.Request) {})
	}()
	if escaped != nil || !errors.Is(err, cause) {
		t.Fatalf("error=%v panic=%v", err, escaped)
	}
	routes.failure = nil
	if err := reg.Register("POST", "/retry", func(http.ResponseWriter, *http.Request) {}); err != nil {
		t.Fatalf("failed install reserved pair: %v", err)
	}
	if routes.calls != 2 {
		t.Fatalf("calls=%d", routes.calls)
	}
}

func TestRegisterAcceptsChiPatterns(t *testing.T) {
	for _, tc := range []struct{ pattern, url string }{{"/", "/"}, {"/items/{id}", "/items/42"}, {"/items/{id:[0-9]+}", "/items/42"}, {"/files/*", "/files/a/b"}, {"/ids/{id:[0-9]{2}}", "/ids/42"}} {
		t.Run(tc.pattern, func(t *testing.T) {
			engine := httpserver.NewEngine()
			if err := NewRegistrar(engine).Register("GET", tc.pattern, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, httptest.NewRequest("GET", tc.url, nil))
			if w.Code != 204 {
				t.Fatalf("status=%d", w.Code)
			}
		})
	}
}
