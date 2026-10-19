package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
	"github.com/tjbdwanghaibo/roost-core/infra/base/lifecycle"
	"github.com/tjbdwanghaibo/roost-core/infra/network/httpserver"
	"github.com/tjbdwanghaibo/roost-core/infra/network/webroute"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

type pingRoutes struct{}

func (pingRoutes) RegisterRoutes(reg webroute.Registerer) error {
	return reg.Register(http.MethodGet, "/ping", func(w http.ResponseWriter, r *http.Request) {
		webroute.WriteResult(w, http.StatusOK, map[string]string{"pong": "ok"}, nil)
	})
}

// 按 App 的顺序走一遍：Init → Provide → Start（只 bind）→ Service.Init 里 RegisterRoutes → service.started 开始
// Serve → 请求可达 → 之后再登记被拒 → service.stopping 停止监听。
func TestWebModServesRoutesRegisteredInServiceInit(t *testing.T) {
	cfg := viper.New()
	cfg.Set("web.addr", "127.0.0.1:0")
	registry := app.NewRegistry(cfg)
	mod := NewMod()
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	defer mod.Stop()
	if err := RegisterRoutes(registry, pingRoutes{}); err != nil {
		t.Fatalf("RegisterRoutes in Service.Init: %v", err)
	}
	life := app.MustLookup[*lifecycle.Registry](registry, mods.ModLifecycle)
	if err := life.Emit(context.Background(), lifecycle.Event{Phase: lifecycle.PhaseServiceStarted}); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + mod.Addr() + "/ping")
	if err != nil {
		t.Fatalf("GET /ping: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var decoded map[string]any
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &decoded) != nil || !strings.Contains(string(body), "pong") {
		t.Fatalf("GET /ping = %d %s", resp.StatusCode, body)
	}
	if err := mod.Register(http.MethodGet, "/late", func(http.ResponseWriter, *http.Request) {}); err == nil || !strings.Contains(err.Error(), "Service.Init") {
		t.Fatalf("Register after service.started = %v; want a refusal naming Service.Init", err)
	}
	if err := life.Emit(context.Background(), lifecycle.Event{Phase: lifecycle.PhaseServiceStopping}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Get("http://" + mod.Addr() + "/ping"); err == nil {
		t.Fatal("GET /ping succeeded after service.stopping")
	}
}

// 端口被占用时 Start 失败（而不是进程没有路由地跑下去）；没装 web Mod 时 RegisterRoutes 报错并给命令。
func TestWebModStartFailsOnBusyPortAndRegisterNamesTheMod(t *testing.T) {
	first := NewMod()
	cfg := viper.New()
	cfg.Set("web.addr", "127.0.0.1:0")
	if err := first.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	defer first.Stop()
	busy := viper.New()
	busy.Set("web.addr", first.Addr())
	second := NewMod()
	if err := second.Init(busy); err != nil {
		t.Fatal(err)
	}
	if err := second.Start(); err == nil || !strings.Contains(err.Error(), "web.addr") {
		t.Fatalf("Start on a busy port = %v; want an error naming web.addr", err)
	}
	if err := RegisterRoutes(app.NewRegistry(viper.New()), pingRoutes{}); err == nil || !strings.Contains(err.Error(), "roost add mod web") {
		t.Fatalf("RegisterRoutes without the web mod = %v", err)
	}
}

// 生产环境不默认把没有鉴权的业务路由暴露到公网；请求头期限不大于整个请求的期限。
func TestWebModRefusesPublicAddrInProductionUnlessAllowed(t *testing.T) {
	c := config{Enabled: true, Addr: "0.0.0.0:8080"}
	if err := c.ValidateConfig(true); err == nil || !strings.Contains(err.Error(), "web.allow_public_addr") {
		t.Fatalf("production public addr = %v", err)
	}
	c.AllowPublicAddr = true
	if err := c.ValidateConfig(true); err != nil {
		t.Fatal(err)
	}
	if err := (&config{Enabled: true, Addr: "127.0.0.1:8080"}).ValidateConfig(true); err != nil {
		t.Fatal(err)
	}
	// 请求头期限大于整个请求的期限时它永远不会先到期：开发与生产都拒绝。
	slow := config{Enabled: true, Addr: "127.0.0.1:8080", ReadHeaderTimeout: 20 * time.Second, ReadTimeout: 15 * time.Second}
	if err := slow.ValidateConfig(false); err == nil || !strings.Contains(err.Error(), "web.read_header_timeout") || !strings.Contains(err.Error(), "web.read_timeout") {
		t.Fatalf("read_header_timeout > read_timeout = %v; want an error naming both keys", err)
	}
}

// startWebMod 按 App 的顺序起一个 web Mod：Init（settings 写进 viper）→ Provide → Start → 登记 modules → service.started。
func startWebMod(t *testing.T, settings map[string]any, modules ...webroute.Module) *Mod {
	t.Helper()
	cfg := viper.New()
	cfg.Set("web.addr", "127.0.0.1:0")
	for key, value := range settings {
		cfg.Set(key, value)
	}
	registry := app.NewRegistry(cfg)
	mod := NewMod()
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mod.Stop)
	if err := RegisterRoutes(registry, modules...); err != nil {
		t.Fatal(err)
	}
	life := app.MustLookup[*lifecycle.Registry](registry, mods.ModLifecycle)
	if err := life.Emit(context.Background(), lifecycle.Event{Phase: lifecycle.PhaseServiceStarted}); err != nil {
		t.Fatal(err)
	}
	return mod
}

