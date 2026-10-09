package health

import (
	"context"
	"errors"
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

// DefaultCheckTimeout 是每个 checker 的期限（维护者第十二轮决定，N01b 观察 O-H1）。
//
// Snapshot 并发调用全部 checker，各自最多等这么久；到期没返回的报 Fail（原因写明期限），不拖住
// 整个快照。取 1.5s：k8s 探针 timeoutSeconds 是 2s，并发之后整个 /readyz 在探针断开之前答完；
// Redis / Mongo / etcd 的 checker 自带 2s ping 超时，现在被这个期限截短。
const DefaultCheckTimeout = 1500 * time.Millisecond

type Registry struct {
	mu       sync.RWMutex
	checkers map[string]*registeredChecker
	timeout  time.Duration
}

// registeredChecker 持有一个 checker 和它正在进行的那次调用。
//
// 同一个 checker 同一时刻最多一次调用：卡住不返回的 checker 杀不掉，如果每次快照都新开一个，
// 每次探针就多留一个卡住的 goroutine。后来的快照等同一次调用（在自己的期限内），它返回后下一次
// 快照才重新调用。
type registeredChecker struct {
	checker Checker
	mu      sync.Mutex
	call    *checkCall
}

type checkCall struct {
	done    chan struct{}
	result  Result
	started time.Time
}

func NewRegistry() *Registry {
	return &Registry{checkers: make(map[string]*registeredChecker), timeout: DefaultCheckTimeout}
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
	r.checkers[name] = &registeredChecker{checker: checker}
	r.mu.Unlock()
}

// SetCheckTimeout 改每个 checker 的期限；d <= 0 恢复 DefaultCheckTimeout。
func (r *Registry) SetCheckTimeout(d time.Duration) {
	if r == nil {
		return
	}
	if d <= 0 {
		d = DefaultCheckTimeout
	}
	r.mu.Lock()
	r.timeout = d
	r.mu.Unlock()
}

func (r *Registry) Reset() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.checkers = make(map[string]*registeredChecker)
	r.mu.Unlock()
}

// Snapshot 并发调用全部 checker，每个最多等 r.timeout（ctx 先结束则以 ctx 为准），结果按名字排序。
// 到期没返回的 checker 记 Fail，Error 写明期限与已经跑了多久；它那次调用继续在后台跑到返回为止，
// 期间的快照不再新开调用。
func (r *Registry) Snapshot(ctx context.Context) Snapshot {
	if r == nil {
		return Snapshot{OK: true}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.RLock()
	names := make([]string, 0, len(r.checkers))
	checkers := make(map[string]*registeredChecker, len(r.checkers))
	for name, checker := range r.checkers {
		names = append(names, name)
		checkers[name] = checker
	}
	timeout := r.timeout
	r.mu.RUnlock()
	sort.Strings(names)

	now := time.Now()
	calls := make([]*checkCall, len(names))
	for i, name := range names {
		calls[i] = checkers[name].begin(ctx, name, timeout, now)
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	snap := Snapshot{OK: true, Results: make([]Result, 0, len(names))}
	for i, name := range names {
		result := calls[i].wait(waitCtx, name, timeout)
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

// begin 返回这个 checker 正在进行的调用，没有就开一个。调用的 ctx 脱离请求的取消（一个探针断开不该
// 让同时等这次调用的其他探针看到取消），只保留期限。
func (c *registeredChecker) begin(ctx context.Context, name string, timeout time.Duration, now time.Time) *checkCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.call != nil {
		return c.call
	}
	call := &checkCall{done: make(chan struct{}), started: now}
	c.call = call
	go func() {
		callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		defer cancel()
		call.result = checkOne(callCtx, name, c.checker, now.UnixMilli())
		c.mu.Lock()
		c.call = nil
		c.mu.Unlock()
		close(call.done)
	}()
	return call
}

func (call *checkCall) wait(ctx context.Context, name string, timeout time.Duration) Result {
	select {
	case <-call.done:
		return call.result
	case <-ctx.Done():
	}
	// 已经返回就用它的结果（期限与返回同时到达时不误报）。
	select {
	case <-call.done:
		return call.result
	default:
	}
	result := Result{Name: name, Status: StatusFail, CheckedAt: time.Now().UnixMilli()}
	if errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
		result.Message = "check timed out"
		result.Error = fmt.Sprintf("health check did not return within its deadline (per-check limit %s; in flight for %s)",
			timeout, time.Since(call.started).Round(time.Millisecond))
	} else {
		result.Message = "check abandoned"
		result.Error = fmt.Sprintf("health check abandoned: %v (in flight for %s)",
			context.Cause(ctx), time.Since(call.started).Round(time.Millisecond))
	}
	return result
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
