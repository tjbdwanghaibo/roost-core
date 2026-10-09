package ops

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/admin"

	"github.com/tjbdwanghaibo/roost-core/infra/base/clock"
	"github.com/tjbdwanghaibo/roost-core/infra/network/httpserver"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/health"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

const opsMaxJSONBodyBytes int64 = 1 << 20

// defaultAdminTimeout 是没经 Init 直接装配的 OpsMod 交给命令的期限，与 ops.admin_timeout 声明的缺省相同
// （TestOpsAdminTimeoutDefaultMatchesTheDeclaration）。adminWriteMargin 是 HTTP 写超时比它多出的部分：配合 ctx 的命令到期返回之后，回复还来得及写出去。
const (
	defaultAdminTimeout = 10 * time.Second
	adminWriteMargin    = 5 * time.Second
	defaultWriteTimeout = 15 * time.Second // 与 httpserver 的默认写超时相同
)

type Server struct {
	enabled      bool
	addr         string
	adminEnabled bool
	adminToken   string
	adminTimeout time.Duration
	sid          int32
	service      string
	health       *health.Registry
	metrics      *metrics.Registry
	commands     *admin.Registry
	serverMu     sync.Mutex
	server       *http.Server
	boundAddr    string // server 实际监听的地址（ops.addr 写端口 0 时由系统分配），serverMu 保护
	stats        func() (any, error)
	ready        atomic.Bool
	readyMsg     atomic.Value
}

// Config 是运行参数；配置读取、进程能力查找和就绪生命周期接线属于 Wiring。
type Config struct {
	Enabled      bool
	Addr         string
	AdminEnabled bool
	AdminToken   string
	AdminTimeout time.Duration
	Service      string
	SID          int32
}
type Dependencies struct {
	Health   *health.Registry
	Metrics  *metrics.Registry
	Commands *admin.Registry
	Stats    func() (any, error)
}

var ErrStatsUnavailable = errors.New("stats unavailable: no registry")
var ErrStatsNotFound = errors.New("stats unavailable: statslog mod is not assembled in this process")

func New() *Server { return &Server{addr: "127.0.0.1:9100"} }

// Configure 只能在启动前调用；handler 与监听共用这一份运行参数。
func (m *Server) Configure(cfg Config) error {
	if cfg.AdminTimeout < 0 {
		return errors.New("ops: admin timeout must be positive")
	}
	m.serverMu.Lock()
	defer m.serverMu.Unlock()
	if m.server != nil {
		return errors.New("ops: configure after start")
	}
	m.enabled, m.addr = cfg.Enabled, cfg.Addr
	if m.addr == "" {
		m.addr = "127.0.0.1:9100"
	}
	m.adminEnabled, m.adminToken = cfg.AdminEnabled, cfg.AdminToken
	m.adminTimeout, m.service, m.sid = cfg.AdminTimeout, cfg.Service, cfg.SID
	return nil
}
func (m *Server) Connect(deps Dependencies) error {
	m.serverMu.Lock()
	defer m.serverMu.Unlock()
	if m.server != nil {
		return errors.New("ops: connect after start")
	}
	m.health = deps.Health
	m.metrics = deps.Metrics
	m.commands = deps.Commands
	m.stats = deps.Stats
	return nil
}
func (m *Server) Start() error {
	if !m.enabled {
		return nil
	}
	m.serverMu.Lock()
	defer m.serverMu.Unlock()
	if m.server != nil {
		return errors.New("ops: server already started or awaiting shutdown")
	}
	engine := m.Handler()
	// 写超时比命令期限多出 adminWriteMargin：配合 ctx 的命令到期返回后，504 回复还写得出去（N02 O1）。
	writeTimeout := max(defaultWriteTimeout, m.commandTimeout()+adminWriteMargin)
	server := httpserver.NewServer(m.addr, engine,
		httpserver.WithMaxBodyBytes(opsMaxJSONBodyBytes),
		httpserver.WithTimeouts(0, 0, writeTimeout, 0))
	// 在 Start 里同步 bind（RR-20261005-NC-230）：Start 返回 nil 就表示探针与运维端点已经在监听。
	// 端口被占用时启动失败，而不是让进程在没有 /healthz、/readyz 的情况下继续跑——同机部署的健康检查
	// 那时探到的是占着端口的另一个进程。
	listener, err := net.Listen("tcp", m.addr)
	if err != nil {
		return fmt.Errorf("ops: listen on ops.addr %s: %w", m.addr, err)
	}
	m.server = server
	m.boundAddr = listener.Addr().String()
	go func() {
		// Serve 在 Shutdown 之后返回 ErrServerClosed，并关闭 listener（Shutdown 早于 Serve 登记 listener 时
		// 也由 Serve 关闭）。
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("ops: http server failed", "addr", m.boundAddr, "err", err)
		}
	}()
	slog.Info("ops: serving", "addr", m.boundAddr, "admin_enabled", m.adminEnabled, "admin_timeout", m.commandTimeout())
	return nil
}

