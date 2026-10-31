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
}
type callSeries struct {
	Count   CallCounts
	buckets [256]int64
	max     time.Duration
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
	// 固定指数桶，约 9% 相邻精度；分位数给桶上界并以实际 max 截断，不保存无限样本。
	bucket := 0
	if elapsed > time.Microsecond {
		bucket = min(255, int(math.Ceil(math.Log(float64(elapsed)/float64(time.Microsecond))/math.Log(1.09))))
	}
	series.buckets[bucket]++
	series.max = max(series.max, elapsed)
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
		quantile := func(q float64) float64 {
			target := int64(math.Ceil(float64(s.Count.Completed) * q))
			if target == 0 {
				return 0
			}
			var seen int64
			for i, n := range s.buckets {
				seen += n
				if seen >= target {
					upper := float64(time.Microsecond) * math.Pow(1.09, float64(i))
					if i == 255 {
						upper = float64(s.max)
					}
					return min(upper, float64(s.max)) / float64(time.Millisecond)
				}
			}
			return 0
		}
		out.Messages = append(out.Messages, CallReport{MessageID: id, Overflow: id == overflowMessageID, CallCounts: s.Count, Inflight: s.Count.Started - s.Count.Completed, P50MS: quantile(.5), P95MS: quantile(.95), P99MS: quantile(.99), MaxMS: float64(s.max) / float64(time.Millisecond)})
	}
	sort.Slice(out.Messages, func(i, j int) bool { return out.Messages[i].MessageID < out.Messages[j].MessageID })
	return out
}
