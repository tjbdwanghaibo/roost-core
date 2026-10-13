// Ticker：回调隔离、注册顺序、Start / Stop 的并发与幂等、实例状态隔离。

package nest

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 每个 tick 回调各自 SafeFunc：一个回调 panic 不停 ticker，也不跳过同一 tick 里排在它后面的回调。
func TestTickerPanickingCallbackStopsNeitherTheTickerNorItsPeers(t *testing.T) {
	resetTickCallbacksForTest()
	t.Cleanup(resetTickCallbacksForTest)

	var panics, peerRuns, zeroFrames atomic.Int64
	MustRegisterTickCallback(NewTickCallbackName("test_tick_panic"), func(msg TickMsg) {
		panics.Add(1)
		panic("boom")
	})
	// Registered after the panicking one: reached only if the panic is
	// contained per callback rather than per tick.
	MustRegisterTickCallback(NewTickCallbackName("test_tick_peer"), func(msg TickMsg) {
		peerRuns.Add(1)
		if msg.FrameNumber == 0 {
			zeroFrames.Add(1)
		}
	})

	tk := NewTicker(time.Millisecond)
	tk.Start()

	// Bounded wait for three ticks: a t.Fatal here beats hanging until the
	// go test timeout, which reports nothing useful.
	deadline := time.Now().Add(2 * time.Second)
	for tk.CurrentTick() < 3 {
		if time.Now().After(deadline) {
			tk.Stop()
			t.Fatalf("ticker stalled at tick %d after a callback panicked (panics=%d)",
				tk.CurrentTick(), panics.Load())
		}
		time.Sleep(time.Millisecond)
	}
	tk.Stop()

	ticks := tk.CurrentTick()
	if got := panics.Load(); got < 3 {
		t.Fatalf("panicking callback ran %d times across %d ticks; it must be invoked every tick", got, ticks)
	}
	if got := peerRuns.Load(); got < 3 {
		t.Fatalf("peer callback ran %d times across %d ticks; a panic must not skip its peers", got, ticks)
	}
	if got := zeroFrames.Load(); got != 0 {
		t.Fatalf("callbacks received frame number 0 in %d tick(s); frames start at 1", got)
	}

	// Stop is idempotent and must not block on an already-stopped ticker.
	stopped := make(chan struct{})
	go func() { tk.Stop(); tk.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("repeated Stop blocked; Stop must be idempotent")
	}
}

// A ticker that was never started must still be safe to stop, and must stay
// stopped rather than starting later.
func TestTickerStopBeforeStartIsSafeAndFinal(t *testing.T) {
	tk := NewTicker(time.Millisecond)
	done := make(chan struct{})
	go func() { tk.Stop(); tk.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop blocked on a ticker that was never started")
	}

	tk.Start()
	time.Sleep(10 * time.Millisecond)
	if got := tk.CurrentTick(); got != 0 {
		t.Fatalf("a stopped ticker started anyway and reached tick %d", got)
	}
}

func TestTickCallbacksRegisteredAfterStartTakeEffectInOrder(t *testing.T) {
	resetTickCallbacksForTest()
	t.Cleanup(resetTickCallbacksForTest)
	// Regression: NewTicker used to snapshot the global registry at engine
	// construction — callbacks registered afterwards silently never ran, and
	// snapshot order came from map iteration (nondeterministic).
	var order []int
	var orderMu sync.Mutex
	MustRegisterTickCallback(NewTickCallbackName("tick_order_a"), func(TickMsg) {
		orderMu.Lock()
		order = append(order, 1)
		orderMu.Unlock()
	})
	ticker := NewTicker(time.Hour) // never fires on its own; we drive doTick manually
	ticker.doTick()
	MustRegisterTickCallback(NewTickCallbackName("tick_order_b"), func(TickMsg) {
		orderMu.Lock()
		order = append(order, 2)
		orderMu.Unlock()
	})
	ticker.doTick()
	orderMu.Lock()
	defer orderMu.Unlock()
	if len(order) != 3 || order[0] != 1 || order[1] != 1 || order[2] != 2 {
		t.Fatalf("callback order = %v, want [1 1 2] (late registration effective, registration order kept)", order)
	}
}

func resetTickCallbacksForTest() {
	tickMu.Lock()
	tickCbSeen = make(map[TickCallbackName]struct{})
	tickCbList = nil
	tickMu.Unlock()
}

func TestTickerConcurrentStartStopAlwaysClosesStartedRun(t *testing.T) {
	for i := range 20000 {
		ticker := NewTicker(time.Hour)
		gate := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() { <-gate; ticker.Start() })
		wg.Go(func() { <-gate; ticker.Stop() })
		close(gate)
		wg.Wait()
		if !ticker.started {
			continue
		}
		select {
		case <-ticker.done:
		default:
			// Stop 认为尚未启动而随后 Start 仍启动时，清理泄漏的运行以免污染其它测试。
			select {
			case <-ticker.stopChan:
			default:
				close(ticker.stopChan)
			}
			<-ticker.done
			t.Fatalf("iteration %d: stopped ticker retained a live run", i)
		}
	}
}

func TestTickerStateIsInstanceScoped(t *testing.T) {
	first := NewTicker(time.Hour)
	second := NewTicker(time.Hour)
	first.doTick()
	first.doTick()
	second.doTick()
	if first.CurrentTick() != 2 || second.CurrentTick() != 1 {
		t.Fatalf("ticker state leaked: first=%d second=%d", first.CurrentTick(), second.CurrentTick())
	}
}
