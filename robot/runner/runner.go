// Package runner schedules robots: goroutine-per-bot with k6-style
// executors. Ported from the cube robot service with the business calls
// replaced by injection points (IdentityProvider, Bootstrap) and two of its
// gaps fixed: multi-stage ramping and open-loop arrival-rate injection
// exist in-process now, and cancelled robots are counted (the original let
// them vanish, skewing the failure rate optimistic).
package runner

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/cube-core/obs"
	"github.com/tjbdwanghaibo/cube-core/robot"
	"github.com/tjbdwanghaibo/cube-core/robot/action"
	"github.com/tjbdwanghaibo/cube-core/robot/protocol"
	"github.com/tjbdwanghaibo/cube-core/robot/scenario"
	"github.com/tjbdwanghaibo/cube-core/robot/transport"
)

// Executor selects the scheduling model (k6 terminology):
//
//   - pool (default): launch Count robots (ramped by Ramp), each runs the
//     scenario once — closed-loop client simulation.
//   - looping: like pool, but each robot repeats its scenario until the run
//     stops — closed-loop sustained load.
//   - arrival-rate: open-loop injection — start Rate fresh robots per
//     second regardless of how long earlier ones take (the vegeta model;
//     exposes queueing collapse that closed loops hide).
type Executor string

const (
	ExecutorPool        Executor = "pool"
	ExecutorLooping     Executor = "looping"
	ExecutorArrivalRate Executor = "arrival-rate"
)

// Ramp launches robots in batches of Step every Interval.
type Ramp struct {
	Step     int
	Interval time.Duration
}

// Stage is one phase of a staged run: ramp the online robot target to
// Target over Duration (k6 ramping-vus). Only meaningful with the looping
// executor.
type Stage struct {
	Target   int
	Duration time.Duration
}

// IdentityProvider maps a robot ordinal (1-based) to its business identity.
// Games inject their entity-id scheme here; the default is the ordinal
// itself.
type IdentityProvider func(index int) (int64, error)

// Config shapes one run.
type Config struct {
	Executor Executor
	// Count is the robot population for pool/looping (default 1).
	Count int
	// Rate is robots started per second for arrival-rate (default 1).
	Rate float64
	// Stages optionally reshape the looping population over time.
	Stages []Stage
	// Scenario is the registered scenario name.
	Scenario string
	// Duration bounds the run (0 = until the scenario pool drains or Stop).
	Duration time.Duration
	Ramp     Ramp
	// Seed makes robot RNGs reproducible (0 picks a fixed default).
	Seed      int64
	Transport transport.Config
}

func (c Config) Normalize() Config {
	if c.Executor == "" {
		c.Executor = ExecutorPool
	}
	c.Executor = Executor(strings.ToLower(strings.TrimSpace(string(c.Executor))))
	if c.Count <= 0 {
		c.Count = 1
	}
	if c.Rate <= 0 {
		c.Rate = 1
	}
	if c.Scenario == "" {
		c.Scenario = "ping"
	}
	if c.Seed == 0 {
		c.Seed = 1
	}
	c.Transport = c.Transport.Normalize()
	if c.Ramp.Step <= 0 {
		c.Ramp.Step = c.Count
	}
	return c
}

// Stats are the run counters. Canceled counts robots whose scenario ended
// because the run stopped — kept separate so the failure rate's denominator
// (Success+Failure) is honest and Started == Success+Failure+Canceled once
// the run drains.
type Stats struct {
	Started  int64
	Online   int64
	Success  int64
	Failure  int64
	Canceled int64
}

type Runner struct {
	cfg       Config
	actions   *action.Registry
	scenarios *scenario.Registry
	protocols *protocol.Registry
	identity  IdentityProvider
	bootstrap func() error
	auth      robot.AuthProvider
	labels    obs.Labels

	cancelMu sync.Mutex
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	started  atomic.Int64
	online   atomic.Int64
	success  atomic.Int64
	failure  atomic.Int64
	canceled atomic.Int64
}

type Option func(*Runner)

func WithActionRegistry(reg *action.Registry) Option {
	return func(r *Runner) {
		if reg != nil {
			r.actions = reg
		}
	}
}

func WithScenarioRegistry(reg *scenario.Registry) Option {
	return func(r *Runner) {
		if reg != nil {
			r.scenarios = reg
		}
	}
}

func WithProtocolRegistry(reg *protocol.Registry) Option {
	return func(r *Runner) {
		if reg != nil {
			r.protocols = reg
		}
	}
}

// WithIdentityProvider injects the business identity scheme (cube maps the
// ordinal through its entity-id encoding here).
func WithIdentityProvider(provider IdentityProvider) Option {
	return func(r *Runner) {
		if provider != nil {
			r.identity = provider
		}
	}
}

