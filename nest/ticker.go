package nest

import (
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/goroutine"
	"sync"
	"sync/atomic"
	"time"
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
	tickCbList = append(tickCbList, cb)
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

// snapshotTickCallbacks copies the registration-ordered callback list so
// callers run callbacks outside the registry lock (a callback registering
// another callback must not deadlock).
func snapshotTickCallbacks() []func(msg TickMsg) {
	tickMu.RLock()
	defer tickMu.RUnlock()
	callbacks := make([]func(msg TickMsg), len(tickCbList))
	copy(callbacks, tickCbList)
	return callbacks
}

// Ticker is the frame-based timing system (channel-based, no actor).
type Ticker struct {
	duration     time.Duration
	lastTickTime time.Time
	tick         atomic.Uint64
	stopChan     chan struct{}
	done         chan struct{}
	started      atomic.Bool
	stopped      atomic.Bool
	stopOnce     sync.Once
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

func (t *Ticker) Start() {
	if t.stopped.Load() {
		return
	}
	if t.started.CompareAndSwap(false, true) {
		go t.run()
	}
}

func (t *Ticker) Stop() {
	if !t.stopped.CompareAndSwap(false, true) {
		if t.started.Load() {
			<-t.done
		}
		return
	}
	if !t.started.Load() {
		return
	}
	t.stopOnce.Do(func() {
		close(t.stopChan)
	})
	<-t.done
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
