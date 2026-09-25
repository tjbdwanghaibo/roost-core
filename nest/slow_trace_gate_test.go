package nest

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSlowTraceGateBoundsConcurrentDiagnostics(t *testing.T) {
	var gate slowTraceGate
	now := time.Now()
	var accepted atomic.Int64
	var wg sync.WaitGroup
	for range 128 {
		wg.Go(func() {
			if ok, _ := gate.take(now); ok {
				accepted.Add(1)
			}
		})
	}
	wg.Wait()
	if got := accepted.Load(); got != 1 {
		t.Fatalf("stack captures for one burst=%d", got)
	}
	if ok, _ := gate.take(now.Add(nestSlowDispatchStackInterval - time.Nanosecond)); ok {
		t.Fatal("window reopened early")
	}
	if ok, skipped := gate.take(now.Add(nestSlowDispatchStackInterval)); !ok || skipped != 128 {
		t.Fatalf("next window allowed=%v suppressed=%d", ok, skipped)
	}
	if ok, skipped := gate.take(now.Add(2 * nestSlowDispatchStackInterval)); !ok || skipped != 0 {
		t.Fatalf("idle next window allowed=%v suppressed=%d", ok, skipped)
	}
}
