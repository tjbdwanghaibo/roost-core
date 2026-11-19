package httpserver

import (
	"net/http"
	"net/http/httptest"
	"net/url"
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
	// RV-20261118-08：写明默认端口（https 的 443、http 的 80）时浏览器发的 Origin 不带端口，永远对不上；拒绝并提示该写的值。
	// 非默认端口照常接受。
	for origin, want := range map[string]string{
		"https://play.example.com:443": "https://play.example.com",
		"http://localhost:80":          "http://localhost",
		"https://[::1]:443":            "https://[::1]",
	} {
		err := CheckCORSOrigins("portal.cors_origins", []string{origin})
		if err == nil || !strings.Contains(err.Error(), "portal.cors_origins") || !strings.Contains(err.Error(), `"`+want+`"`) {
			t.Fatalf("%q: err = %v, want refused naming the key and suggesting %q", origin, err, want)
		}
	}
	if err := CheckCORSOrigins("portal.cors_origins", []string{"https://play.example.com:80", "http://play.example.com:443", "https://play.example.com:8443"}); err != nil {
		t.Fatalf("non-default ports: %v", err)
	}
}

// RV-20261118-10：配置检查接受的每一项，都要与浏览器（WHATWG URL 序列化）实际发出的 Origin 逐字节相同，否则预检 403。
// 端口与数字主机有多种写法指向同一个地址：前导零的端口（:0443 → 省略，:08443 → :8443）、空端口（"…:" → 省略）、
// 简写或八进制 / 十六进制的 IPv4（127.1、0x7f.0.0.1 → 127.0.0.1）。这些写法要么被拒绝（端口类给出该写的值），
// 要么预检 204；不能“检查通过、预检 403”。浏览器解析不了的（越界的 IPv4、越界端口）一律拒绝。
// 规范写法（非默认端口 :8443、点分十进制 IPv4、方括号 IPv6、DNS 名）照常接受，预检 204。
func TestCheckCORSOriginsAcceptsOnlyWhatTheBrowserSerializes(t *testing.T) {
	const key = "portal.cors_origins"
	preflight := func(configured, browser string) int {
		request := httptest.NewRequest(http.MethodOptions, "/portal/login", nil)
		request.Header.Set("Origin", browser)
		recorder := httptest.NewRecorder()
		NewCORS([]string{configured}, []string{http.MethodPost}, []string{"Content-Type"}).Preflight(recorder, request)
		return recorder.Code
	}
	// 配置的写法 → 浏览器序列化后的 Origin（Chrome / Firefox / Node 的 new URL(...).origin 相同）。
	for configured, browser := range map[string]string{
		"https://play.example.com:0443":  "https://play.example.com",
		"https://play.example.com:08443": "https://play.example.com:8443",
		"http://localhost:080":           "http://localhost",
		"https://[::1]:08443":            "https://[::1]:8443",
		"https://play.example.com:":      "https://play.example.com",
		"http://127.1:5173":              "http://127.0.0.1:5173",
		"https://0x7f.0.0.1":             "https://127.0.0.1",
		"https://010.0.0.1":              "https://8.0.0.1",
		"https://[2001:db8:0:0:0:0:0:1]": "https://[2001:db8::1]",
		"https://[::ffff:192.0.2.30]":    "https://[::ffff:c000:21e]",
	} {
		err := CheckCORSOrigins(key, []string{configured})
		if err == nil {
			if code := preflight(configured, browser); code != http.StatusNoContent {
				t.Errorf("%q passed the config check but the browser sends Origin %q and the preflight answers %d", configured, browser, code)
			}
			continue
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("%q: err = %v, want it to name the key", configured, err)
		}
		if strings.Contains(configured, "example.com") || strings.Contains(configured, "localhost") || strings.Contains(configured, "[") {
			if !strings.Contains(err.Error(), `"`+browser+`"`) {
				t.Errorf("%q: err = %v, want it to suggest %q", configured, err, browser)
			}
		}
	}
	// 不写方括号的 IPv6（https://2001:db8::1:8443）浏览器解析不了，与越界 IPv4 一样拒绝。
	for _, origin := range []string{"https://192.0.2.300", "https://192.0.2.30.1.1", "https://1.2.3.4.example.1", "https://play.example.com:65536", "https://[::1%25en0]:8443", "https://[1.2.3.4]",
		"https://2001:db8::1:8443", "https://2001:db8::1", "http://::1:5173"} {
		if err := CheckCORSOrigins(key, []string{origin}); err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%q: err = %v, want refused naming the key (a browser cannot parse it)", origin, err)
		}
	}
	// 0 号端口浏览器能解析（序列化为 :0），但没有页面能从那里打开：与 WebSocket 地址的规则一样只收 1～65535。
	if err := CheckCORSOrigins(key, []string{"https://play.example.com:0"}); err == nil {
		t.Error(`"https://play.example.com:0" passed; port 0 serves no page`)
	}
	for _, origin := range []string{"https://play.example.com:8443", "https://192.0.2.30", "http://192.0.2.30:5173", "https://[2001:db8::1]:8443", "https://[::ffff:c000:21e]", "http://localhost:5173", "https://a1.example", "https://2048.example.com"} {
		if err := CheckCORSOrigins(key, []string{origin}); err != nil {
			t.Errorf("%q: %v, want accepted", origin, err)
			continue
		}
		if code := preflight(origin, origin); code != http.StatusNoContent {
			t.Errorf("%q: preflight %d", origin, code)
		}
	}
}

// BrowserHost 本身（不经 url.Parse）：主机里有冒号却不在方括号里，浏览器把第一个冒号之后当端口、解析失败
// （Node 的 new URL("ws://2001:db8::1:7080/ws") 报 ERR_INVALID_URL），要拒绝并提示写成 [...]。Go 的 url.Parse 只对
// http / https 拒绝这种主机（GODEBUG urlstrictcolons），ws / wss 照样解析成主机 "2001:db8::1"，所以规则要在这里。
// 合法的点分十进制 IPv4、方括号 IPv6、DNS 名照常返回。
func TestBrowserHostRefusesIPv6WithoutBrackets(t *testing.T) {
	for _, host := range []string{"2001:db8::1:7080", "2001:db8::1", "::1:5173", "fe80::1"} {
		for _, scheme := range []string{"ws", "https"} {
			if got, err := BrowserHost(&url.URL{Scheme: scheme, Host: host}); err == nil || !strings.Contains(err.Error(), "brackets") {
				t.Errorf("%s://%s: got %q, %v; want refused, telling to put the IPv6 address in brackets", scheme, host, got, err)
			}
		}
	}
	for host, want := range map[string]string{
		"[2001:db8::1]:7080": "[2001:db8::1]", "[::1]": "[::1]", "192.0.2.30:7080": "192.0.2.30", "game.example:7080": "game.example",
		"game.example:": "game.example", "localhost": "localhost",
	} {
		if got, err := BrowserHost(&url.URL{Scheme: "ws", Host: host}); err != nil || got != want {
			t.Errorf("ws://%s: got %q, %v; want %q", host, got, err, want)
		}
	}
}
