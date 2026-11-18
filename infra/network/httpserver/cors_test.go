package httpserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 跨域白名单经真实的 Engine 路由：POST 路由包一层 Handle、同一路径登记 OPTIONS 答预检。
//   - 白名单里的 Origin：预检 204，带允许的方法、请求头；POST 的响应带 Access-Control-Allow-Origin（回写它自己）。
//   - 不在白名单里的、空的、"null"：预检 403；POST 照常处理但响应不带 Allow-Origin（浏览器读不到）。
//   - 从不发 Access-Control-Allow-Credentials；每个响应都带 Vary: Origin。
//   - "*" 放行任何 Origin（仍回写具体的 Origin，不是 "*"），"null" 除外。
func TestCORSAllowsOnlyListedOriginsThroughTheEngine(t *testing.T) {
	serve := func(origins []string) *Engine {
		cors := NewCORS(origins, []string{http.MethodPost}, []string{"Content-Type"})
		engine := NewEngine()
		engine.Post("/portal/login", cors.Handle(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		engine.Options("/portal/login", cors.Preflight)
		return engine
	}
	do := func(engine *Engine, method, origin string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, "/portal/login", strings.NewReader(`{}`))
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		if method == http.MethodOptions {
			request.Header.Set("Access-Control-Request-Method", http.MethodPost)
			request.Header.Set("Access-Control-Request-Headers", "content-type")
		}
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, request)
		return recorder
	}
	common := func(what string, response *httptest.ResponseRecorder) {
		t.Helper()
		if response.Header().Get("Access-Control-Allow-Credentials") != "" {
			t.Fatalf("%s: Access-Control-Allow-Credentials is set", what)
		}
		if !strings.Contains(strings.Join(response.Header().Values("Vary"), ","), "Origin") {
			t.Fatalf("%s: no Vary: Origin (%v)", what, response.Header())
		}
	}

	listed := serve([]string{"https://www.play.example.com", "http://localhost:5173"})
	for _, origin := range []string{"https://www.play.example.com", "http://localhost:5173"} {
		preflight := do(listed, http.MethodOptions, origin)
		common("preflight "+origin, preflight)
		if preflight.Code != http.StatusNoContent || preflight.Header().Get("Access-Control-Allow-Origin") != origin ||
			preflight.Header().Get("Access-Control-Allow-Methods") != "POST" || preflight.Header().Get("Access-Control-Allow-Headers") != "Content-Type" ||
			preflight.Header().Get("Access-Control-Max-Age") != "600" {
			t.Fatalf("preflight from %s: %d %v", origin, preflight.Code, preflight.Header())
		}
		post := do(listed, http.MethodPost, origin)
		common("post "+origin, post)
		if post.Code != http.StatusOK || post.Header().Get("Access-Control-Allow-Origin") != origin {
			t.Fatalf("post from %s: %d %v", origin, post.Code, post.Header())
		}
	}
	for _, origin := range []string{"https://evil.example", "https://www.play.example.com.evil.example", "https://play.example.com", "http://www.play.example.com", "null", ""} {
		preflight := do(listed, http.MethodOptions, origin)
		common("preflight "+origin, preflight)
		if preflight.Code != http.StatusForbidden || preflight.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("preflight from %q: %d %v, want 403 without Allow-Origin", origin, preflight.Code, preflight.Header())
		}
		post := do(listed, http.MethodPost, origin)
		common("post "+origin, post)
		if post.Code != http.StatusOK || post.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("post from %q: %d %v, want the request served without Allow-Origin", origin, post.Code, post.Header())
		}
	}

	none := serve(nil)
	if preflight := do(none, http.MethodOptions, "https://www.play.example.com"); preflight.Code != http.StatusForbidden {
		t.Fatalf("an empty allow-list answered a preflight with %d", preflight.Code)
	}
	anyOrigin := serve([]string{"*"})
	if preflight := do(anyOrigin, http.MethodOptions, "https://anything.example"); preflight.Code != http.StatusNoContent || preflight.Header().Get("Access-Control-Allow-Origin") != "https://anything.example" {
		t.Fatalf("\"*\": %d %v", preflight.Code, preflight.Header())
	}
	if preflight := do(anyOrigin, http.MethodOptions, "null"); preflight.Code != http.StatusForbidden {
		t.Fatalf("\"*\" let the null origin through: %d", preflight.Code)
	}
}

// 白名单的写法：只认浏览器发的 Origin 的样子，多一个斜杠、带路径、大写主机这类永远对不上的写法在读配置时拒绝，点名键。
func TestCheckCORSOriginsRefusesWhatABrowserNeverSends(t *testing.T) {
	if err := CheckCORSOrigins("portal.cors_origins", []string{"*", "https://www.play.example.com", "http://localhost:5173", "https://[::1]:8443"}); err != nil {
		t.Fatalf("valid origins: %v", err)
	}
	if err := CheckCORSOrigins("portal.cors_origins", nil); err != nil {
		t.Fatalf("an empty list: %v", err)
	}
	for _, origin := range []string{"https://www.play.example.com/", "https://www.play.example.com/play", "https://www.play.example.com?x=1", "https://www.play.example.com#a",
		"https://www.Play.example.com", "HTTPS://www.play.example.com", "www.play.example.com", "ftp://www.play.example.com", "https://user@www.play.example.com", "null", "", "https://"} {
		err := CheckCORSOrigins("portal.cors_origins", []string{"https://ok.example", origin})
		if err == nil || !strings.Contains(err.Error(), "portal.cors_origins") {
			t.Fatalf("%q: err = %v, want refused naming the key", origin, err)
		}
	}
}
