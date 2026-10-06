package goroutine

import (
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

type Task interface {
	GetID() int64
	Execute() error
	String() string
}

type TaskFunc struct {
	ID   int64
	Func func() error
	Desc string
}

func (t *TaskFunc) GetID() int64   { return t.ID }
func (t *TaskFunc) Execute() error { return t.Func() }
func (t *TaskFunc) String() string {
	if t.Desc != "" {
		return t.Desc
	}
	return fmt.Sprintf("Task[%d]", t.ID)
}

type TaskPoolConfig struct {
	WorkerCount     int
	MaxTaskCount    int
	ShutdownTimeout time.Duration
}

func DefaultTaskPoolConfig() *TaskPoolConfig {
	return &TaskPoolConfig{
		WorkerCount:     4,
		MaxTaskCount:    1000,
		ShutdownTimeout: 30 * time.Second,
	}
}

// 测试缝（RR-20261006-20）：只在测试里赋值，生产恒为 nil（每次 Submit / GetStats 多一次 nil 判断）。
// 用来在“任务已进 channel”之后、“读第一个计数”之后停住，确定性地造出统计读写交错。
var (
	submitEnqueuedHook   func() // Submit：任务已进 worker channel 之后
	statsFirstLoadedHook func() // GetStats：读完结束计数之后、读 total 之前
)

type TaskPool struct {
	config  *TaskPoolConfig
	workers []*taskWorker
	running atomic.Bool
	wg      sync.WaitGroup
	// lifeMu 串行化 Start 与 Shutdown 的状态转换；shut 置位后池不能再启动（一次性）。
	// 旧实现用 closeOnce：从未 Start 就 Shutdown 时 once 已执行却没关 worker，之后 Start 起的 worker
	// 再也关不掉；Shutdown 之后 Start 又会把池标成 running、每次 Submit 却都被拒（RR-20261005-NC-269）。
	lifeMu sync.Mutex
	shut   bool

	totalTasks     atomic.Int64
	completedTasks atomic.Int64
	failedTasks    atomic.Int64
}

func NewTaskPool(config *TaskPoolConfig) *TaskPool {
	defaults := DefaultTaskPoolConfig()
	if config == nil {
		config = defaults
	} else {
		// Normalize a partially filled config instead of trusting it: a
		// zero WorkerCount used to build an empty worker slice and made
		// every Submit divide by zero. The caller's struct is not mutated.
		normalized := *config
		if normalized.WorkerCount <= 0 {
			normalized.WorkerCount = defaults.WorkerCount
		}
		if normalized.MaxTaskCount <= 0 {
			normalized.MaxTaskCount = defaults.MaxTaskCount
		}
		if normalized.ShutdownTimeout <= 0 {
			normalized.ShutdownTimeout = defaults.ShutdownTimeout
		}
		config = &normalized
	}
	pool := &TaskPool{
		config:  config,
		workers: make([]*taskWorker, config.WorkerCount),
	}
	for i := 0; i < config.WorkerCount; i++ {
		pool.workers[i] = newTaskWorker(i, config.MaxTaskCount/config.WorkerCount)
	}
	return pool
}

// Start 启动 worker。重复调用无效；Shutdown 之后调用无效（池是一次性的）。
func (tp *TaskPool) Start() {
	tp.lifeMu.Lock()
	defer tp.lifeMu.Unlock()
	if tp.shut || !tp.running.CompareAndSwap(false, true) {
		return
	}
	for _, w := range tp.workers {
		tp.wg.Add(1)
		go func(worker *taskWorker) {
			defer tp.wg.Done()
			worker.run(tp)
		}(w)
	}
}

func (tp *TaskPool) Submit(task Task) error {
	if !tp.running.Load() {
		return fmt.Errorf("task pool is not running")
	}
	workerIndex := tp.hashTaskID(task.GetID())
	// 先计数再入队（RR-20261006-20）：任务一进 channel 就可能被执行完、计入 completed / failed，
	// 入队之后才加 total 时，读统计会瞬间看到“结束数 > 提交数”。被拒绝时撤回这一次计数，
	// 所以 total 可能短暂多算一个正在被拒绝的提交，但不会少算已结束的任务。
	tp.totalTasks.Add(1)
	if err := tp.workers[workerIndex].submitTask(task); err != nil {
		tp.totalTasks.Add(-1)
		return fmt.Errorf("failed to submit task[%d]: %w", task.GetID(), err)
	}
	if h := submitEnqueuedHook; h != nil {
		h()
	}
	return nil
}

func (tp *TaskPool) SubmitFunc(taskID int64, desc string, fn func() error) error {
	return tp.Submit(&TaskFunc{ID: taskID, Func: fn, Desc: desc})
}

func (tp *TaskPool) Shutdown() error {
	return tp.ShutdownWithTimeout(tp.config.ShutdownTimeout)
}

func (tp *TaskPool) ShutdownWithTimeout(timeout time.Duration) error {
	tp.lifeMu.Lock()
	if !tp.shut {
		tp.shut = true
		tp.running.Store(false)
		for _, w := range tp.workers {
			w.close() // 从未启动的 worker 也关掉：之后的 Start 被拒，不会再起读它的 goroutine
		}
	}
	tp.lifeMu.Unlock()

	done := make(chan struct{})
	go func() {
		tp.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("task pool shutdown timeout after %v", timeout)
	}
}

// GetStats 返回提交、完成、失败计数与是否在运行。三个计数各自是原子量，不是同一时刻的快照，
// 但保证 completed + failed ≤ total（RR-20261006-20）：Submit 先加 total 再入队，任务结束才加
// completed / failed；这里先读结束数、后读 total，读到的每个结束任务的提交计数都已可见。
// 没有用一把锁包住计数和读取：那会让每个任务的提交与完成都去抢同一把锁，只为统计一致。
func (tp *TaskPool) GetStats() (total, completed, failed int64, running bool) {
	completed = tp.completedTasks.Load()
	failed = tp.failedTasks.Load()
	if h := statsFirstLoadedHook; h != nil {
		h()
	}
	total = tp.totalTasks.Load()
	return total, completed, failed, tp.running.Load()
}

func (tp *TaskPool) IsRunning() bool {
	return tp.running.Load()
}

func (tp *TaskPool) hashTaskID(taskID int64) int {
	h := fnv.New32a()
	h.Write([]byte(fmt.Sprintf("%d", taskID)))
	// Modulo in uint32 space: int(h.Sum32()) is negative on 32-bit ints and
	// a negative index panics.
	return int(h.Sum32() % uint32(len(tp.workers)))
}

type taskWorker struct {
	id       int
	taskChan chan Task
	// closeMu 让“检查 closed + 发送”与“置 closed + 关闭 channel”互斥
	// （RR-20261005-NC-183）：之前两步之间 Shutdown 关闭 channel，Submit 向已
	// 关闭的 channel 发送即 panic。发送是非阻塞的，读锁不会挡住 close 太久。
	// 与 worker.Worker.closeMu 同一做法：受理即进入 channel，必被排空执行。
	closeMu sync.RWMutex
	closed  atomic.Bool
}

func newTaskWorker(id int, bufferSize int) *taskWorker {
	if bufferSize <= 0 {
		bufferSize = 100
	}
	return &taskWorker{
		id:       id,
		taskChan: make(chan Task, bufferSize),
	}
}

func (w *taskWorker) submitTask(task Task) error {
	w.closeMu.RLock()
	defer w.closeMu.RUnlock()
	if w.closed.Load() {
		return fmt.Errorf("worker[%d] is closed", w.id)
	}
	select {
	case w.taskChan <- task:
		return nil
	default:
		return fmt.Errorf("worker[%d] task queue is full", w.id)
	}
}

func (w *taskWorker) close() {
	w.closeMu.Lock()
	defer w.closeMu.Unlock()
	if w.closed.CompareAndSwap(false, true) {
		close(w.taskChan)
	}
}

func (w *taskWorker) run(pool *TaskPool) {
	for task := range w.taskChan {
		w.executeTask(task, pool)
	}
}

func (w *taskWorker) executeTask(task Task, pool *TaskPool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("TaskPool worker panic", "worker", w.id, "task", task.GetID(), "err", r)
			pool.failedTasks.Add(1)
		}
	}()

	err := task.Execute()
	if err != nil {
		slog.Error("TaskPool task failed", "worker", w.id, "task", task.String(), "err", err)
		pool.failedTasks.Add(1)
	} else {
		pool.completedTasks.Add(1)
	}
}
