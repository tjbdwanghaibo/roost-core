// Package loadtest is the robot load-test control plane: a single-active-run
// state machine with profile selection, ring history, admin commands, SLO
// thresholds and an in-process report. Ported from the cube robot service;
// thresholds and the report generator replace the external grep-the-JSON
// shell gate.
package loadtest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/cube-core/obs"
	"github.com/tjbdwanghaibo/cube-core/robot/runner"
)

const DefaultHistoryLimit = 20

type RunState string

type StopReason string

const (
	StateRunning  RunState = "running"
	StateStopping RunState = "stopping"
	StateFinished RunState = "finished"
	StateFailed   RunState = "failed"

	StopReasonCompleted StopReason = "completed"
	StopReasonDuration  StopReason = "duration"
	StopReasonManual    StopReason = "manual"
	StopReasonShutdown  StopReason = "shutdown"
	StopReasonError     StopReason = "error"
	StopReasonThreshold StopReason = "threshold"
)

var (
	ErrProfileRequired    = errors.New("robot loadtest: profile required")
	ErrProfileNotFound    = errors.New("robot loadtest: profile not found")
	ErrRunActive          = errors.New("robot loadtest: run already active")
	ErrRunIDMismatch      = errors.New("robot loadtest: run id mismatch")
	ErrAdminStartDisabled = errors.New("robot loadtest: admin start disabled")
)

// Runner is the seam the manager drives; runner.Runner satisfies it and
// tests substitute fakes.
type Runner interface {
	Run(context.Context) error
	Stop(context.Context) error
	Stats() runner.Stats
}

// RunnerFactory builds the runner for one run. runID is unique per run —
// the default factory folds it into the metric labels so a run's latency
// histogram is never contaminated by earlier runs of the same profile.
type RunnerFactory func(runID string, profile string, cfg Profile) Runner

// Threshold is one SLO assertion evaluated when a run ends; any violation
// marks the run failed with StopReasonThreshold — the programmatic gate CI
// consumes instead of grepping JSON.
type Threshold struct {
	// Metric selects what to assert:
	//   error_rate        Failure / (Success+Failure), 0..1
	//   p50/p90/p95/p99   quantile of the scenario cost histogram (ok runs)
	Metric string `json:"metric"`
	// Max is the inclusive upper bound: seconds for quantiles (e.g. 0.2 =
	// 200ms), a ratio for error_rate.
	Max float64 `json:"max"`
}

// Profile is one named load-test configuration.
type Profile struct {
	Run        runner.Config
	Thresholds []Threshold
}

type Config struct {
	ActiveProfile   string
	AllowAdminStart bool
	HistoryLimit    int
	Profiles        map[string]Profile
	RunnerFactory   RunnerFactory
	// RunnerOptions are applied by the default factory to every run —
	// the place to hand one shared action/scenario/protocol registration
	// set to all profiles without writing a custom factory.
	RunnerOptions []runner.Option
	// Quantile reads a scenario-cost quantile for threshold evaluation; the
	// default reads obs' robot.runner.scenario.cost histogram for this
	// run's scenario with result=ok.
	Quantile func(runID string, profile string, scenarioName string, q float64) time.Duration
}

type StartRequest struct {
	Profile string `json:"profile,omitempty"`
	RunID   string `json:"run_id,omitempty"`
}