type routeFuncs map[string]http.HandlerFunc // "METHOD /path" → handler

func (routes routeFuncs) RegisterRoutes(reg webroute.Registerer) error {
	for key, handler := range routes {
		method, path, _ := strings.Cut(key, " ")
		if err := reg.Register(method, path, handler); err != nil {
			return err
		}
	}
	return nil
}

// web.* 的超时与请求体上限经声明进入 httpserver：缺省值与 httpserver 的缺省相同；写了就按写的值生效
// （请求体超限回 413，请求头读超时到期关闭连接）。
func TestWebModAppliesDeclaredTimeoutsAndBodyLimit(t *testing.T) {
	schema := NewMod().ConfigSchema()
	defaults := httpserver.DefaultConfig()
	for key, want := range map[string]string{
		"web.read_header_timeout": defaults.ReadHeaderTimeout.String(),
		"web.read_timeout":        defaults.ReadTimeout.String(),
		"web.write_timeout":       defaults.WriteTimeout.String(),
		"web.idle_timeout":        defaults.IdleTimeout.String(),
		"web.max_body_bytes":      strconv.FormatInt(defaults.MaxBodyBytes, 10),
	} {
		declared, ok := schema.Lookup(key)
		if !ok {
			t.Errorf("%s is not declared by the web mod", key)
			continue
		}
		if parsed, err := time.ParseDuration(declared.Default); err == nil {
			declared.Default = parsed.String()
		}
		if declared.Default != want || declared.Min == "" {
			t.Errorf("%s: default %q min %q; want default %q (httpserver's) and a lower bound", key, declared.Default, declared.Min, want)
		}
	}

	echo := func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if !webroute.DecodeJSON(w, r, &request) {
			return
		}
		webroute.WriteResult(w, http.StatusOK, request, nil)
	}
	mod := startWebMod(t, map[string]any{"web.max_body_bytes": 16, "web.read_header_timeout": "200ms"}, routeFuncs{"POST /echo": echo})
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post("http://"+mod.Addr()+"/echo", "application/json", strings.NewReader(`{"name":"a fairly long value"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("POST a 31-byte body with web.max_body_bytes=16 = %d %s; want 413", resp.StatusCode, body)
	}

	// 只发半个请求头：read_header_timeout 到期后服务端关闭连接（缺省 5s 时 2s 内不会关）。
	conn, err := net.Dial("tcp", mod.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "GET /echo HTTP/1.1\r\nHost: x\r\n"); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	started := time.Now()
	_, readErr := io.ReadAll(conn)
	var netErr net.Error
	if errors.As(readErr, &netErr) && netErr.Timeout() {
		t.Fatalf("connection with an unfinished header still open after %s; want it closed by web.read_header_timeout=200ms", time.Since(started).Round(time.Millisecond))
	}
}

// panic 的路由回 500、记一条带路由模板（不是原始路径）的日志，监听照常服务下一个请求。
func TestWebModRecoversPanickingRouteAndLogsTheTemplate(t *testing.T) {
	var logs syncBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	mod := startWebMod(t, nil, routeFuncs{
		"GET /items/{id}": func(http.ResponseWriter, *http.Request) { panic("boom in item handler") },
		"GET /ping":       func(w http.ResponseWriter, _ *http.Request) { webroute.WriteResult(w, http.StatusOK, "pong", nil) },
	})
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + mod.Addr() + "/items/42")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("panicking route = %d; want 500", resp.StatusCode)
	}
	if text := logs.String(); !strings.Contains(text, "route=/items/{id}") || !strings.Contains(text, "boom in item handler") {
		t.Fatalf("panic log does not name the route template /items/{id}; log:\n%s", text)
	}
	resp, err = client.Get("http://" + mod.Addr() + "/ping")
	if err != nil {
		t.Fatalf("GET /ping after a panic: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ping after a panic = %d", resp.StatusCode)
	}
}

// 路由级指标按路由模板计（不是原始路径）：请求数按状态码类别分，延迟进直方图；没匹配到路由的请求归 _unmatched，
// 标准以外的 method 归 _other，标签基数有界。
func TestWebModRecordsPerRouteMetricsByTemplate(t *testing.T) {
	previous := metrics.DefaultRegistry()
	t.Cleanup(func() { metrics.SetDefaultRegistry(previous) })

	notEnough := errcode.Define(100702, "web_test_rejected", "rejected")
	mod := startWebMod(t, nil, routeFuncs{
		"GET /items/{id}": func(w http.ResponseWriter, r *http.Request) {
			var err error
			if r.PathValue("id") == "bad" || strings.HasSuffix(r.URL.Path, "/bad") {
				err = notEnough
			}
			webroute.WriteResult(w, http.StatusOK, "item", err)
		},
	})
	registry := metrics.DefaultRegistry() // app.NewRegistry 换上的注册表，ops /metrics 导出的就是它
	client := &http.Client{Timeout: 5 * time.Second}
	for _, path := range []string{"/items/1", "/items/2", "/items/bad", "/nowhere/7"} {
		resp, err := client.Get("http://" + mod.Addr() + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	request, _ := http.NewRequest("BREW", "http://"+mod.Addr()+"/nowhere/8", nil)
	if resp, err := client.Do(request); err == nil {
		resp.Body.Close()
	}

	counts := map[string]int64{}
	for _, metric := range registry.Snapshot() {
		if metric.Name == metricRequests {
			counts[metric.Labels["method"]+" "+metric.Labels["route"]+" "+metric.Labels["class"]] += metric.Value
		}
		for _, value := range metric.Labels {
			if strings.Contains(value, "/items/1") || strings.Contains(value, "/nowhere") || value == "BREW" {
				t.Errorf("metric %s carries a raw path or method label %q", metric.Name, value)
			}
		}
	}
	want := map[string]int64{
		"GET /items/{id} 2xx":   2,
		"GET /items/{id} 4xx":   1,
		"GET _unmatched 4xx":    1,
		"_other _unmatched 4xx": 1,
	}
	for key, n := range want {
		if counts[key] != n {
			t.Errorf("%s{%s} = %d, want %d (all: %v)", metricRequests, key, counts[key], n, counts)
		}
	}
	if got := registry.HistogramCount(metricRequestDuration, metrics.Labels{"method": "GET", "route": "/items/{id}"}); got != 3 {
		t.Errorf("%s{GET /items/{id}} count = %d, want 3", metricRequestDuration, got)
	}
}

// syncBuffer 是可并发写的日志缓冲（handler 在服务协程里写，测试协程读）。
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
