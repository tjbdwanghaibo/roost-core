package ops

import (
	"context"
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

	"github.com/tjbdwanghaibo/roost-core/admin"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/clock"
	"github.com/tjbdwanghaibo/roost-core/health"
	"github.com/tjbdwanghaibo/roost-core/httpserver"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	"github.com/tjbdwanghaibo/roost-core/lifecycle"
	"github.com/tjbdwanghaibo/roost-core/metrics"

	"github.com/spf13/viper"
)

const opsMaxJSONBodyBytes int64 = 1 << 20

// defaultAdminTimeout 是 /admin/execute 交给命令的 ctx 期限（ops.admin_timeout 未配置时）。
// adminWriteMargin 是 HTTP 写超时比它多出的部分：配合 ctx 的命令到期返回之后，回复还来得及写出去。
const (
	defaultAdminTimeout = 10 * time.Second
	adminWriteMargin    = 5 * time.Second
	defaultWriteTimeout = 15 * time.Second // 与 httpserver 的默认写超时相同
)

type OpsMod struct {
	enabled       bool
	addr          string
	adminEnabled  bool
	adminToken    string
	allowDevToken bool
	adminTimeout  time.Duration
	sid           int32
	service       string
	health        *health.Registry
	metrics       *metrics.Registry
	commands      *admin.Registry
	lifecycle     *lifecycle.Registry
	serverMu      sync.Mutex
	server        *http.Server
	boundAddr     string // server 实际监听的地址（ops.addr 写端口 0 时由系统分配），serverMu 保护
	registry      *app.Registry
	ready         atomic.Bool
	readyMsg      atomic.Value
}

func NewOpsMod() *OpsMod {
	return &OpsMod{}
}

func (m *OpsMod) Name() app.ModName { return mods.ModOps }

func (m *OpsMod) Init(cfg *viper.Viper) error {
	read := app.NewConfigReader(cfg) // 严格读取（维护者决定 A4）
	m.enabled = read.Bool("ops.enabled")
	m.addr = cfg.GetString("ops.addr")
	if m.addr == "" {
		m.addr = "127.0.0.1:9100"
	}
	m.adminEnabled = read.Bool("ops.admin_enabled")
	m.adminToken = cfg.GetString("ops.admin_token")
	m.allowDevToken = read.Bool("ops.allow_dev_token")
	m.adminTimeout = defaultAdminTimeout
	if timeout := read.Duration("ops.admin_timeout"); timeout > 0 {
		m.adminTimeout = timeout
	} else if cfg.IsSet("ops.admin_timeout") && read.Err() == nil {
		return fmt.Errorf("ops: ops.admin_timeout must be positive, got %s", cfg.GetString("ops.admin_timeout"))
	}
	m.sid = cfg.GetInt32("sid")
	m.service = cfg.GetString("server_type")
	if err := read.Err(); err != nil {
		return fmt.Errorf("ops: %w", err)
	}
	if m.adminEnabled {
		if m.adminToken == "" {
			return errors.New("ops: admin_enabled requires admin_token")
		}
		if strings.HasPrefix(m.adminToken, "dev-") && !m.allowDevToken {
			return errors.New("ops: dev admin token is not allowed unless ops.allow_dev_token=true")
		}
	}
	return nil
}

func (m *OpsMod) Provide(r *app.Registry) error {
	if r == nil {
		return fmt.Errorf("ops: app registry is nil")
	}
	m.registry = r
	var ok bool
	if m.health, ok = app.Lookup[*health.Registry](r, mods.ModHealth); !ok || m.health == nil {
		return fmt.Errorf("ops: capability %q not found", mods.ModHealth)
	}
	if m.metrics, ok = app.Lookup[*metrics.Registry](r, mods.ModMetrics); !ok || m.metrics == nil {
		return fmt.Errorf("ops: capability %q not found", mods.ModMetrics)
	}
	if m.commands, ok = app.Lookup[*admin.Registry](r, mods.ModAdmin); !ok || m.commands == nil {
		return fmt.Errorf("ops: capability %q not found", mods.ModAdmin)
	}
	if m.lifecycle, ok = app.Lookup[*lifecycle.Registry](r, mods.ModLifecycle); !ok || m.lifecycle == nil {
		return fmt.Errorf("ops: capability %q not found", mods.ModLifecycle)
	}
	if err := m.lifecycle.Register(lifecycle.Hook{
		Name:  "ops.ready.service_started",
		Phase: lifecycle.PhaseServiceStarted,
		Handler: func(context.Context, lifecycle.Event) error {
			m.setReady(true, "service started")
			return nil
		},
	}); err != nil {
		return err
	}
	if err := m.lifecycle.Register(lifecycle.Hook{
		Name:  "ops.ready.service_stopping",
		Phase: lifecycle.PhaseServiceStopping,
		Handler: func(context.Context, lifecycle.Event) error {
			m.setReady(false, "service stopping")
			return nil
		},
	}); err != nil {
		return err
	}
	return r.Register(mods.ModOps, m)
}

