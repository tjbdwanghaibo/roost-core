package session

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Recorder 属于一次 Runner，而不是进程全局指标；重连共用它，新运行必须另建。
// 消息 ID、延迟桶与慢样本均有上限，报告不保存请求内容、玩家身份或票据。
type Recorder struct {
	mu    sync.Mutex
	calls map[uint32]*callSeries
	slow  []SlowCall
	steps map[string]*stepSeries
	// scenarios 是成功场景的耗时（FinishScenario），groups 按步骤组把它拆成组内与除去组两份，见 GroupReport。
	scenarios durationSeries
	groups    map[string]*groupSeries
}

// StepSample 是场景一个具名步骤跑完一次的结果（由 scenario.Step 填写）。
// Waited 只算场景 Wait 节点睡掉的时间；动作内部的等待（如 wait_push 的超时）算在 Elapsed 里、不算 Waited。
// RetryAttempts 是步骤内所有 Retry 节点实际执行子节点的次数之和：一次成功记 1，步骤里没有 Retry 记 0。
type StepSample struct {
	// Group 是步骤所属的组（scenario.StepInGroup / YAML 顶层步骤的 group），空表示不属于任何组。
	Group         string
	Elapsed       time.Duration
	Waited        time.Duration
	RetryAttempts int64
	OK            bool
}

// StepReport 是一个具名步骤在本次运行里的汇总。时长分位数覆盖成功与失败的全部样本；
// 失败的步骤也计入（它往往正是最慢的那次），由 OK / Failed 区分。
type StepReport struct {
	Name             string  `json:"name"`
	Group            string  `json:"group,omitempty"`
	Overflow         bool    `json:"overflow,omitempty"`
	Samples          int64   `json:"samples"`
	OK               int64   `json:"ok"`
	Failed           int64   `json:"failed"`
	MeanMS           float64 `json:"mean_ms"`
	P50MS            float64 `json:"p50_ms"`
	P95MS            float64 `json:"p95_ms"`
	MaxMS            float64 `json:"max_ms"`
	WaitMeanMS       float64 `json:"wait_mean_ms"`
	RetryAttemptsAvg float64 `json:"retry_attempts_avg"`
	RetryAttemptsMax int64   `json:"retry_attempts_max"`
}

type stepSeries struct {
	group         string
	latency       latencyBuckets
	ok, failed    int64
	total, waited time.Duration
	attempts      int64
	attemptsMax   int64
}

// overflowStepName 收容超过 256 个步骤名之后的样本：名字有上限，样本不丢。
const overflowStepName = "(overflow)"

// FinishStep 记一次步骤样本。
func (r *Recorder) FinishStep(name string, sample StepSample) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.steps == nil {
		r.steps = make(map[string]*stepSeries)
	}
	series := r.steps[name]
	if series == nil {
		if len(r.steps) >= 256 {
			name = overflowStepName
			series = r.steps[name]
		}
		if series == nil {
			series = &stepSeries{}
			if name != overflowStepName {
				series.group = sample.Group
			}
			r.steps[name] = series
		}
	}
	if sample.Group != "" {
		g := r.groupLocked(sample.Group)
		g.steps++
		if !sample.OK {
			g.stepsFailed++
		}
	}
	if sample.OK {
		series.ok++
	} else {
		series.failed++
	}
	series.latency.observe(sample.Elapsed)
	series.total += sample.Elapsed
	series.waited += sample.Waited
	series.attempts += sample.RetryAttempts
	series.attemptsMax = max(series.attemptsMax, sample.RetryAttempts)
}

// GroupReport 是一个步骤组在本次运行里的汇总。分位数只覆盖成功场景（与整场景耗时的门槛同一口径），
// 每个成功场景记一个样本：没跑这个组的场景组内时间记 0。
type GroupReport struct {
	// Scenarios 是成功场景数，即下面两组分位数的样本数。
	Scenarios int64 `json:"scenarios"`
	// Steps / StepsFailed 是该组步骤的执行与失败次数，含失败场景里的。
	Steps       int64 `json:"steps"`
	StepsFailed int64 `json:"steps_failed"`
	// GroupMS 是每个成功场景在该组步骤上花的时间合计；ExcludingMS 是场景时间减去它（按 0 截底）。
	GroupMS     ScenarioTimes `json:"group_ms"`
	ExcludingMS ScenarioTimes `json:"excluding_ms"`
}

// ScenarioTimes 是一组场景级时长的均值与分位数（毫秒，桶精度同步骤统计）。
type ScenarioTimes struct {
	MeanMS float64 `json:"mean_ms"`
	P50MS  float64 `json:"p50_ms"`
	P90MS  float64 `json:"p90_ms"`
	P95MS  float64 `json:"p95_ms"`
	P99MS  float64 `json:"p99_ms"`
	MaxMS  float64 `json:"max_ms"`
}

// overflowGroupName 收容超过 32 个组名之后的组。
const overflowGroupName = "(overflow)"

