package remoteflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	"github.com/tjbdwanghaibo/roost-core/framework/remoteentity"
)

func remoteNestWorkers() int {
	if remoteLoadEnabled() {
		return remoteInt("ROOST_REMOTE_WORKERS", 64)
	}
	return 4
}

func remoteLoadEnabled() bool { return os.Getenv("ROOST_REMOTE_LOAD") == "1" }
func remoteInt(name string, fallback int) int {
	if value := os.Getenv(name); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			panic("invalid " + name)
		}
		return n
	}
	return fallback
}
func remoteDuration() time.Duration {
	d, err := time.ParseDuration(os.Getenv("ROOST_REMOTE_DURATION"))
	if err != nil || d <= 0 {
		panic("positive ROOST_REMOTE_DURATION required")
	}
	return d
}
func remoteTestTimeout() time.Duration {
	if remoteLoadEnabled() {
		return remoteDuration() + 15*time.Minute
	}
	return 90 * time.Second
}
func remoteEntityCount() int {
	if remoteLoadEnabled() {
		n := remoteInt("ROOST_REMOTE_ENTITIES", 10000)
		if n < 2 || n%2 != 0 {
			panic("even entity count required")
		}
		return n
	}
	return 2
}

// markRemoteVerified 在全量核验通过后写 .verified；负载错误另由调用方在写完之后报告（一致性与错误分开判定）。
// settledAboveSuccess 是各实体实际计数超出成功回复数（区间下界）的总和：结果未知的请求里事后实际提交的部分。
func markRemoteVerified(t *testing.T, load remoteLoadResult, settledAboveSuccess int64) {
	t.Helper()
	content := "all Mongo and NATS snapshots verified; rollback and outbox checks passed\n" +
		fmt.Sprintf("interval check: entities=%d widened=%d settled_above_success=%d; load errors=%d not_applied=%d uncertain=%d harness=%d\n",
			load.Entities, load.Widened, settledAboveSuccess, load.Errors, load.NotApplied, load.Uncertain, load.Errors-load.NotApplied-load.Uncertain)
	if err := os.WriteFile(os.Getenv("ROOST_REMOTE_OUTPUT")+".verified", []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// remoteExpect 是负载结束后一个实体计数的期望区间 [Min, Max]：Min 是成功回复次数，Max 再加上结果未知的请求数。
// 没有结果未知的错误时 Min == Max，核验与点期望逐字相同。
type remoteExpect struct{ Min, Max int64 }

func (e remoteExpect) contains(v int64) bool { return v >= e.Min && v <= e.Max }

// suffix 只在区间被放宽时给失败文本补上期望范围，点期望的失败文本保持原样。
func (e remoteExpect) suffix() string {
	if e.Min == e.Max {
		return ""
	}
	return fmt.Sprintf(" want=[%d,%d]", e.Min, e.Max)
}

// remoteLoadResult 是负载阶段交给最终核验的期望区间与错误计数。
type remoteLoadResult struct {
	Expected                      map[int64]remoteExpect
	Entities, Widened             int
	Errors, NotApplied, Uncertain uint64
	FirstError                    string
}

type remoteErrorOutcome uint8

const (
	// remoteOutcomeUncertain：请求可能已提交（判别表“可能 / 已提交 / 部分已提交 / 结果未知”），实体区间上界 +1。
	remoteOutcomeUncertain remoteErrorOutcome = iota
	// remoteOutcomeNotApplied：判别表“否”的行，且本 fixture 的 handler 都是 RollbackState（失败即整条回滚），对实体不计。
	remoteOutcomeNotApplied
)

type remoteErrorRule struct {
	name    string
	outcome remoteErrorOutcome
	match   func(error) bool
}

func remoteErrorIs(target error) func(error) bool {
	return func(err error) bool { return errors.Is(err, target) }
}

// remoteErrorRules 按 docs/USER_GUIDE.md §4“回复错误判别”表从上到下排列，取第一个命中的行，全部 errors.Is，不匹配文本。
// 第 10、11、13 行在本 fixture 不会出现，未列出，命中时落入下面的保守兜底。
var remoteErrorRules = []remoteErrorRule{
	{"row1 nest.ErrCommitIndeterminate", remoteOutcomeUncertain, remoteErrorIs(nest.ErrCommitIndeterminate)},
	{"row2 entity.ErrRemotePersistenceIndeterminate", remoteOutcomeUncertain, remoteErrorIs(entity.ErrRemotePersistenceIndeterminate)},
	{"row2 entity.ErrRemoteCommitTimeout", remoteOutcomeUncertain, remoteErrorIs(entity.ErrRemoteCommitTimeout)},
	// 第 3 行是“已提交”；区间上界已覆盖，不单独计成功，避免把收尾失败的回复当成功回复。
	{"row3 nest.ErrAfterCommitFailed", remoteOutcomeUncertain, remoteErrorIs(nest.ErrAfterCommitFailed)},
	{"row4 nest.ErrRemotePartRejected", remoteOutcomeUncertain, remoteErrorIs(nest.ErrRemotePartRejected)},
	{"row5 nest.ErrNestedTransactionCommitted", remoteOutcomeUncertain, remoteErrorIs(nest.ErrNestedTransactionCommitted)},
	{"row6 nest.ErrNonRollbackNotRequeued", remoteOutcomeUncertain, remoteErrorIs(nest.ErrNonRollbackNotRequeued)},
	{"row7 nest.ErrCreatedEntityLockConflict", remoteOutcomeUncertain, func(err error) bool {
		return errors.Is(err, nest.ErrCreatedEntityLockConflict) && !errors.Is(err, nest.ErrLockTimeout)
	}},
	{"row8 nest.ErrCreatedEntityLockConflict+ErrLockTimeout", remoteOutcomeNotApplied, remoteErrorIs(nest.ErrCreatedEntityLockConflict)},
	{"row9 nest.ErrLockTimeout", remoteOutcomeNotApplied, remoteErrorIs(nest.ErrLockTimeout)},
	{"row9 nest.ErrEntityLockGroupChanged", remoteOutcomeNotApplied, remoteErrorIs(nest.ErrEntityLockGroupChanged)},
	{"row9 nest.ErrEntityGroupTransitionPending", remoteOutcomeNotApplied, remoteErrorIs(nest.ErrEntityGroupTransitionPending)},
	// 第 12 行：写任何持久记录之前被拒绝；本 fixture 的 handler 是 RollbackState，已整条回滚。
	{"row12 nest.ErrCommitRejected", remoteOutcomeNotApplied, remoteErrorIs(nest.ErrCommitRejected)},
	{"row14 nest.ErrNestTimeout", remoteOutcomeUncertain, remoteErrorIs(nest.ErrNestTimeout)},
	{"row14 nest.ErrNestCanceled", remoteOutcomeUncertain, remoteErrorIs(nest.ErrNestCanceled)},
	// 第 15 行里能确认的准入前拒绝：写许可 / 批次 / wrapper 容量（PrepareRemoteWriteBatch 在 handler 之前返回）、
	// fence 后不执行（表后说明）、派发队列满、停机。其余不命中任何行的错误不按第 15 行乐观处理，落入保守兜底。
	{"row15 entity.ErrRemoteOverloaded", remoteOutcomeNotApplied, remoteErrorIs(entity.ErrRemoteOverloaded)},
	{"row15 nest.ErrNestFenced", remoteOutcomeNotApplied, remoteErrorIs(nest.ErrNestFenced)},
	{"row15 nest.ErrQueueFull", remoteOutcomeNotApplied, remoteErrorIs(nest.ErrQueueFull)},
	{"row15 nest.ErrNestStopped", remoteOutcomeNotApplied, remoteErrorIs(nest.ErrNestStopped)},
}

// classifyRemoteLoadError 给一笔失败请求定结果语义；分不清的一律按结果未知（区间只会更宽，不会把真实提交判成不一致）。
func classifyRemoteLoadError(err error) (string, remoteErrorOutcome) {
	for _, rule := range remoteErrorRules {
		if rule.match(err) {
			return rule.name, rule.outcome
		}
	}
	return "unclassified", remoteOutcomeUncertain
}

func (o remoteErrorOutcome) String() string {
	if o == remoteOutcomeNotApplied {
		return "not_applied"
	}
	return "uncertain"
}

type remoteLoadSample struct {
	Seconds    float64               `json:"seconds"`
	Completed  uint64                `json:"completed"`
	Errors     uint64                `json:"errors"`
	Dropped    uint64                `json:"dropped"`
	HeapBytes  uint64                `json:"heap_bytes"`
	Goroutines int                   `json:"goroutines"`
	WAL        nestwal.Stats         `json:"wal"`
	Projection engine.ProjectorStats `json:"projection"`
	Nest       nest.DispatcherStats  `json:"nest"`
	Remote     remoteentity.Stats    `json:"remote"`
}
type remoteLoadReport struct {
	DurationSeconds                float64 `json:"duration_seconds"`
	Entities, Sessions, TargetRate int
	Completed, Errors, Dropped     uint64
	// Errors = ErrorsNotApplied + ErrorsUncertain + harness 自身错误（续订 Interest、写采样、ctx 结束，计入 ErrorClasses["harness"]）。
	ErrorsNotApplied, ErrorsUncertain uint64
	ErrorClasses                      map[string]uint64
	// ErrorClassFirst 记每个类别第一次出现的错误文本，用于核对分类（ErrorDetails 只留前 32 条，可能全是同一类）。
	ErrorClassFirst                                        map[string]string
	CompletionTPS                                          float64
	LatencyP50MS, LatencyP95MS, LatencyP99MS, LatencyMaxMS int64
	FirstError                                             string
	ErrorDetails                                           []remoteLoadError
	Metrics                                                []metrics.Metric
	Samples                                                []remoteLoadSample
}

type remoteLoadError struct {
	Entities   []int64
	Error      string
	ElapsedMS  int64
	Projection engine.ProjectorStats
}

// 固定速率输入独立于完成速度；有界队列满时计入 dropped，不能静默降速掩盖过载。
// 每个业务会话为一个 worker；这里不模拟客户端 TCP，也不把 Remote 写入当作 AOI 广播。
func runRemoteLoad(t *testing.T, ctx context.Context, scheduler *nest.NestMgr, name, warmup nest.HandlerName, ids []int64, keys []entity.RemoteSnapshotKey, receiver, owner *remoteentity.Manager, projector *engine.Projector, wal *nestwal.WAL) remoteLoadResult {
	t.Helper()
	sessions, rate := remoteInt("ROOST_REMOTE_SESSIONS", 1000), remoteInt("ROOST_REMOTE_RATE", 20)
	duration := remoteDuration()
	output := os.Getenv("ROOST_REMOTE_OUTPUT")
	if output == "" {
		t.Fatal("ROOST_REMOTE_OUTPUT required")
	}
	heapProfiles := newRemoteHeapProfiles(t, output)
	progress, err := os.Create(output + ".jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer progress.Close()
	encoder := json.NewEncoder(progress)
	// counts 只计成功回复；uncertain 计结果未知的失败请求。最终每个实体的期望区间是 [counts, counts+uncertain]。
	counts := make([]atomic.Int64, len(ids))
	uncertain := make([]atomic.Int64, len(ids))
	var completed, failed, dropped, notApplied, uncertainErrors atomic.Uint64
	var firstError string
	var errorDetails []remoteLoadError
	errorClasses := make(map[string]uint64)
	errorClassFirst := make(map[string]string)
	var errorMu sync.Mutex
	recordError := func(err error, class string) {
		failed.Add(1)
		errorMu.Lock()
		if firstError == "" {
			firstError = err.Error()
		}
		if errorClasses[class] == 0 {
			errorClassFirst[class] = err.Error()
		}
		errorClasses[class]++
		errorMu.Unlock()
	}
	// harness 自身的错误（续订、写采样、ctx 结束）不对应某笔业务请求，不影响区间，单独归类。
	recordHarnessError := func(err error) { recordError(err, "harness") }
	// 长稳期间继续续订，而不是人为延长 Interest TTL 到整个测试时长。
	renewCtx, cancelRenew := context.WithCancel(ctx)
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				for _, key := range keys {
					if err := receiver.RenewRemoteSnapshotInterest(renewCtx, key); err != nil {
						if renewCtx.Err() == nil {
							recordHarnessError(err)
							t.Errorf("renew Remote Interest: %v", err)
						}
						return
					}
				}
			}
		}
	}()
	// 输入结束不等于投影排空；继续续订到最终快照校验结束，不能在 async 积压期间退订。
	t.Cleanup(func() { cancelRenew(); <-renewDone })
	type job struct {
		pair     int
		due      time.Time
		measured bool
	}
	jobs := make(chan job, sessions)
	var workers sync.WaitGroup
	var histogram [60001]atomic.Uint64
	var maxLatency atomic.Int64
	warmSlots := make(chan struct{}, remoteInt("ROOST_REMOTE_WARM_CONCURRENCY", 16))
	// ROOST_REMOTE_REQUEST_TIMEOUT 只用于区间核验的负对照：调小后计时请求以调用方截止（判别表第 14 行）结束、事务仍可能提交。
	// 预热不受它影响，始终 30 秒。
	requestTimeout := 30 * time.Second
	if value := os.Getenv("ROOST_REMOTE_REQUEST_TIMEOUT"); value != "" {
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 {
			t.Fatal("positive ROOST_REMOTE_REQUEST_TIMEOUT required")
		}
		requestTimeout = d
	}
	request := func(j job) {
		if !j.measured {
			defer func() { <-warmSlots }()
		}
		pair := ids[j.pair*2 : j.pair*2+2]
		handler := name
		if !j.measured {
			handler = warmup
		}
		timeout := 30 * time.Second
		if j.measured {
			timeout = requestTimeout
		}
		callCtx, cancel := context.WithTimeout(ctx, timeout)
		requestStarted := time.Now()
		_, err := scheduler.RequestMulti(callCtx, handler, pair, nil)
		cancel()
		if err != nil {
			class, outcome := classifyRemoteLoadError(err)
			if outcome == remoteOutcomeNotApplied {
				notApplied.Add(1)
			} else {
				uncertainErrors.Add(1)
				uncertain[j.pair*2].Add(1)
				uncertain[j.pair*2+1].Add(1)
			}
			recordError(err, outcome.String()+" "+class)
			errorMu.Lock()
			if len(errorDetails) < 32 {
				errorDetails = append(errorDetails, remoteLoadError{append([]int64(nil), pair...), err.Error(), time.Since(requestStarted).Milliseconds(), projector.Stats()})
			}
			errorMu.Unlock()
			return
		}
		counts[j.pair*2].Add(1)
		counts[j.pair*2+1].Add(1)
		if j.measured {
			completed.Add(1)
			ms := time.Since(j.due).Milliseconds()
			histogram[min(ms, 60000)].Add(1)
			for old := maxLatency.Load(); ms > old && !maxLatency.CompareAndSwap(old, ms); old = maxLatency.Load() {
			}
		}
	}
	for range sessions {
		workers.Go(func() {
			for j := range jobs {
				request(j)
			}
		})
	}
	// 先把所有实体实际走过一次完整事务；预热不计入持续负载吞吐。
	for i := 0; i < len(ids)/2; i++ {
		warmSlots <- struct{}{}
		jobs <- job{pair: i}
	}
	for {
		total := int64(0)
		for i := range counts {
			total += counts[i].Load()
		}
		if total == int64(len(ids)) || failed.Load() > 0 {
			break
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			recordHarnessError(ctx.Err())
		}
	}
	if failed.Load() > 0 {
		close(jobs)
		workers.Wait()
		errorMu.Lock()
		message := firstError
		errorMu.Unlock()
		t.Fatalf("warmup: %s", message)
	}
	runtime.GC()
	report := remoteLoadReport{Entities: len(ids), Sessions: sessions, TargetRate: rate}
	started := time.Now()
	sample := func() {
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		s := remoteLoadSample{Seconds: time.Since(started).Seconds(), Completed: completed.Load(), Errors: failed.Load(), Dropped: dropped.Load(), HeapBytes: mem.HeapAlloc, Goroutines: runtime.NumGoroutine(), WAL: wal.Stats(), Projection: projector.Stats(), Remote: owner.Stats(), Nest: scheduler.Stats()}
		report.Samples = append(report.Samples, s)
		if err := encoder.Encode(s); err != nil {
			recordHarnessError(err)
		}
		t.Logf("load %.0fs completed=%d errors=%d dropped=%d heap=%d goroutines=%d unacked=%d", s.Seconds, s.Completed, s.Errors, s.Dropped, s.HeapBytes, s.Goroutines, s.Projection.WALUnacked)
	}
	sample()
	if os.Getenv("ROOST_REMOTE_PROFILE") == "1" {
		file, err := os.Create(output + ".cpu.pprof")
		if err != nil {
			t.Fatal(err)
		}
		if err := pprof.StartCPUProfile(file); err != nil {
			t.Fatal(err)
		}
		defer func() { pprof.StopCPUProfile(); file.Close() }()
	}
	progressTick := time.NewTicker(10 * time.Second)
	defer progressTick.Stop()
	interval := time.Second / time.Duration(rate)
	if interval <= 0 {
		t.Fatal("rate too large")
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	finish := time.NewTimer(duration)
	defer finish.Stop()
	sequence := 0
	emitUntil := func(until time.Time) {
		count := int(min(until.Sub(started), duration) / interval)
		for sequence < count {
			pairs := len(ids) / 2
			if os.Getenv("ROOST_REMOTE_SHAPE") == "hot" {
				pairs = min(pairs, 100)
			}
			j := job{pair: sequence % pairs, due: started.Add(time.Duration(sequence+1) * interval), measured: true}
			sequence++
			select {
			case jobs <- j:
			default:
				dropped.Add(1)
			}
		}
	}
loop:
	for {
		select {
		case <-tick.C:
			emitUntil(time.Now())
		case <-progressTick.C:
			sample()
			heapProfiles.writeDue(time.Since(started))
		case <-finish.C:
			emitUntil(started.Add(duration))
			break loop
		case <-ctx.Done():
			recordHarnessError(ctx.Err())
			break loop
		}
	}
	close(jobs)
	workers.Wait()
	sample()
	heapProfiles.writeEnd()
	report.DurationSeconds = time.Since(started).Seconds()
	report.Completed = completed.Load()
	report.Errors = failed.Load()
	report.Dropped = dropped.Load()
	report.CompletionTPS = float64(report.Completed) / report.DurationSeconds
	report.LatencyMaxMS = maxLatency.Load()
	errorMu.Lock()
	report.FirstError = firstError
	report.ErrorDetails = errorDetails
	report.ErrorClasses = errorClasses
	report.ErrorClassFirst = errorClassFirst
	errorMu.Unlock()
	report.ErrorsNotApplied = notApplied.Load()
	report.ErrorsUncertain = uncertainErrors.Load()
	report.Metrics = metrics.Snapshot()
	percentile := func(p uint64) int64 {
		target := (report.Completed*p + 99) / 100
		var n uint64
		for i := range histogram {
			n += histogram[i].Load()
			if n >= target {
				return int64(i)
			}
		}
		return 60000
	}
	report.LatencyP50MS = percentile(50)
	report.LatencyP95MS = percentile(95)
	report.LatencyP99MS = percentile(99)
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if report.Dropped != 0 {
		// dropped 是请求根本没发出（有界队列满），不影响区间，但仍按过载失败、不做核验（原口径）。
		t.Fatalf("load errors=%d dropped=%d first=%s; final consistency not verified", report.Errors, report.Dropped, report.FirstError)
	}
	if p := projector.Stats(); p.FatalProjectionConflicts != 0 {
		t.Errorf("fatal projection conflicts: %+v", p)
	}
	// 超时后事务可能仍会提交，成功回复次数只是下界：结果未知的请求把上界放宽，确定未生效的不计。
	// 负载错误不在这里失败，由调用方在最终核验写完 .verified 之后报告。
	result := remoteLoadResult{Expected: make(map[int64]remoteExpect, len(ids)), Entities: len(ids), Errors: report.Errors, NotApplied: report.ErrorsNotApplied, Uncertain: report.ErrorsUncertain, FirstError: report.FirstError}
	for i, id := range ids {
		low := counts[i].Load()
		result.Expected[id] = remoteExpect{Min: low, Max: low + uncertain[i].Load()}
		if uncertain[i].Load() != 0 {
			result.Widened++
		}
	}
	t.Logf("REMOTE_LOAD %s", raw)
	return result
}

// remoteHeapProfiles 是长稳内存调查用的 heap profile 开关（RR-20260930-03 的 C01 调查引入，
// 修复后正式保留）。默认关闭；设置 ROOST_REMOTE_HEAP_PROFILE_MINUTES="10,30,60" 后，
// 在负载开始后的对应分钟（随 10 秒采样检查，误差不超过一个采样周期）以及负载结束时，
// 先 runtime.GC 再写 <ROOST_REMOTE_OUTPUT>.heap-<N>m.pprof / .heap-end.pprof。
// 每次写入会多一次强制 GC 停顿，只用于内存调查，不与正式延迟验收混用。
type remoteHeapProfiles struct {
	t       *testing.T
	output  string
	minutes []int // 升序、去重，已写出的从头部移除
	enabled bool
}

func newRemoteHeapProfiles(t *testing.T, output string) *remoteHeapProfiles {
	t.Helper()
	p := &remoteHeapProfiles{t: t, output: output}
	spec := strings.TrimSpace(os.Getenv("ROOST_REMOTE_HEAP_PROFILE_MINUTES"))
	if spec == "" {
		return p
	}
	for part := range strings.SplitSeq(spec, ",") {
		minute, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || minute <= 0 {
			// 写错的时间点不静默跳过：否则长跑结束才发现少了 profile。
			t.Fatalf("ROOST_REMOTE_HEAP_PROFILE_MINUTES=%q: want comma-separated positive minutes", spec)
		}
		p.minutes = append(p.minutes, minute)
	}
	slices.Sort(p.minutes)
	p.minutes = slices.Compact(p.minutes)
	p.enabled = true
	return p
}

func (p *remoteHeapProfiles) writeDue(elapsed time.Duration) {
	for len(p.minutes) > 0 && elapsed >= time.Duration(p.minutes[0])*time.Minute {
		p.write(strconv.Itoa(p.minutes[0]) + "m")
		p.minutes = p.minutes[1:]
	}
}

func (p *remoteHeapProfiles) writeEnd() {
	if p.enabled {
		p.write("end")
	}
}

func (p *remoteHeapProfiles) write(tag string) {
	runtime.GC()
	path := p.output + ".heap-" + tag + ".pprof"
	file, err := os.Create(path)
	if err != nil {
		p.t.Errorf("heap profile %s: %v", tag, err)
		return
	}
	err = pprof.Lookup("heap").WriteTo(file, 0)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		p.t.Errorf("heap profile %s: %v", tag, err)
		return
	}
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	p.t.Logf("heap profile %s heap_alloc=%d heap_objects=%d path=%s", tag, mem.HeapAlloc, mem.HeapObjects, path)
}