func (m *OpsMod) Start() error {
	if !m.enabled {
		return nil
	}
	m.serverMu.Lock()
	defer m.serverMu.Unlock()
	if m.server != nil {
		return errors.New("ops: server already started or awaiting shutdown")
	}
	engine := httpserver.NewEngine(httpserver.WithMaxBodyBytes(opsMaxJSONBodyBytes))
	engine.Get("/healthz", m.handleHealth)
	engine.Get("/readyz", m.handleReady)
	engine.Get("/metrics", m.handleMetrics)
	engine.Get("/statsz", m.handleStats)
	engine.Get("/admin/commands", m.handleAdminCommands)
	engine.Post("/admin/execute", m.handleAdminExecute)
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

// listenAddr 返回 server 实际监听的地址；没有在跑的 server 时为空。
func (m *OpsMod) listenAddr() string {
	m.serverMu.Lock()
	defer m.serverMu.Unlock()
	if m.server == nil {
		return ""
	}
	return m.boundAddr
}

// commandTimeout 是交给 admin 命令的期限；没经 Init 直接装配的 OpsMod 用默认值。
func (m *OpsMod) commandTimeout() time.Duration {
	if m.adminTimeout > 0 {
		return m.adminTimeout
	}
	return defaultAdminTimeout
}

func (m *OpsMod) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.StopWithContext(ctx); err != nil {
		slog.Warn("ops: shutdown failed", "err", err)
	}
}

func (m *OpsMod) StopWithContext(ctx context.Context) error {
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

func (m *OpsMod) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"service": m.service,
		"sid":     m.sid,
	})
}

func (m *OpsMod) handleReady(w http.ResponseWriter, r *http.Request) {
	deps := health.Snapshot{OK: true}
	if m.health != nil {
		deps = m.health.Snapshot(r.Context())
	}
	ok := m.ready.Load() && deps.OK
	status := http.StatusOK
	if !ok {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{
		"ok":             ok,
		"service":        m.service,
		"sid":            m.sid,
		"message":        m.readyMessage(),
		"server_time_ms": clock.UnixMilli(),
		"metrics":        m.metricCount(),
		"dependencies":   deps.Results,
	})
}

func (m *OpsMod) handleMetrics(w http.ResponseWriter, _ *http.Request) {
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
func (m *OpsMod) handleStats(w http.ResponseWriter, _ *http.Request) {
	if m.registry == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "stats unavailable: no registry"})
		return
	}
	collector, ok := app.Lookup[statsCollector](m.registry, mods.ModStatsLog)
	if !ok || collector == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "stats unavailable: statslog mod is not assembled in this process"})
		return
	}
	writeJSON(w, http.StatusOK, collector.CollectStats())
}

func (m *OpsMod) handleAdminCommands(w http.ResponseWriter, r *http.Request) {
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

func (m *OpsMod) handleAdminExecute(w http.ResponseWriter, r *http.Request) {
	if !m.adminEnabled {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "admin disabled"})
		return
	}
	if !m.authorized(r) {
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
		writeJSON(w, http.StatusBadRequest, result)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (m *OpsMod) metricCount() int {
	if m == nil || m.metrics == nil {
		return 0
	}
	return len(m.metrics.Snapshot())
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
// admin_enabled without a token, so this is defence in depth for an OpsMod
// assembled directly rather than through Init.
func (m *OpsMod) authorized(r *http.Request) bool {
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
func bearerToken(header string) string {
	header = strings.TrimSpace(header)
	if len(header) >= 7 && strings.EqualFold(header[:7], "bearer ") {
		return strings.TrimSpace(header[7:])
	}
	return header
}

// secretEqual reports whether presented equals want without leaking where
// they first differ. Length is still observable — ConstantTimeCompare returns
// early on a length mismatch, as does hmac.Equal — which is the accepted
// trade-off: the token's length is not the secret, its contents are.
func secretEqual(presented, want string) bool {
	return subtle.ConstantTimeCompare([]byte(presented), []byte(want)) == 1
}

func (m *OpsMod) setReady(ok bool, msg string) {
	m.ready.Store(ok)
	m.readyMsg.Store(msg)
}

func (m *OpsMod) readyMessage() string {
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

var _ app.Mod = (*OpsMod)(nil)
