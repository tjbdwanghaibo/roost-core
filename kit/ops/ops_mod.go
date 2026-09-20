package ops

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/admin"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/clock"
	"github.com/tjbdwanghaibo/roost-core/health"
	"github.com/tjbdwanghaibo/roost-core/httpserver"
	"github.com/tjbdwanghaibo/roost-core/lifecycle"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	"github.com/tjbdwanghaibo/roost-kit/mods"

	"github.com/spf13/viper"
)

const opsMaxJSONBodyBytes int64 = 1 << 20

type OpsMod struct {
	enabled       bool
	addr          string
	adminEnabled  bool
	adminToken    string
	allowDevToken bool
	sid           int32
	service       string
	health        *health.Registry
	metrics       *metrics.Registry
	commands      *admin.Registry
	lifecycle     *lifecycle.Registry
	server        *http.Server
	registry      *app.Registry
	ready         atomic.Bool
	readyMsg      atomic.Value
}

func NewOpsMod() *OpsMod {
	return &OpsMod{}
}

func (m *OpsMod) Name() app.ModName { return mods.ModOps }

func (m *OpsMod) Init(cfg *viper.Viper) error {
	m.enabled = cfg.GetBool("ops.enabled")
	m.addr = cfg.GetString("ops.addr")
	if m.addr == "" {
		m.addr = "127.0.0.1:9100"
	}
	m.adminEnabled = cfg.GetBool("ops.admin_enabled")
	m.adminToken = cfg.GetString("ops.admin_token")
	m.allowDevToken = cfg.GetBool("ops.allow_dev_token")
	m.sid = cfg.GetInt32("sid")
	m.service = cfg.GetString("server_type")
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
	engine := httpserver.NewEngine(httpserver.WithMaxBodyBytes(opsMaxJSONBodyBytes))
	engine.Get("/healthz", m.handleHealth)
	engine.Get("/readyz", m.handleReady)
	engine.Get("/metrics", m.handleMetrics)
	engine.Get("/statsz", m.handleStats)
	engine.Get("/admin/commands", m.handleAdminCommands)
	engine.Post("/admin/execute", m.handleAdminExecute)
	m.server = httpserver.NewServer(m.addr, engine, httpserver.WithMaxBodyBytes(opsMaxJSONBodyBytes))
	go func() {
		if err := m.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("ops: http server failed", "addr", m.addr, "err", err)
		}
	}()
	slog.Info("ops: serving", "addr", m.addr, "admin_enabled", m.adminEnabled)
	return nil
}

func (m *OpsMod) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.StopWithContext(ctx); err != nil {
		slog.Warn("ops: shutdown failed", "err", err)
	}
}

func (m *OpsMod) StopWithContext(ctx context.Context) error {
	if m.server == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	err := m.server.Shutdown(ctx)
	m.server = nil
	return err
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
	result, err := m.commands.Execute(r.Context(), cmd)
	if err != nil {
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