// FinishScenario 记一次成功场景：整场景耗时与本次在各组步骤上花的时间（robot.Context.GroupTimes）。
// 只给成功场景调用（Runner 如此），所以组报告的分位数与整场景耗时门槛同一口径。
func (r *Recorder) FinishScenario(elapsed time.Duration, groupTimes map[string]time.Duration) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// 先建出本次新出现的组（用此前的场景补齐，见 newGroupLocked），再记本次：每个已知组都记一个样本，
	// 没跑这个组的记 0。超出上限的几个组都落到溢出项上，时间合在一起。
	spent := make(map[*groupSeries]time.Duration, len(groupTimes))
	for name, d := range groupTimes {
		spent[r.groupLocked(name)] += d
	}
	r.scenarios.observe(elapsed)
	for _, g := range r.groups {
		d := spent[g]
		g.inGroup.observe(d)
		// Parallel 里并行的同组步骤各记各的，组时间可能超过场景时间，这里按 0 截底。
		g.excluding.observe(max(0, elapsed-d))
	}
}

// GroupSnapshot 返回各组的汇总；include 点名而从未出现的组也返回（组内时间全 0，除去组 = 整场景），
// 但不留在 Recorder 里。门槛点名的组在没有该组步骤的场景上因此等价于整场景，不会因为“没有样本”误判。
func (r *Recorder) GroupSnapshot(include ...string) map[string]GroupReport {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]GroupReport, len(r.groups)+len(include))
	for name, g := range r.groups {
		out[name] = g.report()
	}
	for _, name := range include {
		if _, ok := out[name]; !ok && name != "" {
			out[name] = r.newGroupLocked().report()
		}
	}
	return out
}

// groupSeries 是一个组的统计：每个成功场景在组内步骤上的时间、场景时间减去它，以及组内步骤的执行 / 失败次数。
type groupSeries struct {
	inGroup, excluding durationSeries
	steps, stepsFailed int64
}

func (g *groupSeries) report() GroupReport {
	return GroupReport{Scenarios: g.inGroup.count, Steps: g.steps, StepsFailed: g.stepsFailed, GroupMS: g.inGroup.times(), ExcludingMS: g.excluding.times()}
}

// groupLocked 取出或建出名为 name 的组；组名最多 32 个，之后的并进溢出项。
func (r *Recorder) groupLocked(name string) *groupSeries {
	if r.groups == nil {
		r.groups = make(map[string]*groupSeries)
	}
	if g := r.groups[name]; g != nil {
		return g
	}
	if len(r.groups) >= 32 {
		name = overflowGroupName
		if g := r.groups[name]; g != nil {
			return g
		}
	}
	g := r.newGroupLocked()
	r.groups[name] = g
	return g
}

// newGroupLocked 建一个新组并用此前的成功场景补齐：那些场景都没跑过这个组，组内时间各记 0，
// 除去组就是整场景耗时，直接复制整场景直方图——与从第一个场景起就记的结果完全一样。
func (r *Recorder) newGroupLocked() *groupSeries {
	g := &groupSeries{excluding: r.scenarios}
	g.inGroup.count = r.scenarios.count
	g.inGroup.latency.buckets[0] = r.scenarios.count
	return g
}

// durationSeries 是一组场景级时长：直方图加总和（求均值）。
type durationSeries struct {
	latency latencyBuckets
	count   int64
	total   time.Duration
}

func (d *durationSeries) observe(v time.Duration) {
	d.latency.observe(v)
	d.count++
	d.total += v
}

func (d *durationSeries) times() ScenarioTimes {
	if d.count == 0 {
		return ScenarioTimes{}
	}
	q := func(p float64) float64 { return d.latency.quantileMS(d.count, p) }
	return ScenarioTimes{
		MeanMS: float64(d.total) / float64(d.count) / float64(time.Millisecond),
		P50MS:  q(.5), P90MS: q(.9), P95MS: q(.95), P99MS: q(.99), MaxMS: d.latency.maxMS(),
	}
}

