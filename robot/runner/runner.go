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
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"github.com/tjbdwanghaibo/roost-core/robot"
	"github.com/tjbdwanghaibo/roost-core/robot/action"
	"github.com/tjbdwanghaibo/roost-core/robot/protocol"
	"github.com/tjbdwanghaibo/roost-core/robot/scenario"
	"github.com/tjbdwanghaibo/roost-core/robot/session"
	"github.com/tjbdwanghaibo/roost-core/robot/transport"
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
//
// Ordinals are never reused within one run: a staged run that shrinks and
// grows again hands the new robots fresh ordinals past every one issued so
// far, because a robot just stopped may still be closing its session under
// its identity (RR-20261006-09). Such a run therefore uses ordinals beyond
// Count — up to Count plus every regrowth — and the provider must cover them.
type IdentityProvider func(index int) (int64, error)

// Config shapes one run.
type Config struct {
	Executor Executor
	// Count is the robot population for pool/looping (default 1).
	Count int
	// Rate is robots started per second for arrival-rate (default 1).
	Rate float64
	// MaxConcurrent bounds in-flight arrival-rate robots (default 1024). Excess arrivals are counted, never queued.
	MaxConcurrent int
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
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 1024
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
// (Success+Failure) is honest. For pool and arrival-rate runs, where each
// robot runs its scenario once, Started == Success+Failure+Canceled once the
// run drains. Looping robots repeat their scenario: Success and Failure count
// scenario runs, Started counts robots, so the sum exceeds Started (N12 O9).
type Stats struct {
	Scheduled       int64
	DroppedCapacity int64
	DroppedSchedule int64
	LaunchFailed    int64
	PeakOnline      int64
	Started         int64
	Online          int64
	Success         int64
	Failure         int64
	Canceled        int64
}

type Runner struct {
	recorder  session.Recorder
	traffic   transport.Traffic
	cfg       Config
	actions   *action.Registry
	scenarios *scenario.Registry
	protocols *protocol.Registry
	identity  IdentityProvider
	bootstrap func() error
	auth      robot.AuthProvider
	labels    metrics.Labels

	cancelMu sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
	used     bool
	wg       sync.WaitGroup

	scheduled       atomic.Int64
	droppedCapacity atomic.Int64
	droppedSchedule atomic.Int64
	launchFailed    atomic.Int64
	peakOnline      atomic.Int64
	started         atomic.Int64
	online          atomic.Int64
	success         atomic.Int64
	failure         atomic.Int64
	canceled        atomic.Int64
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

func WithMetricLabels(labels metrics.Labels) Option {
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
	r.cfg.Transport.Traffic = &r.traffic
	return r
}

func (r *Runner) Run(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// 只建一个 ctx：旧实现先 WithCancel 再在 Duration > 0 时覆盖成 WithTimeout，第一个 cancel 丢失，
	// 那个 ctx 直到父 ctx 结束才释放（N12 观察 O9）。
	var runCtx context.Context
	var cancel context.CancelFunc
	if r.cfg.Duration > 0 {
		runCtx, cancel = context.WithTimeout(ctx, r.cfg.Duration)
	} else {
		runCtx, cancel = context.WithCancel(ctx)
	}
	r.cancelMu.Lock()
	if r.used {
		r.cancelMu.Unlock()
		cancel()
		return fmt.Errorf("robot runner: each runner can only run once")
	}
	r.used = true
	r.done = make(chan struct{})
	r.cancel = cancel
	r.cancelMu.Unlock()
	defer func() {
		cancel()
		r.cancelMu.Lock()
		r.cancel = nil
		close(r.done)
		r.cancelMu.Unlock()
	}()

	if r.cfg.Executor != ExecutorPool && r.cfg.Executor != ExecutorLooping && r.cfg.Executor != ExecutorArrivalRate {
		return fmt.Errorf("robot runner: unknown executor %q", r.cfg.Executor)
	}
	if math.IsNaN(r.cfg.Rate) || math.IsInf(r.cfg.Rate, 0) || r.cfg.Rate > float64(time.Second) || float64(time.Second)/r.cfg.Rate >= float64(1<<63-1) {
		return fmt.Errorf("robot runner: rate must be finite and yield an interval between 1ns and the maximum duration")
	}
	for _, stage := range r.cfg.Stages {
		if stage.Target < 0 || stage.Duration <= 0 {
			return fmt.Errorf("robot runner: stage target must be nonnegative and duration positive")
		}
	}
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
	metrics.SetGauge("robot.runner.target", r.metricLabels(metrics.Labels{"scenario": scn.Name()}), int64(r.cfg.Count))

	var err error
	switch r.cfg.Executor {
	case ExecutorArrivalRate:
		err = r.runArrivalRate(runCtx, scn)
	case ExecutorLooping:
		err = r.runPopulation(runCtx, scn, true)
	default:
		err = r.runPopulation(runCtx, scn, false)
	}
	// 启动失败时先取消已启动机器人，否则它们等 Run 结束，Run 却等它们结束。
	if err != nil {
		cancel()
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
	// launched 是当前在线目标数（缩容会减回去）；lastOrdinal 是已经发出去的最大序号，只增不回收
	// （RR-20261006-09）：缩容停掉的机器人可能还在收尾（关会话、登出），扩容若从 launched+1 起编号，
	// 新机器人会拿到它的序号与 PlayerID，两个机器人同时用一个身份。
	launched, lastOrdinal := 0, 0
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
				if ctx.Err() != nil {
					return nil
				}
				r.scheduled.Add(1)
				lastOrdinal++
				index := lastOrdinal
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
			metrics.SetGauge("robot.runner.target", r.metricLabels(metrics.Labels{"scenario": scn.Name()}), int64(launched))
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
	// 以绝对时间计算应投递次数。Ticker 丢拍不等于服务端完成得快，不能隐藏压测端欠载。
	start := time.Now()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var accounted int64
	defer func() {
		// 结束前最后一段调度停顿也计入欠载，不能因为先收到取消而漏掉这些时间槽。
		end := time.Now()
		if deadline, ok := ctx.Deadline(); ok && deadline.Before(end) {
			end = deadline
		}
		due := int64(end.Sub(start) / interval)
		if due > accounted {
			r.scheduled.Add(due - accounted)
			r.droppedSchedule.Add(due - accounted)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if ctx.Err() != nil {
				return nil
			}
			due := int64(time.Since(start) / interval)
			if due <= accounted {
				continue
			}
			count := due - accounted
			accounted = due
			r.scheduled.Add(count)
			r.droppedSchedule.Add(count - 1)
			// 不补发过期的突发流量；只尝试当前这一拍，容量满则明确记录拒绝。
			if r.online.Load() >= int64(r.cfg.MaxConcurrent) {
				r.droppedCapacity.Add(1)
				continue
			}
			if err := r.launch(ctx, int(due), scn, false, nil); err != nil {
				return err
			}
		}
	}
}

func (r *Runner) launch(ctx context.Context, index int, scn scenario.Scenario, looping bool, stop <-chan struct{}) error {
	playerID, err := r.identity(index)
	if err != nil {
		r.launchFailed.Add(1)
		return fmt.Errorf("robot runner: identity for robot %d: %w", index, err)
	}
	r.wg.Add(1)
	r.started.Add(1)
	// 在启动 goroutine 前保留名额，避免调度器连续投递时看不到尚未运行的机器人。
	online := r.online.Add(1)
	for peak := r.peakOnline.Load(); online > peak; peak = r.peakOnline.Load() {
		if r.peakOnline.CompareAndSwap(peak, online) {
			break
		}
	}
	go func() {
		defer r.wg.Done()
		labels := r.metricLabels(metrics.Labels{"scenario": scn.Name()})
		metrics.AddGauge("robot.runner.online", labels, 1)
		defer func() {
			r.online.Add(-1)
			metrics.AddGauge("robot.runner.online", labels, -1)
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
		Recorder:  &r.recorder,
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
		metrics.IncCounter("robot.runner.scenario.total", r.metricLabels(metrics.Labels{"scenario": scn.Name(), "result": "canceled"}), 1)
	case err != nil:
		r.failure.Add(1)
		metrics.IncCounter("robot.runner.scenario.total", r.metricLabels(metrics.Labels{"scenario": scn.Name(), "result": "error"}), 1)
		metrics.ObserveHistogram("robot.runner.scenario.cost", r.metricLabels(metrics.Labels{"scenario": scn.Name(), "result": "error"}), elapsed)
		slog.Warn("robot runner: scenario failed", "robot", index, "player", playerID, "scenario", scn.Name(), "err", err)
	default:
		r.success.Add(1)
		metrics.IncCounter("robot.runner.scenario.total", r.metricLabels(metrics.Labels{"scenario": scn.Name(), "result": "ok"}), 1)
		metrics.ObserveHistogram("robot.runner.scenario.cost", r.metricLabels(metrics.Labels{"scenario": scn.Name(), "result": "ok"}), elapsed)
	}
}

func (r *Runner) Stop(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.cancelMu.Lock()
	cancel, done := r.cancel, r.done
	r.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// 等调度器停止新增任务且机器人全部退出；不能在 Add 仍可能发生时直接 Wait。
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runner) Stats() Stats {
	if r == nil {
		return Stats{}
	}
	return Stats{
		Scheduled:       r.scheduled.Load(),
		DroppedCapacity: r.droppedCapacity.Load(),
		DroppedSchedule: r.droppedSchedule.Load(),
		LaunchFailed:    r.launchFailed.Load(),
		PeakOnline:      r.peakOnline.Load(),
		Started:         r.started.Load(),
		Online:          r.online.Load(),
		Success:         r.success.Load(),
		Failure:         r.failure.Load(),
		Canceled:        r.canceled.Load(),
	}
}

// Protocols exposes the runner's protocol registry so business registration
// (generated codec tables, RegisterCall lines) can target it.
func (r *Runner) Protocols() *protocol.Registry { return r.protocols }

// Actions exposes the action registry for business registration.
func (r *Runner) Actions() *action.Registry { return r.actions }

// Scenarios exposes the scenario registry for business registration.
func (r *Runner) Scenarios() *scenario.Registry { return r.scenarios }

func (r *Runner) metricLabels(base metrics.Labels) metrics.Labels {
	labels := cloneLabels(r.labels)
	for key, value := range base {
		labels[key] = value
	}
	return labels
}

func cloneLabels(labels metrics.Labels) metrics.Labels {
	out := make(metrics.Labels, len(labels)+2)
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

// MessageReport 返回本次运行的请求统计，含重连；不复用其他 Runner 的历史指标。
func (r *Runner) MessageReport() session.Report          { return r.recorder.Snapshot() }
func (r *Runner) TrafficReport() transport.TrafficReport { return r.traffic.Snapshot() }