// ListenAddr 返回 server 实际监听的地址；没有在跑的 server 时为空。
func (m *Server) ListenAddr() string {
	m.serverMu.Lock()
	defer m.serverMu.Unlock()
	if m.server == nil {
		return ""
	}
	return m.boundAddr
}

// commandTimeout 是交给 admin 命令的期限；没经 Init 直接装配的 Server 用默认值。
func (m *Server) commandTimeout() time.Duration {
	if m.adminTimeout > 0 {
		return m.adminTimeout
	}
	return defaultAdminTimeout
}

func (m *Server) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.StopWithContext(ctx); err != nil {
		slog.Warn("ops: shutdown failed", "err", err)
	}
}

func (m *Server) StopWithContext(ctx context.Context) error {
	m.serverMu.Lock()
	server := m.server
	m.serverMu.Unlock()
	if server == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// RR-20261004-NC-04：取消/超时不等于 handler 已排空，错误时保留同一 server 供重试。
	// Shutdown 在锁外等待，不能阻塞其他调用取得自己的 context/关闭结果。
	if err := server.Shutdown(ctx); err != nil {
		return err
	}
	m.serverMu.Lock()
	if m.server == server {
		m.server = nil
	}
	m.serverMu.Unlock()
	return nil
}

func (m *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"service": m.service,
		"sid":     m.sid,
	})
}

// handleReady 是 readiness：就绪位为真且没有 checker 为 Fail 时 200，否则 503。
//
// Degraded 算就绪（维护者决定 D1，2026-10-06）：有降级项时仍返回 200、`ok` 为 true，
// 用 `degraded: true` 与 `degraded_dependencies`（每项的 name / status / message / error）标出，
// `dependencies` 照样列出全部 checker。之前 Degraded 与 Fail 一样 503，k8s 会把单副本服务唯一的
// endpoint 摘掉，entitysync 在 80% 容量边界上还会来回翻转。/healthz 不受影响。
func (m *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	deps := health.Snapshot{OK: true}
	if m.health != nil {
		deps = m.health.Snapshot(r.Context())
	}
	ok := m.ready.Load() && deps.OK
	status := http.StatusOK
	if !ok {
		status = http.StatusServiceUnavailable
	}
	degraded := deps.DegradedResults()
	if degraded == nil {
		degraded = []health.Result{}
	}
	writeJSON(w, status, map[string]any{
		"ok":                    ok,
		"degraded":              deps.Degraded,
		"degraded_dependencies": degraded,
		"service":               m.service,
		"sid":                   m.sid,
		"message":               m.readyMessage(),
		"server_time_ms":        clock.UnixMilli(),
		"metrics":               m.metricCount(),
		"dependencies":          deps.Results,
	})
}

func (m *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var snapshot []metrics.Metric
	if m.metrics != nil {
		snapshot = m.metrics.Snapshot()
	}
	_, _ = w.Write(metrics.PrometheusText(snapshot))
}

// statsCollector is what the statslog Mod publishes: one observation of the
// process — goroutines, heap, entity counts by category and kind, Nest
// dispatcher figures — as a JSON-marshalable record.
type statsCollector interface{ CollectStats() any }

// handleStats serves the current runtime observation. It is the JSON twin of
// the statslog JSONL line and of the gauges on /metrics, for an operator who
// wants to look at one process right now rather than at a dashboard.
func (m *Server) handleStats(w http.ResponseWriter, _ *http.Request) {
	if m.stats == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "stats unavailable: no registry"})
		return
	}
	value, err := m.stats()
	if err != nil {
		status := http.StatusNotFound
		if errors.Is(err, ErrStatsUnavailable) {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (m *Server) handleAdminCommands(w http.ResponseWriter, r *http.Request) {
	if !m.adminEnabled {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "admin disabled"})
		return
	}
	if !m.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
		return
	}
	if m.commands == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "admin registry unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"commands": m.commands.Names(),
	})
}

