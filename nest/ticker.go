package nest

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/goroutine"
)

var tick uint64

func CurTick() uint64 {
	return atomic.LoadUint64(&tick)
}

func IncTick() uint64 {
	return atomic.AddUint64(&tick, 1)
}

func SetTick(curTick uint64) {
	atomic.StoreUint64(&tick, curTick)
}

var (
	tickMu sync.RWMutex
	// tickCbSeen deduplicates names; tickCbList preserves registration order
	// so callbacks execute deterministically (a map-ordered walk would make
	// inter-callback ordering vary per process).
	tickCbSeen = make(map[TickCallbackName]struct{})
	tickCbList []func(msg TickMsg)
)

type TickCallbackName struct {
	value string
}

func NewTickCallbackName(value string) TickCallbackName {
	return TickCallbackName{value: value}
}

func (n TickCallbackName) String() string {
	return n.value
}

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

func MustRegisterTickCallback(name TickCallbackName, cb func(msg TickMsg)) {
	if err := RegisterTickCallback(name, cb); err != nil {
		panic(err)
	}
}

func RangeAllTickCallback(f func(ff func(msg TickMsg))) {
	for _, cb := range snapshotTickCallbacks() {
		f(cb)
	}
}

// snapshotTickCallbacks 返回只读快照，调用者不得改写切片。
// 注册时发布新数组，故 callback 内注册新 callback 不会影响本轮或造成锁重入。
func snapshotTickCallbacks() []func(msg TickMsg) {
	tickMu.RLock()
	defer tickMu.RUnlock()
	return tickCbList
}

// Ticker is the frame-based timing system (channel-based, no actor).
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

func (t *Ticker) CurrentTick() uint64 {
	if t == nil {
		return 0
	}
	return t.tick.Load()
}

func (t *Ticker) SetCurrentTick(value uint64) {
	if t != nil {
		t.tick.Store(value)
	}
}

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
	// Read the live registry every tick (registration order, copied outside
	// the lock): callbacks registered after the engine started take effect on
	// the next tick instead of being silently dropped by a construction-time snapshot.
	for _, f := range snapshotTickCallbacks() {
		goroutine.SafeFunc(func() {
			f(msg)
		})
	}
}
