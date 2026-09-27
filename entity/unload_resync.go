package entity

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	flog "github.com/tjbdwanghaibo/roost-core/log"
	"github.com/tjbdwanghaibo/roost-core/metrics"
)

// 卸载后重载（RR-20260926-59）。
//
// ManagerAccess.Unload 只卸载内存（RR-20260926-30 驱逐被 lease fence 跳过的原生步骤留下的实体、RR-20260926-39
// Remote 事务被持久拒绝后的实例共用它）。卸载关闭实例的同步状态，Sync 订阅保持登记、不发 remove；可订阅者手里
// 可能是从未成为权威的内容（Durability 1 推测性确认、原生步骤准入时已外发）。所以卸载时若该 subject 仍有订阅者，
// 框架主动从权威重载实体并 Rebind 全量；重载失败退回 remove：
//
//	Unload（快池持锁，或未装配 Nest 时就地）
//	  └─ schedule：只登记（按实体 ID 去重、有界队列），不做 I/O、不等待
//	重载 worker（快池之外的后台 goroutine，最多 Workers 个，队列空即退出）
//	  ├─ ManagerAccess.Get：与业务冷加载同一条共享加载（RR-20260926-54：与调用方 ctx 解耦、受框架加载上限
//	  │    与 loader 注销约束；正式 EntityRepository 先等该实体的在途投影/驱逐），发布经 Nest 绑定的
//	  │    BindLocalExecutor（NestMgr.RunLocal）回快池，OnEntityLoaded → Rebind 走既有正式路径
//	  ├─ 再 Rebind 一次（同一状态是空操作；覆盖不经 EntityRepository 的 loader）
//	  └─ 权威没有该实体 / 重载后仍不能服务订阅者 / 有界重试用尽 → RetractUnloadedSubject（订阅者收到 remove）
//
// 与业务并发：业务访问与重载经 ManagerAccess 的 singleflight 共用一次加载；业务先重载并 Register/Rebind 时
// subject 已不再等待，worker 直接结束。队列放不下（重载风暴）时立即退回 remove，订阅者不会停在错误内容上。
// 停机（stop）让 worker 立即离开在途等待并退出；取消不是重载失败，不发 remove。在途的共享加载本身按 RR-54 的
// 规则继续到完成或被 loader 注销（DataEngine 停机）取消，完成后照常发布，之后的访问直接命中。

// ErrAuthorityEntityNotFound 由 AggregateLoader 报告“权威里没有这个实体”（可 errors.Is 判别；
// DataEngine 的 ErrEntityAggregateNotFound 满足它）。卸载后重载据此立即退回 remove，不做重试。
var ErrAuthorityEntityNotFound = errors.New("entity: entity does not exist in its authority")

// UnloadedSubjectSync 是卸载后重载需要的 Sync 能力，entitysync.Manager 实现它。
type UnloadedSubjectSync interface {
	// SubjectAwaitsReload：subject 已登记、未退役、内容状态已关闭（实体被卸载），且仍有持有或等待该对象的订阅者。
	SubjectAwaitsReload(subjectID int64) bool
	// Rebind 把 subject 接到重载出来的新状态上，原订阅者下一帧收到全量。
	Rebind(state *SubjectSyncState) error
	// RetractUnloadedSubject 只在 subject 仍停在已关闭的状态上时注销它（订阅者收到 remove），返回是否注销。
	RetractUnloadedSubject(subjectID int64) bool
}

