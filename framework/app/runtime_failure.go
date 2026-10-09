package app

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
)

// RuntimeFailure is the process-wide fail-stop signal for infrastructure that
// can no longer safely accept writes. The first failure wins and wakes App's
// normal graceful-shutdown path; later reports are joined for diagnostics.
//
// 首次失败时按登记顺序执行 OnFail 回调（例如 kit Nest Mod 登记的 NestMgr.Fence），全部执行完
// 才向 Done 投递，所以 run 醒来开始优雅停机时 Nest 已经拒绝新派发。
//
// 回调不放在 sync.Once 里执行：Once.Do 里的函数 panic 会让 Once 视为已完成、done 永远不投递，
// 回调里（间接）再调 Fail 会让 Once.Do 重入死锁。这里在互斥锁下判断并置位 failed、取出回调列表，
// 解锁后逐个执行（各自 recover），最后在 defer 里投递 done。
type RuntimeFailure struct {
	mu     sync.Mutex
	err    error // 全部失败（含回调 panic）的合并，供 Err 诊断
	first  error // 首次失败；投递给 Done，也传给失败之后才登记的回调
	failed bool
	hooks  []func(error) // 首次失败前登记、尚未执行的回调；失败时取出并清空
	done   chan error
}

func NewRuntimeFailure() *RuntimeFailure {
	return &RuntimeFailure{done: make(chan error, 1)}
}

// Fail 报告一次基础设施失败。第一次调用在当前 goroutine 上同步执行全部 OnFail 回调，再向 Done
// 投递；之后的调用只并入 Err 并立即返回（回调里再调 Fail 也走这条路，不会死锁）。
func (r *RuntimeFailure) Fail(err error) {
	if r == nil || err == nil {
		return
	}
	r.mu.Lock()
	r.err = errors.Join(r.err, err)
	if r.failed {
		r.mu.Unlock()
		return
	}
	r.failed = true
	r.first = err
	hooks := r.hooks
	r.hooks = nil
	r.mu.Unlock()

	defer func() { r.done <- err }()
	for _, hook := range hooks {
		r.runHook(hook, err)
	}
}

// OnFail 登记一个在首次失败时调用的回调（恰好一次，按登记顺序，同步执行）。回调运行在调用 Fail
// 的 goroutine 上，必须快速、不阻塞、不做 I/O。失败已经发生时登记，在登记者的 goroutine 上立即
// 调用（参数是首次失败）。判断“已失败”与追加列表在同一把锁下完成，与首次 Fail 并发时每个回调
// 仍然恰好调用一次；但失败之后才登记的回调不保证先于 Done 投递。
func (r *RuntimeFailure) OnFail(hook func(error)) {
	if r == nil || hook == nil {
		return
	}
	r.mu.Lock()
	if !r.failed {
		r.hooks = append(r.hooks, hook)
		r.mu.Unlock()
		return
	}
	first := r.first
	r.mu.Unlock()
	r.runHook(hook, first)
}

// runHook 执行一个回调；panic 记日志并并入 Err，不影响后面的回调与 Done 的投递。
func (r *RuntimeFailure) runHook(hook func(error), cause error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			panicErr := fmt.Errorf("app: runtime failure hook panicked: %v", recovered)
			slog.Error("runtime failure hook panicked", "err", panicErr, "cause", cause)
			r.mu.Lock()
			r.err = errors.Join(r.err, panicErr)
			r.mu.Unlock()
		}
	}()
	hook(cause)
}

func (r *RuntimeFailure) Done() <-chan error {
	if r == nil {
		return nil
	}
	return r.done
}

func (r *RuntimeFailure) Err() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}
