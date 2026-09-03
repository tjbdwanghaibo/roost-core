package lock

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/goroutine"
)

// spinReentrantMutex is the previous ReentrantMutex implementation, kept
// verbatim (Gosched busy-wait) so benchmarks can compare the old spin
// behavior against the current parking implementation and sync.Mutex.
type spinReentrantMutex struct {
	mu        sync.Mutex
	owner     int64
	recursion int32
}

func (rm *spinReentrantMutex) Lock() {
	gid := goroutine.GoID()

	rm.mu.Lock()
	if rm.owner == gid {
		rm.recursion++
		rm.mu.Unlock()
		return
	}

	for rm.owner != 0 {
		rm.mu.Unlock()
		runtime.Gosched()
		rm.mu.Lock()
	}

	rm.owner = gid
	rm.recursion = 1
	rm.mu.Unlock()
}

func (rm *spinReentrantMutex) Unlock() {
	gid := goroutine.GoID()

	rm.mu.Lock()
	if rm.owner != gid {
		rm.mu.Unlock()
		panic("unlock of unowned mutex")
	}

	recursion := rm.recursion - 1
	rm.recursion = recursion
	if recursion == 0 {
		rm.owner = 0
	}
	rm.mu.Unlock()
}

type benchLocker interface {
	Lock()
	Unlock()
}

func benchmarkUncontended(b *testing.B, mu benchLocker) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		mu.Lock()
		mu.Unlock() //nolint:staticcheck // benchmark measures the empty critical section
	}
}

func BenchmarkLockUncontended(b *testing.B) {
	b.Run("parking", func(b *testing.B) { benchmarkUncontended(b, NewReentrantMutex(1)) })
	b.Run("spin", func(b *testing.B) { benchmarkUncontended(b, &spinReentrantMutex{}) })
	b.Run("sync.Mutex", func(b *testing.B) { benchmarkUncontended(b, &sync.Mutex{}) })
}

func BenchmarkLockReentrantFastPath(b *testing.B) {
	b.Run("parking", func(b *testing.B) {
		mu := NewReentrantMutex(1)
		mu.Lock()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			mu.Lock()
			mu.Unlock()
		}
		b.StopTimer()
		mu.Unlock()
	})
	b.Run("spin", func(b *testing.B) {
		mu := &spinReentrantMutex{}
		mu.Lock()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			mu.Lock()
			mu.Unlock()
		}
		b.StopTimer()
		mu.Unlock()
	})
}

func benchmarkContended(b *testing.B, mu benchLocker, hold time.Duration) {
	var counter atomic.Int64
	b.SetParallelism(4) // 4×GOMAXPROCS goroutines contending on one lock
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			mu.Lock()
			counter.Add(1)
			if hold > 0 {
				// Simulate work performed while holding the entity lock
				// (e.g. a WAL group-commit fsync at the commit point).
				spinFor(hold)
			}
			mu.Unlock()
		}
	})
}

// spinFor burns CPU for roughly d to model a critical section whose holder is
// busy (fsync completion, serialization) rather than sleeping; time.Sleep
// would let the scheduler idle and hide the cost the waiters pay.
func spinFor(d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
	}
}

func BenchmarkLockContendedShortCritical(b *testing.B) {
	b.Run("parking", func(b *testing.B) { benchmarkContended(b, NewReentrantMutex(1), 0) })
	b.Run("spin", func(b *testing.B) { benchmarkContended(b, &spinReentrantMutex{}, 0) })
	b.Run("sync.Mutex", func(b *testing.B) { benchmarkContended(b, &sync.Mutex{}, 0) })
}

func BenchmarkLockContendedSlowCritical(b *testing.B) {
	const hold = 100 * time.Microsecond
	b.Run("parking", func(b *testing.B) { benchmarkContended(b, NewReentrantMutex(1), hold) })
	b.Run("spin", func(b *testing.B) { benchmarkContended(b, &spinReentrantMutex{}, hold) })
	b.Run("sync.Mutex", func(b *testing.B) { benchmarkContended(b, &sync.Mutex{}, hold) })
}

// BenchmarkLockWaiterCPUWhileHolderBlocked measures the CPU cost paid by
// waiters while the lock holder is blocked (not runnable) for a long stretch,
// e.g. waiting on a durable WAL admission. This is the scenario where the old
// spin implementation burned CPU on every waiter; parked waiters cost nothing.
// Reported metric: waiter CPU milliseconds consumed per second of holder
// block time (cpu-ms/s, lower is better).
func BenchmarkLockWaiterCPUWhileHolderBlocked(b *testing.B) {
	run := func(b *testing.B, mu benchLocker) {
		const waiters = 4
		const blockFor = 50 * time.Millisecond
		var totalCPU time.Duration
		var totalBlocked time.Duration
		for i := 0; i < b.N; i++ {
			release := make(chan struct{})
			held := make(chan struct{})
			go func() {
				mu.Lock()
				close(held)
				<-release // holder blocked while owning the lock
				mu.Unlock()
			}()
			<-held

			var wg sync.WaitGroup
			for w := 0; w < waiters; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					mu.Lock()
					mu.Unlock() //nolint:staticcheck // measuring wait cost only
				}()
			}
			// Let the waiters settle into their waiting state, then measure
			// process CPU consumed while nothing useful can run.
			time.Sleep(5 * time.Millisecond)
			startCPU := processCPUTime(b)
			time.Sleep(blockFor)
			totalCPU += processCPUTime(b) - startCPU
			totalBlocked += blockFor
			close(release)
			wg.Wait()
		}
		b.ReportMetric(float64(totalCPU.Milliseconds())/totalBlocked.Seconds(), "cpu-ms/s")
	}
	b.Run("parking", func(b *testing.B) { run(b, NewReentrantMutex(1)) })
	b.Run("spin", func(b *testing.B) { run(b, &spinReentrantMutex{}) })
	b.Run("sync.Mutex", func(b *testing.B) { run(b, &sync.Mutex{}) })
}
