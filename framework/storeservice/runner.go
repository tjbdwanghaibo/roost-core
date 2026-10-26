package storeservice

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

//go:embed lua/*.lua
var runnerLua embed.FS

// Options 配置运行器。零值字段取缺省（括号内）。
type Options struct {
	// Owner 是分区租约的持有者名（store_service.owner）；不写取 store-<随机>。
	Owner string
	// BatchSize 是每批最多读的条目数（256）。
	BatchSize int
	// LeaseDuration 是分区租约时长（10s）；每 LeaseDuration/3 续约一次，续约失败立即放手该分区。
	LeaseDuration time.Duration
	// Block 是没有新条目时 XREAD 阻塞等待的上限（100ms）。
	Block time.Duration
	// MaxPartitions 是本进程最多同时持有的分区数，0 = 不限。
	MaxPartitions int
	// PausedRecheck 是暂停的分区多久看一次暂停键是否已被运维删除（30s）。
	PausedRecheck time.Duration
	// RetryMin / RetryMax 是暂时错误的重试退避（50ms 起翻倍，封顶 5s）。
	RetryMin, RetryMax time.Duration
}

func (o Options) withDefaults() Options {
	if strings.TrimSpace(o.Owner) == "" {
		var raw [8]byte
		_, _ = rand.Read(raw[:])
		o.Owner = "store-" + hex.EncodeToString(raw[:])
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 256
	}
	if o.LeaseDuration <= 0 {
		o.LeaseDuration = 10 * time.Second
	}
	if o.Block <= 0 {
		o.Block = 100 * time.Millisecond
	}
	if o.PausedRecheck <= 0 {
		o.PausedRecheck = 30 * time.Second
	}
	if o.RetryMin <= 0 {
		o.RetryMin = 50 * time.Millisecond
	}
	if o.RetryMax < o.RetryMin {
		o.RetryMax = 5 * time.Second
	}
	return o
}