// WithBootstrap runs once before the first robot launches (kind registries,
// warmups).
func WithBootstrap(fn func() error) Option {
	return func(r *Runner) { r.bootstrap = fn }
}

// WithAuthProvider injects the per-robot gateway handshake builder.
func WithAuthProvider(auth robot.AuthProvider) Option {
	return func(r *Runner) { r.auth = auth }
}

func WithMetricLabels(labels obs.Labels) Option {
	return func(r *Runner) { r.labels = cloneLabels(labels) }
}

func New(cfg Config, opts ...Option) *Runner {
	r := &Runner{
		cfg:       cfg.Normalize(),
		actions:   action.NewRegistry(),
		scenarios: scenario.NewRegistry(),
		protocols: protocol.NewRegistry(protocol.JSONCodec{}),
		identity:  func(index int) (int64, error) { return int64(index), nil },
	}
	for _, opt := range opts {
		if opt != nil {
			opt(r)
		}
	}
	return r
}

func (r *Runner) Run(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	if r.cfg.Duration > 0 {
		runCtx, cancel = context.WithTimeout(ctx, r.cfg.Duration)
	}
	r.cancelMu.Lock()
	r.cancel = cancel
	r.cancelMu.Unlock()
	defer func() {
		cancel()
		r.cancelMu.Lock()
		r.cancel = nil
		r.cancelMu.Unlock()
	}()

	if r.bootstrap != nil {
		if err := r.bootstrap(); err != nil {
			return fmt.Errorf("robot runner: bootstrap: %w", err)
		}
	}
	scn, ok := r.scenarios.Get(r.cfg.Scenario)
	if !ok {
		return fmt.Errorf("robot runner: scenario %q not found; available=%v", r.cfg.Scenario, r.scenarios.Names())
	}
	slog.Info("robot runner: start",
		"executor", r.cfg.Executor, "count", r.cfg.Count, "rate", r.cfg.Rate,
		"scenario", scn.Name(), "transport", r.cfg.Transport.Type, "endpoint", r.cfg.Transport.Endpoint)
	obs.SetGauge("robot.runner.target", r.metricLabels(obs.Labels{"scenario": scn.Name()}), int64(r.cfg.Count))

	var err error
	switch r.cfg.Executor {
	case ExecutorArrivalRate:
		err = r.runArrivalRate(runCtx, scn)
	case ExecutorLooping:
		err = r.runPopulation(runCtx, scn, true)
	default:
		err = r.runPopulation(runCtx, scn, false)
	}
	r.wg.Wait()
	stats := r.Stats()
	slog.Info("robot runner: stop",
		"started", stats.Started, "success", stats.Success,
		"failure", stats.Failure, "canceled", stats.Canceled)
	return err
}

// runPopulation launches Count robots ramped by Ramp; looping robots repeat
// their scenario until the run context ends. Stages (looping only) then
// steer the population up and down over time.
func (r *Runner) runPopulation(ctx context.Context, scn scenario.Scenario, looping bool) error {
	stop := make([]chan struct{}, 0, r.cfg.Count)
	launched := 0
	launchUpTo := func(target int) error {
		for launched < target {
			if err := ctx.Err(); err != nil {
				return nil
			}
			step := r.cfg.Ramp.Step
			if remaining := target - launched; step > remaining {
				step = remaining
			}
			for i := 0; i < step; i++ {
				index := launched + i + 1
				stopCh := make(chan struct{})
				stop = append(stop, stopCh)
				if err := r.launch(ctx, index, scn, looping, stopCh); err != nil {
					return err
				}
			}
			launched += step
			if launched < target && r.cfg.Ramp.Interval > 0 {
				if !sleepCtx(ctx, r.cfg.Ramp.Interval) {
					return nil
				}
			}
		}
		return nil
	}
	shrinkTo := func(target int) {
		for launched > target && launched > 0 {
			launched--
			close(stop[launched])
			stop = stop[:launched]
		}
	}
	if err := launchUpTo(r.cfg.Count); err != nil {
		return err
	}
	if looping && len(r.cfg.Stages) > 0 {
		for _, stage := range r.cfg.Stages {
			if ctx.Err() != nil {
				return nil
			}
			if stage.Target > launched {
				if err := launchUpTo(stage.Target); err != nil {
					return err
				}
			} else if stage.Target < launched {
				shrinkTo(stage.Target)
			}
			obs.SetGauge("robot.runner.target", r.metricLabels(obs.Labels{"scenario": scn.Name()}), int64(launched))
			if !sleepCtx(ctx, stage.Duration) {
				return nil
			}
		}
		// Stages exhausted: the run is complete.
		shrinkTo(0)
		return nil
	}
	if looping {
		<-ctx.Done()
	}
	return nil
}

