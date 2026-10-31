package robot

import (
	"errors"
	"math"
	"sort"
	"sync"
	"time"
)

type SyncKind string

const (
	SyncFull   SyncKind = "full"
	SyncDelta  SyncKind = "delta"
	SyncRemove SyncKind = "remove"
)

// SyncKey 由场景关联一次预期更新。Connection 必须随重连递增；Sequence 是场景自己的
// 关联号，不把可合并的实体版本假装成每条修改必有一个网络包。
type SyncKey struct {
	Connection uint64
	Sequence   uint64
	Kind       SyncKind
}

type SyncLatency struct {
	Kind     SyncKind `json:"kind"`
	Expected int64    `json:"expected"`
	Received int64    `json:"received"`
	TimedOut int64    `json:"timed_out"`
	Canceled int64    `json:"canceled"`
	Pending  int64    `json:"pending"`
	P50MS    float64  `json:"p50_ms"`
	P95MS    float64  `json:"p95_ms"`
	P99MS    float64  `json:"p99_ms"`
	MaxMS    float64  `json:"max_ms"`
}
type SyncSlowSample struct {
	Connection uint64   `json:"connection"`
	Sequence   uint64   `json:"sequence"`
	Kind       SyncKind `json:"kind"`
	DurationMS float64  `json:"duration_ms"`
}
type SyncReport struct {
	Events           []SyncLatency    `json:"events"`
	Slowest          []SyncSlowSample `json:"slowest"`
	Unmatched        int64            `json:"unmatched"`
	CapacityRejected int64            `json:"capacity_rejected"`
}
type syncSeries struct {
	SyncLatency
	buckets [256]int64
	max     time.Duration
}
type syncExpectation struct{ start, deadline time.Time }

// SyncProbe 是一次运行的有界端到端观测器，不改 Sync 协议。场景在同一时钟域登记起点，
// 成功解码、应用并核对业务值后调用 Observe；收包但未应用不算成功。跨机服务器时间
// 不能直接与本机 time.Now 相减；场景必须说明起点是请求发起还是同机提交。
type SyncProbe struct {
	mu                  sync.Mutex
	limit               int
	pending             map[SyncKey]syncExpectation
	series              [3]syncSeries
	slow                []SyncSlowSample
	unmatched, rejected int64
}

func NewSyncProbe(maxPending int) (*SyncProbe, error) {
	if maxPending <= 0 {
		return nil, errors.New("robot sync: positive pending capacity required")
	}
	p := &SyncProbe{limit: maxPending, pending: make(map[SyncKey]syncExpectation)}
	for i, k := range []SyncKind{SyncFull, SyncDelta, SyncRemove} {
		p.series[i].Kind = k
	}
	return p, nil
}
func syncKindIndex(kind SyncKind) int {
	switch kind {
	case SyncFull:
		return 0
	case SyncDelta:
		return 1
	case SyncRemove:
		return 2
	default:
		return -1
	}
}
func (p *SyncProbe) Expect(key SyncKey, start time.Time, timeout time.Duration) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := syncKindIndex(key.Kind)
	if i < 0 || key.Connection == 0 || key.Sequence == 0 || start.IsZero() || timeout <= 0 {
		return errors.New("robot sync: invalid expectation")
	}
	p.expire(start)
	if _, ok := p.pending[key]; ok {
		return errors.New("robot sync: duplicate expectation")
	}
	if len(p.pending) >= p.limit {
		p.rejected++
		return errors.New("robot sync: pending capacity exceeded")
	}
	p.pending[key] = syncExpectation{start, start.Add(timeout)}
	p.series[i].Expected++
	p.series[i].Pending++
	return nil
}
func (p *SyncProbe) Observe(key SyncKey, applied time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expire(applied)
	e, ok := p.pending[key]
	if !ok {
		p.unmatched++
		return false
	}
	if applied.Before(e.start) {
		p.unmatched++
		return false
	}
	delete(p.pending, key)
	s := &p.series[syncKindIndex(key.Kind)]
	s.Pending--
	s.Received++
	elapsed := applied.Sub(e.start)
	s.max = max(s.max, elapsed)
	bucket := 0
	if elapsed > time.Microsecond {
		bucket = min(255, int(math.Ceil(math.Log(float64(elapsed)/float64(time.Microsecond))/math.Log(1.09))))
	}
	s.buckets[bucket]++
	p.slow = append(p.slow, SyncSlowSample{Connection: key.Connection, Sequence: key.Sequence, Kind: key.Kind, DurationMS: float64(elapsed) / float64(time.Millisecond)})
	sort.Slice(p.slow, func(i, j int) bool { return p.slow[i].DurationMS > p.slow[j].DurationMS })
	if len(p.slow) > 32 {
		p.slow = p.slow[:32]
	}
	return true
}

// CancelConnection 在连接关闭时结算未完成项；新连接不得完成旧连接的等待。
func (p *SyncProbe) CancelConnection(connection uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key := range p.pending {
		if key.Connection == connection {
			delete(p.pending, key)
			s := &p.series[syncKindIndex(key.Kind)]
			s.Pending--
			s.Canceled++
		}
	}
}
func (p *SyncProbe) expire(now time.Time) {
	for key, e := range p.pending {
		if !now.Before(e.deadline) {
			delete(p.pending, key)
			s := &p.series[syncKindIndex(key.Kind)]
			s.Pending--
			s.TimedOut++
		}
	}
}

// Snapshot 同时结算到期项，返回独立副本；零样本保持 Received=0，不代表延迟通过。
func (p *SyncProbe) Snapshot(now time.Time) SyncReport {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expire(now)
	r := SyncReport{Unmatched: p.unmatched, CapacityRejected: p.rejected, Slowest: append([]SyncSlowSample(nil), p.slow...)}
	for _, s := range p.series {
		v := s.SyncLatency
		quantile := func(q float64) float64 {
			if s.Received == 0 {
				return 0
			}
			target := int64(math.Ceil(float64(s.Received) * q))
			var count int64
			for i, n := range s.buckets {
				count += n
				if count >= target {
					return min(float64(s.max), float64(time.Microsecond)*math.Pow(1.09, float64(i))) / float64(time.Millisecond)
				}
			}
			return float64(s.max) / float64(time.Millisecond)
		}
		v.P50MS = quantile(.5)
		v.P95MS = quantile(.95)
		v.P99MS = quantile(.99)
		v.MaxMS = float64(s.max) / float64(time.Millisecond)
		r.Events = append(r.Events, v)
	}
	return r
}