// Runner 按（投影，分区）并行地把权威 Redis 的日志投影进 Mongo。
type Runner struct {
	redis   fredis.IRedis
	scripts fredis.DurableScriptBatcher
	streams fredis.StreamReader
	opts    Options
	lua     struct{ plock, advance *fredis.LuaScript }

	projections []Projection
	slots       chan struct{} // MaxPartitions > 0 时限制同时持有的分区数
	states      []*projectionState

	runMu  sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// projectionState 是一个投影的分区状态，供健康检查与指标汇总。
type projectionState struct {
	projection Projection
	paused     []atomic.Bool
	lagMS      []atomic.Int64
	backlog    []atomic.Int64
}

// NewRunner 校验能力并构造运行器，不访问 Redis。authority 必须实现 DurableScriptBatcher、StreamReader 与 ScriptLoader。
func NewRunner(authority fredis.IRedis, options Options, projections ...Projection) (*Runner, error) {
	scripts, okScripts := authority.(fredis.DurableScriptBatcher)
	streams, okStreams := authority.(fredis.StreamReader)
	_, okLoader := authority.(fredis.ScriptLoader)
	if authority == nil || !okScripts || !okStreams || !okLoader {
		return nil, fmt.Errorf("store service: %T lacks durable script batches, stream reads or SCRIPT LOAD", authority)
	}
	if len(projections) == 0 {
		return nil, errors.New("store service: at least one projection is required")
	}
	options = options.withDefaults()
	r := &Runner{redis: authority, scripts: scripts, streams: streams, opts: options, projections: projections}
	for _, name := range []struct {
		file   string
		target **fredis.LuaScript
	}{{"plock", &r.lua.plock}, {"advance", &r.lua.advance}} {
		body, err := runnerLua.ReadFile("lua/" + name.file + ".lua")
		if err != nil {
			return nil, err
		}
		*name.target = fredis.NewLuaScript(name.file, string(body))
	}
	seen := map[string]bool{}
	for _, projection := range projections {
		if projection == nil || projection.Partitions() < 1 || projection.KeyPrefix() == "" || seen[projection.Name()] {
			return nil, fmt.Errorf("store service: invalid or duplicate projection %v", projection)
		}
		seen[projection.Name()] = true
		n := projection.Partitions()
		r.states = append(r.states, &projectionState{projection: projection, paused: make([]atomic.Bool, n), lagMS: make([]atomic.Int64, n), backlog: make([]atomic.Int64, n)})
	}
	if options.MaxPartitions > 0 {
		r.slots = make(chan struct{}, options.MaxPartitions)
	}
	return r, nil
}

// Owner 返回本运行器的租约持有者名。
func (r *Runner) Owner() string { return r.opts.Owner }

// Concurrency 是运行器在权威 Redis 连接池上的并发上限：每个持有的分区一个循环（阻塞读占一条连接）。
func (r *Runner) Concurrency() int {
	total := 0
	for _, projection := range r.projections {
		total += projection.Partitions()
	}
	if r.opts.MaxPartitions > 0 && r.opts.MaxPartitions < total {
		return r.opts.MaxPartitions
	}
	return total
}

// Start 装载脚本，并在后台为每个（投影，分区）起一个循环，运行到 Stop。运行中再次 Start 无效果。
func (r *Runner) Start(ctx context.Context) error {
	r.runMu.Lock()
	defer r.runMu.Unlock()
	if r.done != nil {
		select {
		case <-r.done:
		default:
			return nil
		}
	}
	if err := fredis.PreloadScripts(ctx, r.redis.(fredis.ScriptLoader), r.lua.plock, r.lua.advance); err != nil {
		return fmt.Errorf("store service: %w", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	r.cancel, r.done = cancel, done
	var wg sync.WaitGroup
	for _, state := range r.states {
		for partition := range state.projection.Partitions() {
			wg.Go(func() { r.partitionLoop(runCtx, state, partition) })
		}
	}
	go func() { wg.Wait(); close(done) }()
	return nil
}

// Stop 取消全部循环并在 ctx 内等它们退出：超时返回 ctx 的错误，用新 ctx 再调会再等；已停或没启动时返回 nil。
// 循环在一批的中途被取消时，Mongo 写与进度推进都是幂等的，下一次启动从进度重放。
func (r *Runner) Stop(ctx context.Context) error {
	r.runMu.Lock()
	cancel, done := r.cancel, r.done
	r.runMu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Paused 返回每个投影暂停的分区（投影名 → 分区号）；健康检查非空即 Fail。
func (r *Runner) Paused() map[string][]int {
	out := map[string][]int{}
	for _, state := range r.states {
		for partition := range state.paused {
			if state.paused[partition].Load() {
				out[state.projection.Name()] = append(out[state.projection.Name()], partition)
			}
		}
	}
	return out
}

// Backlog 返回每个投影最近一次看到的未投影条目数之和与最老条目的年龄（秒）。
func (r *Runner) Backlog() map[string][2]float64 {
	out := map[string][2]float64{}
	for _, state := range r.states {
		var entries, lag int64
		for partition := range state.backlog {
			entries += state.backlog[partition].Load()
			lag = max(lag, state.lagMS[partition].Load())
		}
		out[state.projection.Name()] = [2]float64{float64(entries), float64(lag) / 1000}
	}
	return out
}

func (r *Runner) key(projection Projection, kind string, partition int) string {
	return projection.KeyPrefix() + ":" + kind + ":" + strconv.Itoa(partition)
}

// partitionLoop 是一个（投影，分区）的循环：取租约 → 处理到租约丢失 → 再取。
func (r *Runner) partitionLoop(ctx context.Context, state *projectionState, partition int) {
	name := state.projection.Name()
	for ctx.Err() == nil {
		if r.slots != nil {
			select {
			case r.slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
		}
		token, held := r.lease(ctx, state.projection, partition)
		if !held {
			if r.slots != nil {
				<-r.slots
			}
			sleep(ctx, r.opts.LeaseDuration/3)
			continue
		}
		r.process(ctx, state, partition, token)
		if r.slots != nil {
			<-r.slots
		}
		if ctx.Err() == nil {
			metrics.IncCounter("store_service.lease_lost_total", metrics.Labels{"projection": name}, 1)
		}
	}
}

// lease 取得或续约分区租约，返回 token。
func (r *Runner) lease(ctx context.Context, projection Projection, partition int) (string, bool) {
	code, items, err := r.run(ctx, r.lua.plock, []string{r.key(projection, "plock", partition)}, r.opts.Owner, r.opts.LeaseDuration.Milliseconds())
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("store service: partition lease", "projection", projection.Name(), "partition", partition, "err", err)
		}
		return "", false
	}
	if code != "OK" {
		return "", false
	}
	return fredis.ReplyString(items, 0), true
}

// process 在持有租约期间处理一个分区，租约丢失或 ctx 结束时返回。
func (r *Runner) process(ctx context.Context, state *projectionState, partition int, token string) {
	projection := state.projection
	name := projection.Name()
	renewAt := time.Now().Add(r.opts.LeaseDuration / 3)
	maintainAt, observeAt := time.Now(), time.Now()
	cursor, err := r.progress(ctx, projection, partition)
	if err != nil {
		slog.Warn("store service: read progress", "projection", name, "partition", partition, "err", err)
		return
	}
	for ctx.Err() == nil {
		if time.Now().After(renewAt) {
			renewed, held := r.lease(ctx, projection, partition)
			if !held || renewed != token {
				return
			}
			renewAt = time.Now().Add(r.opts.LeaseDuration / 3)
		}
		if r.pausedKey(ctx, projection, partition) {
			r.setPaused(state, partition, true)
			sleep(ctx, min(r.opts.PausedRecheck, r.opts.LeaseDuration/3))
			continue
		}
		r.setPaused(state, partition, false)
		entries, err := r.read(ctx, projection, partition, cursor)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("store service: read log", "projection", name, "partition", partition, "err", err)
				sleep(ctx, r.opts.RetryMin)
			}
			continue
		}
		if time.Now().After(observeAt) {
			r.observeBacklog(ctx, state, partition, cursor)
			observeAt = time.Now().Add(time.Second)
		}
		if len(entries) == 0 {
			if time.Now().After(maintainAt) {
				// 空闲时也按进度做一次收尾（saga：驱逐过了保留期的终态记录）。
				if err := projection.AfterAdvance(ctx, partition, cursor); err != nil && ctx.Err() == nil {
					slog.Warn("store service: after advance", "projection", name, "partition", partition, "err", err)
				}
				maintainAt = time.Now().Add(r.opts.LeaseDuration / 3)
			}
			continue
		}
		if !r.applyBatch(ctx, state, partition, entries) {
			continue
		}
		upto := entries[len(entries)-1].ID
		code, _, err := r.run(ctx, r.lua.advance, []string{r.key(projection, "plock", partition), r.key(projection, "proj", partition), r.key(projection, "log", partition)},
			r.opts.Owner, token, upto)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("store service: advance", "projection", name, "partition", partition, "err", err)
			}
			// 结果未知：重新读进度再继续（推进与 Mongo 写都幂等）。
			if cursor, err = r.progress(ctx, projection, partition); err != nil {
				return
			}
			continue
		}
		switch code {
		case "OK", "NOOP":
		case "LOST":
			slog.Warn("store service: lost the partition lease before advancing; another instance continues", "projection", name, "partition", partition)
			return
		default:
			slog.Error("store service: advance refused", "projection", name, "partition", partition, "code", code)
			sleep(ctx, r.opts.RetryMax)
			return
		}
		cursor = upto
		metrics.IncCounter("store_service.projected_entries_total", metrics.Labels{"projection": name}, int64(len(entries)))
		metrics.IncCounter("store_service.batches_total", metrics.Labels{"projection": name}, 1)
		if err := projection.AfterAdvance(ctx, partition, upto); err != nil && ctx.Err() == nil {
			slog.Warn("store service: after advance", "projection", name, "partition", partition, "err", err)
		}
		maintainAt = time.Now().Add(r.opts.LeaseDuration / 3)
	}
}

