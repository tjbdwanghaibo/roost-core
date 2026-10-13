package nest

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/goroutine"
)

var (
	tickMu sync.RWMutex
	// tickCbSeen 按名字去重；tickCbList 保持注册顺序，让回调执行顺序确定（按 map 遍历会让回调间顺序随进程变化）。
	tickCbSeen = make(map[TickCallbackName]struct{})
	tickCbList []func(msg TickMsg)
)

// TickCallbackName 是帧回调的唯一名字。
type TickCallbackName struct {
	value string
}

func NewTickCallbackName(value string) TickCallbackName {
	return TickCallbackName{value: value}
}

func (n TickCallbackName) String() string {
	return n.value
}

// RegisterTickCallback 注册一个每帧执行的回调（包级，所有 Ticker 共用）；重名返回错误。引擎启动后注册的回调
// 从下一帧开始生效。
func RegisterTickCallback(name TickCallbackName, cb func(msg TickMsg)) error {
	tickMu.Lock()
	defer tickMu.Unlock()
	if _, exist := tickCbSeen[name]; exist {
		return fmt.Errorf("nest: duplicate tick callback %q", name.String())
	}
	tickCbSeen[name] = struct{}{}
	// 注册时复制并发布，已取得旧列表的 tick 可以无锁继续执行。
	callbacks := make([]func(TickMsg), len(tickCbList)+1)
	copy(callbacks, tickCbList)
	callbacks[len(tickCbList)] = cb
	tickCbList = callbacks
	return nil
}

// MustRegisterTickCallback 是 RegisterTickCallback 的 panic 版本。
func MustRegisterTickCallback(name TickCallbackName, cb func(msg TickMsg)) {
	if err := RegisterTickCallback(name, cb); err != nil {
		panic(err)
	}
}

// snapshotTickCallbacks 返回只读快照，调用者不得改写切片。
// 注册时发布新数组，故 callback 内注册新 callback 不会影响本轮或造成锁重入。
func snapshotTickCallbacks() []func(msg TickMsg) {
	tickMu.RLock()
	defer tickMu.RUnlock()
	return tickCbList
}

// Ticker 是帧驱动的计时器：按固定间隔递增帧号并依次执行已注册的帧回调（每个回调独立恢复 panic）。
type Ticker struct {
	duration     time.Duration
	lastTickTime time.Time
	tick         atomic.Uint64
	stopChan     chan struct{}
	done         chan struct{}
	lifecycleMu  sync.Mutex
	started      bool
	stopped      bool
}

// NewTicker 构造帧间隔为 duration 的 Ticker；duration <= 0 取 100ms。
func NewTicker(duration time.Duration) *Ticker {
	if duration <= 0 {
		duration = 100 * time.Millisecond
	}
	return &Ticker{
		duration:     duration,
		lastTickTime: time.Now(),
		stopChan:     make(chan struct{}),
		done:         make(chan struct{}),
	}
}

// CurrentTick 返回当前帧号；nil 接收者为 0。
func (t *Ticker) CurrentTick() uint64 {
	if t == nil {
		return 0
	}
	return t.tick.Load()
}

// Duration 返回帧间隔；nil 或未配置时为 100ms。
func (t *Ticker) Duration() time.Duration {
	if t == nil || t.duration <= 0 {
		return 100 * time.Millisecond
	}
	return t.duration
}

// Start/Stop 的状态判定与通道关闭属于同一临界区，不能用两个独立 atomic 判定启动所有权。
func (t *Ticker) Start() {
	t.lifecycleMu.Lock()
	defer t.lifecycleMu.Unlock()
	if t.stopped || t.started {
		return
	}
	t.started = true
	go t.run()
}

// Stop 停止 Ticker 并等待帧循环退出；可重复、可并发调用，未启动时直接返回。
func (t *Ticker) Stop() {
	t.lifecycleMu.Lock()
	started := t.started
	if !t.stopped {
		t.stopped = true
		if started {
			close(t.stopChan)
		}
	}
	t.lifecycleMu.Unlock()
	// 等待不能持生命周期锁；所有 Stop 调用者等待同一个 run 退出。
	if started {
		<-t.done
	}
}

func (t *Ticker) run() {
	defer close(t.done)
	ticker := time.NewTicker(t.duration)
	defer ticker.Stop()

	for {
		select {
		case <-t.stopChan:
			return
		case <-ticker.C:
			t.doTick()
		}
	}
}

func (t *Ticker) doTick() {
	curFrame := t.tick.Add(1)
	now := time.Now()
	var elapsed int64
	if curFrame > 1 {
		elapsed = now.Sub(t.lastTickTime).Nanoseconds()
	}
	t.lastTickTime = now
	msg := TickMsg{Elapsed: elapsed, FrameNumber: curFrame}
	// 每帧读取当前注册表（按注册顺序、锁内只取切片）：引擎启动后注册的回调从下一帧生效，
	// 不会因构造时的快照被静默丢弃。
	for _, f := range snapshotTickCallbacks() {
		goroutine.SafeFunc(func() {
			f(msg)
		})
	}
}
