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
}

// StepSample 是场景一个具名步骤跑完一次的结果（由 scenario.Step 填写）。
// Waited 只算场景 Wait 节点睡掉的时间；动作内部的等待（如 wait_push 的超时）算在 Elapsed 里、不算 Waited。
// RetryAttempts 是步骤内所有 Retry 节点实际执行子节点的次数之和：一次成功记 1，步骤里没有 Retry 记 0。
type StepSample struct {
	Elapsed       time.Duration
	Waited        time.Duration
	RetryAttempts int64
	OK            bool
}

// StepReport 是一个具名步骤在本次运行里的汇总。时长分位数覆盖成功与失败的全部样本；
// 失败的步骤也计入（它往往正是最慢的那次），由 OK / Failed 区分。
type StepReport struct {
	Name             string  `json:"name"`
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
			r.steps[name] = series
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
			Name: name, Overflow: name == overflowStepName, Samples: n, OK: s.ok, Failed: s.failed,
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