// applyBatch 把一批条目交给投影，暂时错误退避重试同一批；非暂时错误写暂停键、暂停该分区（不跳过）。返回是否已写进 Mongo。
func (r *Runner) applyBatch(ctx context.Context, state *projectionState, partition int, entries []Entry) bool {
	projection := state.projection
	name := projection.Name()
	delay := r.opts.RetryMin
	for ctx.Err() == nil {
		start := time.Now()
		err := projection.Apply(ctx, partition, entries)
		metrics.ObserveHistogram("store_service.batch_seconds", metrics.Labels{"projection": name}, time.Since(start))
		if err == nil {
			return true
		}
		if IsPermanent(err) {
			reason := fmt.Sprintf("%s: entries %s..%s: %v", time.Now().UTC().Format(time.RFC3339), entries[0].ID, entries[len(entries)-1].ID, err)
			if setErr := r.redis.Set(ctx, r.key(projection, "paused", partition), reason, 0); setErr != nil {
				slog.Error("store service: write the pause key", "projection", name, "partition", partition, "err", setErr)
			}
			r.setPaused(state, partition, true)
			slog.Error("store service: projection hit a permanent error; the partition is paused, nothing is skipped. Repair the conflict, then delete the pause key",
				"projection", name, "partition", partition, "pause_key", r.key(projection, "paused", partition), "err", err)
			return false
		}
		if ctx.Err() == nil {
			slog.Warn("store service: projection failed; retrying the same batch", "projection", name, "partition", partition, "err", err, "retry_in", delay)
		}
		sleep(ctx, delay)
		delay = min(delay*2, r.opts.RetryMax)
	}
	return false
}

