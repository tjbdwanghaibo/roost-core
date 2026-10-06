package health

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

type Status string

const (
	StatusOK       Status = "ok"
	StatusFail     Status = "fail"
	StatusDegraded Status = "degraded"
)

type Result struct {
	Name      string `json:"name"`
	Status    Status `json:"status"`
	Message   string `json:"message,omitempty"`
	Error     string `json:"error,omitempty"`
	CheckedAt int64  `json:"checked_at_ms"`
	Err       error  `json:"-"`
}

// Snapshot 是一次健康检查的聚合结果。
//
// 聚合规则（维护者决定 D1，2026-10-06）：Degraded 算可用。OK 为真表示没有任何 checker 为
// Fail——Degraded 不影响 OK，只置 Degraded；Fail 与未知的 Status 一律让 OK 为假。Degraded 的
// 含义是“还能服务、需要关注”（单实例锁续期结果未知、容量接近上限、写许可用满、积压告警），
// Fail 是“不能再安全工作”。kit/ops 的 /readyz 据此只在 Fail（或就绪位为假）时返回 503，
// 并在响应体里列出降级项。
type Snapshot struct {
	OK       bool     `json:"ok"`
	Degraded bool     `json:"degraded"`
	Results  []Result `json:"results"`
}

// DegradedResults 返回状态为 Degraded 的结果（按名字排序，与 Results 同序）。
func (s Snapshot) DegradedResults() []Result {
	var degraded []Result
	for _, result := range s.Results {
		if result.Status == StatusDegraded {
			degraded = append(degraded, result)
		}
	}
	return degraded
}

type Checker interface {
	CheckHealth(context.Context) Result
}

type CheckerFunc func(context.Context) Result

func (f CheckerFunc) CheckHealth(ctx context.Context) Result {
	if f == nil {
		return Result{Status: StatusFail, Message: "checker is nil"}
	}
	return f(ctx)
}

type Registry struct {
	mu       sync.RWMutex
	checkers map[string]Checker
}

func NewRegistry() *Registry {
	return &Registry{checkers: make(map[string]Checker)}
}

var defaultRegistry = NewRegistry()

func DefaultRegistry() *Registry {
	return defaultRegistry
}

func Register(name string, checker Checker) {
	defaultRegistry.Register(name, checker)
}

func Check(ctx context.Context) Snapshot {
	return defaultRegistry.Snapshot(ctx)
}

func (r *Registry) Register(name string, checker Checker) {
	if r == nil || name == "" || checker == nil {
		return
	}
	r.mu.Lock()
	r.checkers[name] = checker
	r.mu.Unlock()
}

func (r *Registry) Reset() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.checkers = make(map[string]Checker)
	r.mu.Unlock()
}

func (r *Registry) Snapshot(ctx context.Context) Snapshot {
	if r == nil {
		return Snapshot{OK: true}
	}
	r.mu.RLock()
	names := make([]string, 0, len(r.checkers))
	checkers := make(map[string]Checker, len(r.checkers))
	for name, checker := range r.checkers {
		names = append(names, name)
		checkers[name] = checker
	}
	r.mu.RUnlock()
	sort.Strings(names)

	snap := Snapshot{OK: true, Results: make([]Result, 0, len(names))}
	now := time.Now().UnixMilli()
	for _, name := range names {
		result := checkOne(ctx, name, checkers[name], now)
		switch result.Status {
		case StatusOK:
		case StatusDegraded:
			snap.Degraded = true
		default:
			snap.OK = false
		}
		snap.Results = append(snap.Results, result)
	}
	return snap
}

func checkOne(ctx context.Context, name string, checker Checker, now int64) (result Result) {
	defer func() {
		if r := recover(); r != nil {
			result = Result{
				Name:      name,
				Status:    StatusFail,
				Error:     fmt.Sprintf("panic: %v", r),
				CheckedAt: now,
			}
		}
	}()
	result = checker.CheckHealth(ctx)
	result.Name = name
	if result.Status == "" {
		result.Status = StatusOK
	}
	if result.Err != nil {
		result.Error = result.Err.Error()
	}
	if result.CheckedAt == 0 {
		result.CheckedAt = now
	}
	return result
}