// UnloadResyncConfig 限定卸载后重载的资源；零值取默认。kit 配置键见 nest.unload_resync.*（RR-20260927-13）。
//
// 最坏延迟（RR-20260927-14）：一个实体从登记到得出结论（重载成功或退回 remove）最多
//
//	T_entity = Attempts × T_load + Σ_{k=1}^{Attempts-1} min(RetryMin·2^(k-1), RetryMax)
//
// T_load 是一次共享冷加载的框架上限（ManagerAccess.ConfigureLoadTimeout，默认 DefaultEntityLoadTimeout 30s）。队列按 FIFO
// 由 Workers 个 worker 处理，风暴中最后一个被接纳的实体最迟在 ceil((QueueCapacity+Workers)/Workers) × T_entity 后得出结论。
// 默认值（4 / 4096 / 5、100ms~2s、30s）：T_entity = 5×30s + 1.5s = 151.5s，上界 1025 × 151.5s ≈ 43.1 小时；加载快速失败时
// （T_load≈0）约 1025 × 1.5s ≈ 26 分钟。上界假设风暴之后没有新的卸载：处理中再次被卸载的实体会再排一轮。
// 期间订阅者停在旧内容上；积压见 gauge entity.unload_resync.backlog（进程内全部接线之和）与 UnloadResyncStats.Backlog（本接线）。
type UnloadResyncConfig struct {
	Workers       int           // 并发重载上限，默认 4
	QueueCapacity int           // 等待重载的实体数上限，默认 4096；放不下的立即退回 remove
	Attempts      int           // 每个实体的重载尝试次数，默认 5；用尽退回 remove
	RetryMin      time.Duration // 重试退避下限，默认 100ms，逐次翻倍
	RetryMax      time.Duration // 重试退避上限，默认 2s
	// 单次重载没有单独的截止时间：它就是一次共享冷加载，受 ManagerAccess 的框架加载上限
	// （ConfigureLoadTimeout，默认 DefaultEntityLoadTimeout）与 loader 注销约束。
}

func (config UnloadResyncConfig) normalized() UnloadResyncConfig {
	if config.Workers <= 0 {
		config.Workers = 4
	}
	if config.QueueCapacity <= 0 {
		config.QueueCapacity = 4096
	}
	if config.Attempts <= 0 {
		config.Attempts = 5
	}
	if config.RetryMin <= 0 {
		config.RetryMin = 100 * time.Millisecond
	}
	if config.RetryMax < config.RetryMin {
		config.RetryMax = max(config.RetryMin, 2*time.Second)
	}
	return config
}

// UnloadResyncStats 是卸载后重载的累计计数。Scheduled 是登记的重载，Reloaded 是订阅者已接上权威状态，
// Retracted 是退回 remove（含 Overflow），Failures 是失败的重载尝试，Overflow 是队列放不下的卸载。
// Backlog 是本接线此刻等待或正在重载的实体数（不是累计值，RR-20260927-14）；gauge entity.unload_resync.backlog
// 是进程内全部接线的 Backlog 之和（RR-20260928-01），一个进程只有一个 ManagerAccess 时两者相同。
type UnloadResyncStats struct {
	Scheduled uint64
	Reloaded  uint64
	Retracted uint64
	Failures  uint64
	Overflow  uint64
	Backlog   int
}

// unloadResyncBacklogMetric 是卸载后重载的积压 gauge：进程内全部接线等待或正在重载的实体数之和。无标签，
// 一个进程一条序列（RR-20260927-14）；每个接线停止时撤回自己的部分，全部停止后为 0。
const unloadResyncBacklogMetric = "entity.unload_resync.backlog"

// unloadResyncBacklog 汇总进程内全部接线的积压（RR-20260928-01）。一个进程可以有多个 ManagerAccess（多个
// EntityManager、测试或工具进程），原来每个接线直接写自己的 len(jobs)，gauge 变成最后写入者的值，一个接线停止
// 写 0 还会抹掉其余接线的积压。现在每个接线记住自己已计入的份额（unloadResync.published），变化时只把差值记进
// total，并在同一把锁下写出 total——加法与写出在一起，才不会被并发的另一个接线用旧和覆盖。
// 锁是叶子锁（调用方持有 unloadResync.mu，锁内只做加法和一次 SetGauge），不做 I/O、不等待其他任务。
var unloadResyncBacklog struct {
	mu    sync.Mutex
	total int64
}

type resyncJobState uint8

const (
	resyncQueued  resyncJobState = iota + 1
	resyncRunning                // worker 正在处理
	resyncRerun                  // 处理期间同一实体又被卸载：结束后再来一轮
)

type unloadResync struct {
	access *ManagerAccess
	target UnloadedSubjectSync
	config UnloadResyncConfig
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	queue   []int64
	jobs    map[int64]resyncJobState
	workers int
	stopped bool
	// published 是本接线已计入 unloadResyncBacklog.total 的份额（RR-20260928-01）。
	published int64

	scheduled, reloaded, retracted, failures, overflow atomic.Uint64
}

