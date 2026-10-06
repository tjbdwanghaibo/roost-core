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
	if err := tp.workers[workerIndex].submitTask(task); err != nil {
		return fmt.Errorf("failed to submit task[%d]: %w", task.GetID(), err)
	}
	tp.totalTasks.Add(1)
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

func (tp *TaskPool) GetStats() (total, completed, failed int64, running bool) {
	return tp.totalTasks.Load(), tp.completedTasks.Load(),
		tp.failedTasks.Load(), tp.running.Load()
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