// runArrivalRate starts fresh single-shot robots at a constant rate until
// the run context ends — open-loop injection.
func (r *Runner) runArrivalRate(ctx context.Context, scn scenario.Scenario) error {
	interval := time.Duration(float64(time.Second) / r.cfg.Rate)
	if interval <= 0 {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	index := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			index++
			if err := r.launch(ctx, index, scn, false, nil); err != nil {
				return err
			}
		}
	}
}

func (r *Runner) launch(ctx context.Context, index int, scn scenario.Scenario, looping bool, stop <-chan struct{}) error {
	playerID, err := r.identity(index)
	if err != nil {
		return fmt.Errorf("robot runner: identity for robot %d: %w", index, err)
	}
	r.wg.Add(1)
	r.started.Add(1)
	go func() {
		defer r.wg.Done()
		labels := r.metricLabels(obs.Labels{"scenario": scn.Name()})
		r.online.Add(1)
		obs.AddGauge("robot.runner.online", labels, 1)
		defer func() {
			r.online.Add(-1)
			obs.AddGauge("robot.runner.online", labels, -1)
		}()

		botCtx := ctx
		if stop != nil {
			var cancel context.CancelFunc
			botCtx, cancel = context.WithCancel(ctx)
			defer cancel()
			go func() {
				select {
				case <-stop:
					cancel()
				case <-botCtx.Done():
				}
			}()
		}

		for {
			r.runOnce(botCtx, index, playerID, scn)
			if !looping || botCtx.Err() != nil {
				return
			}
		}
	}()
	return nil
}

func (r *Runner) runOnce(ctx context.Context, index int, playerID int64, scn scenario.Scenario) {
	start := time.Now()
	rb := robot.NewContext(robot.Config{
		ID:        index,
		PlayerID:  playerID,
		Transport: r.cfg.Transport,
		Protocols: r.protocols,
		Auth:      r.auth,
		Seed:      r.cfg.Seed*1_000_003 + int64(index),
	})
	rb.RunAction = func(ctx context.Context, rb *robot.Context, name string, param any) error {
		return r.actions.Run(ctx, rb, name, param)
	}
	defer rb.Close()

	err := scn.Run(ctx, rb)
	elapsed := time.Since(start)
	switch {
	case err != nil && ctx.Err() != nil:
		// The run was stopped mid-scenario: neither a success nor a failure,
		// but it must not vanish from the accounting either.
		r.canceled.Add(1)
		obs.IncCounter("robot.runner.scenario.total", r.metricLabels(obs.Labels{"scenario": scn.Name(), "result": "canceled"}), 1)
	case err != nil:
		r.failure.Add(1)
		obs.IncCounter("robot.runner.scenario.total", r.metricLabels(obs.Labels{"scenario": scn.Name(), "result": "error"}), 1)
		obs.ObserveHistogram("robot.runner.scenario.cost", r.metricLabels(obs.Labels{"scenario": scn.Name(), "result": "error"}), elapsed)
		slog.Warn("robot runner: scenario failed", "robot", index, "player", playerID, "scenario", scn.Name(), "err", err)
	default:
		r.success.Add(1)
		obs.IncCounter("robot.runner.scenario.total", r.metricLabels(obs.Labels{"scenario": scn.Name(), "result": "ok"}), 1)
		obs.ObserveHistogram("robot.runner.scenario.cost", r.metricLabels(obs.Labels{"scenario": scn.Name(), "result": "ok"}), elapsed)
	}
}

func (r *Runner) Stop(ctx context.Context) error {
	r.cancelMu.Lock()
	cancel := r.cancel
	r.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	if ctx == nil {
		<-done
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}

func (r *Runner) Stats() Stats {
	if r == nil {
		return Stats{}
	}
	return Stats{
		Started:  r.started.Load(),
		Online:   r.online.Load(),
		Success:  r.success.Load(),
		Failure:  r.failure.Load(),
		Canceled: r.canceled.Load(),
	}
}

// Protocols exposes the runner's protocol registry so business registration
// (generated codec tables, RegisterCall lines) can target it.
func (r *Runner) Protocols() *protocol.Registry { return r.protocols }

// Actions exposes the action registry for business registration.
func (r *Runner) Actions() *action.Registry { return r.actions }

// Scenarios exposes the scenario registry for business registration.
func (r *Runner) Scenarios() *scenario.Registry { return r.scenarios }

func (r *Runner) metricLabels(base obs.Labels) obs.Labels {
	labels := cloneLabels(r.labels)
	for key, value := range base {
		labels[key] = value
	}
	return labels
}

func cloneLabels(labels obs.Labels) obs.Labels {
	out := make(obs.Labels, len(labels)+2)
	for key, value := range labels {
		out[key] = value
	}
	return out
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