// ConfigureUnloadResync 接上卸载后重载：target 通常是 entitysync.Manager。重载的发布走 ManagerAccess 已有的
// 本地执行入口——Nest 构造时经 BindLocalExecutor 绑定的 NestMgr.RunLocal；未绑定（没有 Nest）时就地执行。
// kit 的 Nest Mod 在启动时接好、停止时先于 Nest 调用返回的 stop。stop 让 worker 离开在途等待并退出（受 ctx
// 限制）；一个 ManagerAccess 同时只能接一个，已停止的可以被替换。
func (access *ManagerAccess) ConfigureUnloadResync(target UnloadedSubjectSync, config UnloadResyncConfig) (func(context.Context) error, error) {
	if access == nil || access.manager == nil {
		return nil, ErrEntityNotManaged
	}
	if target == nil {
		return nil, errors.New("entity manager access: unload resync needs a sync target")
	}
	ctx, cancel := context.WithCancel(context.Background())
	resync := &unloadResync{access: access, target: target, config: config.normalized(), ctx: ctx, cancel: cancel, jobs: make(map[int64]resyncJobState)}
	// 已停止的上一份接线可以被替换（Nest Mod 重启）；它的计数保留到被替换为止。
	current := access.resync.Load()
	if (current != nil && !current.isStopped()) || !access.resync.CompareAndSwap(current, resync) {
		cancel()
		return nil, errors.New("entity manager access: unload resync is already configured")
	}
	return resync.stop, nil
}

// UnloadResyncStats 返回当前接线的累计计数；未接线时为零值。
func (access *ManagerAccess) UnloadResyncStats() UnloadResyncStats {
	if access == nil {
		return UnloadResyncStats{}
	}
	resync := access.resync.Load()
	if resync == nil {
		return UnloadResyncStats{}
	}
	resync.mu.Lock()
	backlog := len(resync.jobs)
	resync.mu.Unlock()
	return UnloadResyncStats{
		Scheduled: resync.scheduled.Load(), Reloaded: resync.reloaded.Load(), Retracted: resync.retracted.Load(),
		Failures: resync.failures.Load(), Overflow: resync.overflow.Load(), Backlog: backlog,
	}
}

// publishBacklogLocked 在 jobs 变化后更新积压 gauge（调用方持有 r.mu）。jobs 按实体去重，含排队、处理中与“再来一轮”。
// gauge 是进程内全部接线之和：这里只调整本接线的份额（RR-20260928-01）。
func (r *unloadResync) publishBacklogLocked() {
	contribution := int64(len(r.jobs))
	unloadResyncBacklog.mu.Lock()
	unloadResyncBacklog.total += contribution - r.published
	r.published = contribution
	metrics.SetGauge(unloadResyncBacklogMetric, nil, unloadResyncBacklog.total)
	unloadResyncBacklog.mu.Unlock()
}

// schedule 在 Unload 里调用（快池）：只查订阅、登记、按需启动 worker，不做 I/O、不等待。
func (r *unloadResync) schedule(id int64) {
	if !r.target.SubjectAwaitsReload(id) {
		return
	}
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	switch r.jobs[id] {
	case resyncQueued, resyncRerun:
		r.mu.Unlock()
		return
	case resyncRunning:
		r.jobs[id] = resyncRerun
		r.mu.Unlock()
		return
	}
	if len(r.queue) >= r.config.QueueCapacity {
		r.mu.Unlock()
		r.overflow.Add(1)
		flog.Warn("entity: unload resync queue is full; subscribers get a remove instead of a reload", "entity", id, "capacity", r.config.QueueCapacity)
		r.retract(id)
		return
	}
	r.jobs[id] = resyncQueued
	r.queue = append(r.queue, id)
	r.scheduled.Add(1)
	r.publishBacklogLocked()
	if r.workers < r.config.Workers {
		r.workers++
		r.wg.Add(1)
		go r.work()
	}
	r.mu.Unlock()
}