func (r *Runner) setPaused(state *projectionState, partition int, paused bool) {
	if state.paused[partition].Swap(paused) == paused {
		return
	}
	count := int64(0)
	for i := range state.paused {
		if state.paused[i].Load() {
			count++
		}
	}
	metrics.SetGauge("store_service.paused_partitions", metrics.Labels{"projection": state.projection.Name()}, count)
}

func (r *Runner) pausedKey(ctx context.Context, projection Projection, partition int) bool {
	n, err := r.redis.Exists(ctx, r.key(projection, "paused", partition))
	return err == nil && n > 0
}

func (r *Runner) progress(ctx context.Context, projection Projection, partition int) (string, error) {
	raw, err := r.redis.Get(ctx, r.key(projection, "proj", partition))
	if errors.Is(err, fredis.ErrNil) {
		return "0-0", nil
	}
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// read 读进度之后的一批：先 XRANGE，空了再阻塞 XREAD 等一会儿。
func (r *Runner) read(ctx context.Context, projection Projection, partition int, cursor string) ([]Entry, error) {
	key := r.key(projection, "log", partition)
	entries, err := r.streams.XRangeAfter(ctx, key, cursor, int64(r.opts.BatchSize))
	if err != nil || len(entries) > 0 {
		return toEntries(entries), err
	}
	entries, err = r.streams.XReadAfter(ctx, key, cursor, int64(r.opts.BatchSize), r.opts.Block)
	return toEntries(entries), err
}

func toEntries(entries []fredis.StreamEntry) []Entry {
	out := make([]Entry, len(entries))
	for i, entry := range entries {
		out[i] = Entry{ID: entry.ID, Fields: entry.Fields}
	}
	return out
}

// observeBacklog 更新积压与延迟：积压 = XLEN，延迟 = 现在 − 进度之后第一条条目 ID 的毫秒部分。
func (r *Runner) observeBacklog(ctx context.Context, state *projectionState, partition int, cursor string) {
	projection := state.projection
	name := projection.Name()
	key := r.key(projection, "log", partition)
	n, err := r.streams.XLen(ctx, key)
	if err != nil {
		return
	}
	state.backlog[partition].Store(n)
	metrics.SetGauge("store_service.backlog_entries", metrics.Labels{"projection": name, "partition": strconv.Itoa(partition)}, n)
	lag := int64(0)
	if n > 0 {
		if first, err := r.streams.XRangeAfter(ctx, key, cursor, 1); err == nil && len(first) == 1 {
			if ms, _, ok := strings.Cut(first[0].ID, "-"); ok {
				if at, err := strconv.ParseInt(ms, 10, 64); err == nil {
					lag = max(time.Now().UnixMilli()-at, 0)
				}
			}
		}
	}
	state.lagMS[partition].Store(lag)
	worst := int64(0)
	for i := range state.lagMS {
		worst = max(worst, state.lagMS[i].Load())
	}
	metrics.SetGauge("store_service.lag_seconds", metrics.Labels{"projection": name}, worst/1000)
}

// run 执行一个写脚本（与同连接 WAITAOF 一起）。WAITAOF 没确认时按错误返回：进度与租约是 Store Service 自己的状态，
// 结果未知时重读进度即可，不需要 fail-stop（权威数据的持久由写入方 fail-stop 保证）。
func (r *Runner) run(ctx context.Context, script *fredis.LuaScript, keys []string, args ...any) (string, []any, error) {
	results, local, _, err := r.scripts.DurableScriptBatch(ctx, []fredis.ScriptCall{script.Call(keys, args...)}, 1, 0, time.Second)
	if err != nil {
		return "", nil, err
	}
	if results[0].Err != nil {
		return "", nil, results[0].Err
	}
	if local < 1 {
		return "", nil, fmt.Errorf("store service: %s executed but WAITAOF local=%d", script.Name, local)
	}
	return fredis.ReplyCode(results[0].Value)
}

func sleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
