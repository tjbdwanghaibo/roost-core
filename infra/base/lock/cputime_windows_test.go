//go:build !unix

package lock

import (
	"testing"
	"time"
)

// processCPUTime is only implemented for unix; the CPU-burn benchmark is
// skipped elsewhere.
func processCPUTime(tb testing.TB) time.Duration {
	tb.Helper()
	tb.Skip("process CPU time measurement is unix-only")
	return 0
}