func (r *unloadResync) work() {
	defer r.wg.Done()
	for {
		r.mu.Lock()
		if r.stopped || len(r.queue) == 0 {
			r.workers--
			r.mu.Unlock()
			return
		}
		id := r.queue[0]
		r.queue = r.queue[1:]
		r.jobs[id] = resyncRunning
		r.mu.Unlock()

		r.resync(id)

		r.mu.Lock()
		if r.jobs[id] == resyncRerun && !r.stopped {
			r.jobs[id] = resyncQueued
			r.queue = append(r.queue, id)
		} else {
			delete(r.jobs, id)
			r.publishBacklogLocked()
		}
		r.mu.Unlock()
	}
}

type resyncOutcome uint8

const (
	resyncDone   resyncOutcome = iota // 订阅者已接上权威状态（或已不再等待）
	resyncAbsent                      // 权威里没有它，或重载出来的实例不能服务订阅者：退回 remove
	resyncRetry                       // 这次失败，按退避重试
)

// resync 对一个实体做有界重试。每轮先确认 subject 仍在等：业务已先重载并重新绑定、订阅者都已离开或
// subject 已退役时直接结束。
func (r *unloadResync) resync(id int64) {
	backoff := r.config.RetryMin
	for attempt := 1; ; attempt++ {
		if r.ctx.Err() != nil || !r.target.SubjectAwaitsReload(id) {
			return
		}
		outcome, err := r.reloadOnce(id)
		switch outcome {
		case resyncDone:
			r.reloaded.Add(1)
			return
		case resyncAbsent:
			flog.Info("entity: unloaded entity cannot be reloaded for its subscribers; sending remove", "entity", id, "err", err)
			r.retract(id)
			return
		}
		if r.ctx.Err() != nil {
			return // 停机取消，不是重载失败
		}
		r.failures.Add(1)
		if attempt >= r.config.Attempts {
			flog.Warn("entity: reloading an unloaded entity for its subscribers kept failing; sending remove", "entity", id, "attempts", attempt, "err", err)
			r.retract(id)
			return
		}
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-r.ctx.Done():
			timer.Stop()
			return
		}
		backoff = min(backoff*2, r.config.RetryMax)
	}
}

func (r *unloadResync) reloadOnce(id int64) (resyncOutcome, error) {
	ctx := r.ctx
	// 领头时加载里的发布走这个执行器；worker 离开（停机）后共享加载改走同一个绑定入口（RR-54）。
	if run := r.access.localExecutor.Load(); run != nil {
		ctx = WithLocalExecutor(ctx, *run)
	}
	value, err := r.access.Get(ctx, id, EntityCategoryNone)
	switch {
	case errors.Is(err, ErrAuthorityEntityNotFound):
		return resyncAbsent, err
	case err != nil:
		return resyncRetry, err
	case value == nil:
		return resyncAbsent, errors.New("loader returned no entity")
	}
	var state *SubjectSyncState
	if base := value.Base(); base != nil {
		state = base.Sync()
	}
	var rebindErr error
	if state.Enabled() {
		// 正式 EntityRepository 已在发布时经 OnEntityLoaded 重新绑定，这里是同一状态的空操作。
		rebindErr = r.target.Rebind(state)
	}
	if !r.target.SubjectAwaitsReload(id) {
		return resyncDone, nil
	}
	if value.IsRemoved() {
		return resyncRetry, errors.New("reloaded instance was unloaded again")
	}
	if !state.Enabled() {
		return resyncAbsent, errors.New("reloaded entity has no sync state")
	}
	return resyncRetry, rebindErr
}

func (r *unloadResync) retract(id int64) {
	if r.target.RetractUnloadedSubject(id) {
		r.retracted.Add(1)
	}
}

func (r *unloadResync) isStopped() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopped
}

func (r *unloadResync) stop(ctx context.Context) error {
	r.mu.Lock()
	r.stopped = true
	r.queue = nil
	// 丢弃的排队不再重载；处理中的 worker 结束时按 jobs 缺项处理（delete 空操作），不会再登记。
	clear(r.jobs)
	r.publishBacklogLocked()
	r.mu.Unlock()
	r.cancel()
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
