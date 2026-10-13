package webroute

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/network/httpserver"
)

func TestDecodeJSONRejectsMalformedBody(t *testing.T) {
	engine := httpserver.NewEngine()
	registrar := NewRegistrar(engine)
	if err := registrar.Register(http.MethodPost, "/json", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Name string `json:"name"`
		}
		if !DecodeJSON(w, r, &request) {
			return
		}
		WriteResult(w, http.StatusOK, map[string]string{"name": request.Name}, nil)
	}); err != nil {
		t.Fatalf("register route: %v", err)
	}

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/json", strings.NewReader(`{"name":`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestReadRawCopiesRequestData(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader("payload"))
	request.RemoteAddr = "127.0.0.1:8080"
	request.Header.Set("X-Test", "one")
	rec := httptest.NewRecorder()

	raw, ok := ReadRaw(rec, request)
	if !ok {
		t.Fatalf("read raw failed: %s", rec.Body.String())
	}
	request.Header.Set("X-Test", "changed")
	if got := string(raw.Body); got != "payload" {
		t.Fatalf("body = %q", got)
	}
	if got := raw.Header.Get("X-Test"); got != "one" {
		t.Fatalf("header = %q", got)
	}
}

type testModule struct {
	calls *int
	err   error
}

func (m testModule) RegisterRoutes(Registerer) error {
	*m.calls++
	return m.err
}

func TestRegisterModulesStopsOnFirstError(t *testing.T) {
	calls := 0
	wantErr := errors.New("register failed")
	err := RegisterModules(nil,
		testModule{calls: &calls},
		testModule{calls: &calls, err: wantErr},
		testModule{calls: &calls},
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

// nil 模块拒绝注册，且之前的模块已注册、之后的不再注册。
func TestRegisterModulesRefusesANilModule(t *testing.T) {
	calls := 0
	err := RegisterModules(nil, testModule{calls: &calls}, nil, testModule{calls: &calls})
	if err == nil || !strings.Contains(err.Error(), "route module is required") {
		t.Fatalf("RegisterModules with a nil module = %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (stop at the nil module)", calls)
	}
}

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

// 非法模式返回 error，不碰业务路由；安装失败不能提前占用 method + path。
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