type StopRequest struct {
	RunID  string `json:"run_id,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type StatusRequest struct {
	IncludeHistory bool `json:"include_history,omitempty"`
}

type HistoryRequest struct {
	Limit int `json:"limit,omitempty"`
}

type ReportRequest struct {
	RunID string `json:"run_id,omitempty"`
}

type ProfilesResult struct {
	ActiveProfile string           `json:"active_profile,omitempty"`
	Profiles      []ProfileSummary `json:"profiles"`
}

type ProfileSummary struct {
	Name       string      `json:"name"`
	Executor   string      `json:"executor"`
	Count      int         `json:"count"`
	Rate       float64     `json:"rate,omitempty"`
	Scenario   string      `json:"scenario"`
	DurationMS int64       `json:"duration_ms"`
	Thresholds []Threshold `json:"thresholds,omitempty"`
	Endpoint   string      `json:"endpoint"`
	Transport  string      `json:"transport"`
}

type StatusResult struct {
	Active  *RunSnapshot  `json:"active,omitempty"`
	History []RunSnapshot `json:"history,omitempty"`
}

type HistoryResult struct {
	Runs []RunSnapshot `json:"runs"`
}

type StopResult struct {
	Stopped bool       `json:"stopped"`
	RunID   string     `json:"run_id,omitempty"`
	Reason  StopReason `json:"reason,omitempty"`
}

type ThresholdResult struct {
	Threshold
	Actual   float64 `json:"actual"`
	Violated bool    `json:"violated"`
}

type RunSnapshot struct {
	RunID       string            `json:"run_id"`
	Profile     string            `json:"profile"`
	State       RunState          `json:"state"`
	StopReason  StopReason        `json:"stop_reason,omitempty"`
	StartedAtMS int64             `json:"started_at_ms"`
	EndedAtMS   int64             `json:"ended_at_ms,omitempty"`
	Count       int               `json:"count"`
	Scenario    string            `json:"scenario"`
	Stats       StatsSnapshot     `json:"stats"`
	Thresholds  []ThresholdResult `json:"thresholds,omitempty"`
	QuantilesMS map[string]int64  `json:"quantiles_ms,omitempty"`
	Error       string            `json:"error,omitempty"`
}

type StatsSnapshot struct {
	Started     int64   `json:"started"`
	Online      int64   `json:"online"`
	Success     int64   `json:"success"`
	Failure     int64   `json:"failure"`
	Canceled    int64   `json:"canceled"`
	FailureRate float64 `json:"failure_rate"`
}

type Manager struct {
	cfg      Config
	factory  RunnerFactory
	quantile func(string, string, string, float64) time.Duration

	mu      sync.Mutex
	rootCtx context.Context
	active  *runRecord
	history []RunSnapshot
	seq     atomic.Int64
}

type runRecord struct {
	RunID       string
	Profile     string
	Config      Profile
	State       RunState
	StopReason  StopReason
	StartedAtMS int64
	EndedAtMS   int64
	Error       string
	Thresholds  []ThresholdResult
	Quantiles   map[string]int64

	ctx    context.Context
	cancel context.CancelFunc
	runner Runner
	done   chan struct{}
}

func New(cfg Config) *Manager {
	if cfg.HistoryLimit <= 0 {
		cfg.HistoryLimit = DefaultHistoryLimit
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]Profile{}
	}
	profiles := make(map[string]Profile, len(cfg.Profiles))
	for name, profile := range cfg.Profiles {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		profile.Run = profile.Run.Normalize()
		profiles[name] = profile
	}
	cfg.Profiles = profiles
	factory := cfg.RunnerFactory
	if factory == nil {
		shared := cfg.RunnerOptions
		factory = func(runID string, profile string, cfg Profile) Runner {
			opts := append([]runner.Option{runner.WithMetricLabels(obs.Labels{"profile": profile, "run": runID})}, shared...)
			return runner.New(cfg.Run, opts...)
		}
	}
	quantile := cfg.Quantile
	if quantile == nil {
		quantile = func(runID string, profile string, scenarioName string, q float64) time.Duration {
			return obs.HistogramQuantile("robot.runner.scenario.cost",
				obs.Labels{"profile": profile, "run": runID, "scenario": scenarioName, "result": "ok"}, q)
		}
	}
	return &Manager{cfg: cfg, factory: factory, quantile: quantile, rootCtx: context.Background(), history: make([]RunSnapshot, 0, cfg.HistoryLimit)}
}

// Serve installs the root context and auto-starts ActiveProfile when set,
// then blocks until ctx ends.
func (m *Manager) Serve(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	m.rootCtx = ctx
	m.mu.Unlock()
	if strings.TrimSpace(m.cfg.ActiveProfile) != "" {
		if _, err := m.Start(ctx, StartRequest{Profile: m.cfg.ActiveProfile}); err != nil {
			return err
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

func (m *Manager) Shutdown(ctx context.Context) error {
	if m == nil {
		return nil
	}
	_, err := m.Stop(ctx, StopRequest{Reason: string(StopReasonShutdown)})
	return err
}

func (m *Manager) Profiles() ProfilesResult {
	if m == nil {
		return ProfilesResult{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	names := m.profileNamesLocked()
	profiles := make([]ProfileSummary, 0, len(names))
	for _, name := range names {
		profile := m.cfg.Profiles[name]
		profiles = append(profiles, ProfileSummary{
			Name:       name,
			Executor:   string(profile.Run.Executor),
			Count:      profile.Run.Count,
			Rate:       profile.Run.Rate,
			Scenario:   profile.Run.Scenario,
			DurationMS: profile.Run.Duration.Milliseconds(),
			Thresholds: profile.Thresholds,
			Endpoint:   profile.Run.Transport.Endpoint,
			Transport:  profile.Run.Transport.Type,
		})
	}
	return ProfilesResult{ActiveProfile: m.cfg.ActiveProfile, Profiles: profiles}
}

func (m *Manager) Start(ctx context.Context, req StartRequest) (RunSnapshot, error) {
	if m == nil {
		return RunSnapshot{}, errors.New("robot loadtest: manager is nil")
	}
	return m.start(ctx, req)
}

func (m *Manager) StartAdmin(ctx context.Context, req StartRequest) (RunSnapshot, error) {
	if m == nil {
		return RunSnapshot{}, errors.New("robot loadtest: manager is nil")
	}
	if !m.cfg.AllowAdminStart {
		return RunSnapshot{}, ErrAdminStartDisabled
	}
	return m.start(ctx, req)
}

func (m *Manager) Stop(ctx context.Context, req StopRequest) (StopResult, error) {
	if m == nil {
		return StopResult{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	rec := m.active
	if rec == nil {
		m.mu.Unlock()
		return StopResult{Stopped: false}, nil
	}
	if req.RunID != "" && req.RunID != rec.RunID {
		m.mu.Unlock()
		return StopResult{}, fmt.Errorf("%w: active=%s requested=%s", ErrRunIDMismatch, rec.RunID, req.RunID)
	}
	reason := normalizeStopReason(req.Reason)
	if reason == "" {
		reason = StopReasonManual
	}
	if rec.State != StateStopping {
		rec.State = StateStopping
		rec.StopReason = reason
		rec.cancel()
	}
	runID := rec.RunID
	activeRunner := rec.runner
	done := rec.done
	m.mu.Unlock()

	if activeRunner != nil {
		if err := activeRunner.Stop(ctx); err != nil {
			return StopResult{Stopped: true, RunID: runID, Reason: reason}, err
		}
	}
	select {
	case <-done:
	case <-ctx.Done():
		return StopResult{Stopped: true, RunID: runID, Reason: reason}, ctx.Err()
	}
	return StopResult{Stopped: true, RunID: runID, Reason: reason}, nil
}

func (m *Manager) Status(req StatusRequest) StatusResult {
	if m == nil {
		return StatusResult{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var active *RunSnapshot
	if m.active != nil {
		snapshot := m.active.snapshot()
		active = &snapshot
	}
	result := StatusResult{Active: active}
	if req.IncludeHistory {
		result.History = m.historyLocked(0)
	}
	return result
}

func (m *Manager) History(req HistoryRequest) HistoryResult {
	if m == nil {
		return HistoryResult{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return HistoryResult{Runs: m.historyLocked(req.Limit)}
}

func (m *Manager) start(ctx context.Context, req StartRequest) (RunSnapshot, error) {
	profileName := strings.TrimSpace(req.Profile)
	if profileName == "" {
		profileName = strings.TrimSpace(m.cfg.ActiveProfile)
	}
	if profileName == "" {
		return RunSnapshot{}, ErrProfileRequired
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active != nil {
		return RunSnapshot{}, fmt.Errorf("%w: %s", ErrRunActive, m.active.RunID)
	}
	profile, ok := m.cfg.Profiles[profileName]
	if !ok {
		return RunSnapshot{}, fmt.Errorf("%w: %s available=%v", ErrProfileNotFound, profileName, m.profileNamesLocked())
	}
	rootCtx := m.rootCtx
	if rootCtx == nil {
		rootCtx = context.Background()
	}
	runCtx, cancel := context.WithCancel(rootCtx)
	runID := strings.TrimSpace(req.RunID)
	if runID == "" {
		runID = m.nextRunID(profileName)
	}
	rec := &runRecord{
		RunID:       runID,
		Profile:     profileName,
		Config:      profile,
		State:       StateRunning,
		StartedAtMS: time.Now().UnixMilli(),
		ctx:         runCtx,
		cancel:      cancel,
		runner:      m.factory(runID, profileName, profile),
		done:        make(chan struct{}),
	}
	m.active = rec
	obs.SetGauge("robot.loadtest.active", obs.Labels{"profile": profileName}, 1)
	slog.Info("robot loadtest: run start accepted",
		"run_id", rec.RunID, "profile", rec.Profile,
		"scenario", profile.Run.Scenario, "count", profile.Run.Count,
		"endpoint", profile.Run.Transport.Endpoint)
	go m.execute(rec)
	return rec.snapshot(), nil
}

func (m *Manager) execute(rec *runRecord) {
	start := time.Now()
	err := rec.runner.Run(rec.ctx)
	ended := time.Now()

	quantiles, thresholds := m.evaluate(rec)

	m.mu.Lock()
	rec.Quantiles = quantiles
	rec.Thresholds = thresholds
	violated := false
	for _, t := range thresholds {
		if t.Violated {
			violated = true
			break
		}
	}
	switch {
	case err != nil && rec.ctx.Err() == nil:
		rec.State = StateFailed
		rec.StopReason = StopReasonError
		rec.Error = err.Error()
	case violated:
		rec.State = StateFailed
		rec.StopReason = StopReasonThreshold
	default:
		rec.State = StateFinished
		if rec.StopReason == "" {
			rec.StopReason = stopReasonFromContext(rec.ctx)
		}
	}
	rec.EndedAtMS = ended.UnixMilli()
	snapshot := rec.snapshot()
	if m.active == rec {
		m.active = nil
	}
	m.appendHistoryLocked(snapshot)
	m.mu.Unlock()

	result := "ok"
	if snapshot.State == StateFailed {
		result = "error"
	}
	obs.SetGauge("robot.loadtest.active", obs.Labels{"profile": rec.Profile}, 0)
	obs.IncCounter("robot.loadtest.run.total", obs.Labels{"profile": rec.Profile, "result": result}, 1)
	obs.ObserveDuration("robot.loadtest.run.duration", obs.Labels{"profile": rec.Profile, "result": result}, ended.Sub(start))
	slog.Info("robot loadtest: run done",
		"run_id", rec.RunID, "profile", rec.Profile,
		"state", snapshot.State, "stop_reason", snapshot.StopReason,
		"started", snapshot.Stats.Started, "success", snapshot.Stats.Success,
		"failure", snapshot.Stats.Failure, "canceled", snapshot.Stats.Canceled,
		"elapsed", ended.Sub(start), "err", err)
	close(rec.done)
}

// evaluate computes the run's quantiles and threshold verdicts.
func (m *Manager) evaluate(rec *runRecord) (map[string]int64, []ThresholdResult) {
	scenarioName := rec.Config.Run.Scenario
	quantiles := map[string]int64{}
	for _, q := range []struct {
		name string
		v    float64
	}{{"p50", 0.50}, {"p90", 0.90}, {"p95", 0.95}, {"p99", 0.99}} {
		quantiles[q.name] = m.quantile(rec.RunID, rec.Profile, scenarioName, q.v).Milliseconds()
	}
	stats := statsSnapshot(rec.runner.Stats())
	results := make([]ThresholdResult, 0, len(rec.Config.Thresholds))
	for _, threshold := range rec.Config.Thresholds {
		actual := 0.0
		switch strings.ToLower(strings.TrimSpace(threshold.Metric)) {
		case "error_rate":
			actual = stats.FailureRate
		case "p50", "p90", "p95", "p99":
			actual = float64(quantiles[strings.ToLower(threshold.Metric)]) / 1000.0
		default:
			results = append(results, ThresholdResult{Threshold: threshold, Violated: true})
			continue
		}
		results = append(results, ThresholdResult{Threshold: threshold, Actual: actual, Violated: actual > threshold.Max})
	}
	return quantiles, results
}

func (m *Manager) historyLocked(limit int) []RunSnapshot {
	if limit <= 0 || limit > len(m.history) {
		limit = len(m.history)
	}
	out := make([]RunSnapshot, 0, limit)
	for i := len(m.history) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, m.history[i])
	}
	return out
}

func (m *Manager) appendHistoryLocked(snapshot RunSnapshot) {
	m.history = append(m.history, snapshot)
	if len(m.history) > m.cfg.HistoryLimit {
		m.history = append([]RunSnapshot(nil), m.history[len(m.history)-m.cfg.HistoryLimit:]...)
	}
}

func (m *Manager) profileNamesLocked() []string {
	names := make([]string, 0, len(m.cfg.Profiles))
	for name := range m.cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (m *Manager) nextRunID(profile string) string {
	seq := m.seq.Add(1)
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, strings.TrimSpace(profile))
	name = strings.Trim(name, "-")
	if name == "" {
		name = "profile"
	}
	return fmt.Sprintf("robot-%s-%s-%03d", name, time.Now().Format("20060102-150405"), seq)
}

func (r *runRecord) snapshot() RunSnapshot {
	stats := StatsSnapshot{}
	if r.runner != nil {
		stats = statsSnapshot(r.runner.Stats())
	}
	return RunSnapshot{
		RunID:       r.RunID,
		Profile:     r.Profile,
		State:       r.State,
		StopReason:  r.StopReason,
		StartedAtMS: r.StartedAtMS,
		EndedAtMS:   r.EndedAtMS,
		Count:       r.Config.Run.Count,
		Scenario:    r.Config.Run.Scenario,
		Stats:       stats,
		Thresholds:  r.Thresholds,
		QuantilesMS: r.Quantiles,
		Error:       r.Error,
	}
}

func statsSnapshot(stats runner.Stats) StatsSnapshot {
	done := stats.Success + stats.Failure
	failureRate := 0.0
	if done > 0 {
		failureRate = float64(stats.Failure) / float64(done)
	}
	return StatsSnapshot{
		Started:     stats.Started,
		Online:      stats.Online,
		Success:     stats.Success,
		Failure:     stats.Failure,
		Canceled:    stats.Canceled,
		FailureRate: failureRate,
	}
}

func normalizeStopReason(reason string) StopReason {
	switch StopReason(strings.TrimSpace(strings.ToLower(reason))) {
	case StopReasonCompleted, StopReasonDuration, StopReasonManual, StopReasonShutdown, StopReasonError, StopReasonThreshold:
		return StopReason(strings.TrimSpace(strings.ToLower(reason)))
	default:
		return ""
	}
}

func stopReasonFromContext(ctx context.Context) StopReason {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return StopReasonDuration
	case errors.Is(ctx.Err(), context.Canceled):
		return StopReasonShutdown
	default:
		return StopReasonCompleted
	}
}