func (m *Server) handleAdminExecute(w http.ResponseWriter, r *http.Request) {
	if !m.adminEnabled {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "admin disabled"})
		return
	}
	if !m.authorized(r) {
		slog.Warn("admin execute refused", "outcome", "unauthorized", "remote_addr", r.RemoteAddr)
		metrics.IncCounter("admin.http_refused.total", metrics.Labels{"reason": "unauthorized"}, 1)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	var cmd admin.Command
	raw, ok := httpserver.ReadBody(w, r)
	if !ok {
		return
	}
	if err := json.Unmarshal(raw, &cmd); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if cmd.Source == "" {
		cmd.Source = fmt.Sprintf("ops:%s:%d", m.service, m.sid)
	}
	if m.commands == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "admin registry unavailable"})
		return
	}
	if cmd.TraceID == "" {
		cmd.TraceID = rand.Text()
	}
	slog.Info("admin execute accepted", "remote_addr", r.RemoteAddr, "command", cmd.Name, "trace_id", cmd.TraceID)
	// 命令在 ops.admin_timeout 内执行（N02 O1）。到期时命令可能已经做了一部分：回 504 并写明结果未知，
	// 运维按 trace_id 核对后再决定是否重试。不配合 ctx 的命令 Ops 杀不掉，它跑过写超时后回复写不出去，
	// 客户端看到传输错误——同样是结果未知，不等于没执行。
	ctx, cancel := context.WithTimeout(r.Context(), m.commandTimeout())
	defer cancel()
	result, err := m.commands.Execute(ctx, cmd)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			result.Message = fmt.Sprintf("command did not finish within ops.admin_timeout (%s); its effects are unknown: %v", m.commandTimeout(), err)
			writeJSON(w, http.StatusGatewayTimeout, result)
			return
		}
		status := http.StatusInternalServerError
		if errors.Is(err, admin.ErrCommandNotFound) {
			status = http.StatusNotFound
		} else if errors.Is(err, admin.ErrCommandInvalid) {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, result)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (m *Server) metricCount() int {
	if m == nil || m.metrics == nil {
		return 0
	}
	return m.metrics.SeriesCount()
}

// authorized compares the presented admin token in constant time. This
// endpoint can execute every registered admin command (plugin loading, DLQ
// replay, load-test control), so the token is a credential of the same class
// as the session tokens and payload signatures in roost-core's security
// package — and those are compared with hmac.Equal. A plain `==` short-circuits
// at the first differing byte, which is exactly the signal a timing attack
// needs.
//
// An empty configured token authorizes nothing. Init already refuses
// admin_enabled without a token, so this is defence in depth for an Server
// assembled directly rather than through Init.
func (m *Server) authorized(r *http.Request) bool {
	if m.adminToken == "" {
		return false
	}
	if secretEqual(r.Header.Get("X-Admin-Token"), m.adminToken) {
		return true
	}
	return secretEqual(bearerToken(r.Header.Get("Authorization")), m.adminToken)
}

// bearerToken extracts the credential from an Authorization header. The scheme
// is case-insensitive per RFC 7235, so `bearer x` must work as well as
// `Bearer x`.
//
// 没有 `Bearer ` 前缀的 Authorization 不交出任何凭据（维护者第十二轮决定，N01b 观察 O-P1）：
// 之前原样返回整个头，`Authorization: <token>` 也能通过。不构成绕过（仍要知道 token），但
// Authorization 头按 RFC 7235 必须带 scheme，别的 scheme（Basic 等）更不能被当成 admin token。
// 不带 scheme 的客户端改用 `Authorization: Bearer <token>` 或 `X-Admin-Token: <token>`。
func bearerToken(header string) string {
	header = strings.TrimSpace(header)
	if len(header) >= 7 && strings.EqualFold(header[:7], "bearer ") {
		return strings.TrimSpace(header[7:])
	}
	return ""
}

// secretEqual reports whether presented equals want without leaking where
// they first differ. Length is still observable — ConstantTimeCompare returns
// early on a length mismatch, as does hmac.Equal — which is the accepted
// trade-off: the token's length is not the secret, its contents are.
func secretEqual(presented, want string) bool {
	return subtle.ConstantTimeCompare([]byte(presented), []byte(want)) == 1
}

func (m *Server) SetReady(ok bool, msg string) {
	m.ready.Store(ok)
	m.readyMsg.Store(msg)
}

func (m *Server) readyMessage() string {
	if v := m.readyMsg.Load(); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return "starting"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	httpserver.JSON(w, status, v)
}

// Handler 使用与真实监听相同的路由和 body 限制，可交给其他 HTTP 入口。
func (m *Server) Handler() *httpserver.Engine {
	engine := httpserver.NewEngine(httpserver.WithMaxBodyBytes(opsMaxJSONBodyBytes))
	engine.Get("/healthz", m.handleHealth)
	engine.Get("/readyz", m.handleReady)
	engine.Get("/metrics", m.handleMetrics)
	engine.Get("/statsz", m.handleStats)
	engine.Get("/admin/commands", m.handleAdminCommands)
	engine.Post("/admin/execute", m.handleAdminExecute)
	return engine
}