// StepSnapshot 按步骤名排序返回各步骤汇总（YAML 场景的步骤名以两位序号开头，排序即场景顺序）。
func (r *Recorder) StepSnapshot() []StepReport {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]StepReport, 0, len(r.steps))
	for name, s := range r.steps {
		n := s.ok + s.failed
		mean := func(d time.Duration) float64 { return float64(d) / float64(n) / float64(time.Millisecond) }
		out = append(out, StepReport{
			Name: name, Group: s.group, Overflow: name == overflowStepName, Samples: n, OK: s.ok, Failed: s.failed,
			MeanMS: mean(s.total), P50MS: s.latency.quantileMS(n, .5), P95MS: s.latency.quantileMS(n, .95), MaxMS: s.latency.maxMS(),
			WaitMeanMS: mean(s.waited), RetryAttemptsAvg: float64(s.attempts) / float64(n), RetryAttemptsMax: s.attemptsMax,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// latencyBuckets 是固定指数桶直方图（约 9% 相邻精度）：分位数给桶上界并以实际 max 截断，不保存无限样本。
type latencyBuckets struct {
	buckets [256]int64
	max     time.Duration
}

func (l *latencyBuckets) observe(elapsed time.Duration) {
	bucket := 0
	if elapsed > time.Microsecond {
		bucket = min(255, int(math.Ceil(math.Log(float64(elapsed)/float64(time.Microsecond))/math.Log(1.09))))
	}
	l.buckets[bucket]++
	l.max = max(l.max, elapsed)
}

// quantileMS 读 count 个样本中第 q 分位的毫秒数；count 必须等于 observe 的次数。
func (l *latencyBuckets) quantileMS(count int64, q float64) float64 {
	target := int64(math.Ceil(float64(count) * q))
	if target == 0 {
		return 0
	}
	var seen int64
	for i, n := range l.buckets {
		seen += n
		if seen >= target {
			upper := float64(time.Microsecond) * math.Pow(1.09, float64(i))
			if i == 255 {
				upper = float64(l.max)
			}
			return min(upper, float64(l.max)) / float64(time.Millisecond)
		}
	}
	return 0
}

func (l *latencyBuckets) maxMS() float64 { return float64(l.max) / float64(time.Millisecond) }

type callSeries struct {
	Count   CallCounts
	latency latencyBuckets
}
type CallCounts struct {
	Started   int64 `json:"started"`
	Sent      int64 `json:"sent"`
	Completed int64 `json:"completed"`
	OK        int64 `json:"ok"`
	Rejected  int64 `json:"rejected"`
	Timeout   int64 `json:"timeout"`
	Canceled  int64 `json:"canceled"`
	Errors    int64 `json:"errors"`
}
type CallReport struct {
	MessageID uint32 `json:"message_id"`
	Overflow  bool   `json:"overflow,omitempty"`
	CallCounts
	Inflight int64   `json:"inflight"`
	P50MS    float64 `json:"p50_ms"`
	P95MS    float64 `json:"p95_ms"`
	P99MS    float64 `json:"p99_ms"`
	MaxMS    float64 `json:"max_ms"`
}
type SlowCall struct {
	MessageID  uint32  `json:"message_id"`
	Outcome    string  `json:"outcome"`
	DurationMS float64 `json:"duration_ms"`
}
type Report struct {
	Messages []CallReport `json:"messages"`
	Slowest  []SlowCall   `json:"slowest"`
}

const overflowMessageID = ^uint32(0)

func (r *Recorder) begin(id uint32) *callSeries {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls == nil {
		r.calls = make(map[uint32]*callSeries)
	}
	series := r.calls[id]
	if series == nil {
		if len(r.calls) >= 256 {
			id = overflowMessageID
			series = r.calls[id]
		}
		if series == nil {
			series = &callSeries{}
			r.calls[id] = series
		}
	}
	series.Count.Started++
	return series
}
func (r *Recorder) sent(series *callSeries) {
	if r == nil || series == nil {
		return
	}
	r.mu.Lock()
	series.Count.Sent++
	r.mu.Unlock()
}
func (r *Recorder) finish(series *callSeries, id uint32, outcome string, elapsed time.Duration) {
	if r == nil || series == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	series.Count.Completed++
	switch outcome {
	case "ok":
		series.Count.OK++
	case "rejected":
		series.Count.Rejected++
	case "timeout":
		series.Count.Timeout++
	case "canceled":
		series.Count.Canceled++
	default:
		series.Count.Errors++
	}
	series.latency.observe(elapsed)
	sample := SlowCall{MessageID: id, Outcome: outcome, DurationMS: float64(elapsed) / float64(time.Millisecond)}
	if len(r.slow) < 32 {
		r.slow = append(r.slow, sample)
	} else if sample.DurationMS > r.slow[len(r.slow)-1].DurationMS {
		r.slow[len(r.slow)-1] = sample
	} else {
		return
	}
	sort.Slice(r.slow, func(i, j int) bool { return r.slow[i].DurationMS > r.slow[j].DurationMS })
}
func (r *Recorder) Snapshot() Report {
	if r == nil {
		return Report{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := Report{Slowest: append([]SlowCall(nil), r.slow...)}
	for id, s := range r.calls {
		done := s.Count.Completed
		out.Messages = append(out.Messages, CallReport{MessageID: id, Overflow: id == overflowMessageID, CallCounts: s.Count, Inflight: s.Count.Started - done,
			P50MS: s.latency.quantileMS(done, .5), P95MS: s.latency.quantileMS(done, .95), P99MS: s.latency.quantileMS(done, .99), MaxMS: s.latency.maxMS()})
	}
	sort.Slice(out.Messages, func(i, j int) bool { return out.Messages[i].MessageID < out.Messages[j].MessageID })
	return out
}
