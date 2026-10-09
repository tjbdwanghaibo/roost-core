//go:build unix

package lock

import (
	"syscall"
	"testing"
	"time"
)

// processCPUTime returns user+system CPU time consumed by the whole process.
func processCPUTime(tb testing.TB) time.Duration {
	tb.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		tb.Skipf("getrusage unavailable: %v", err)
	}
	user := time.Duration(usage.Utime.Sec)*time.Second + time.Duration(usage.Utime.Usec)*time.Microsecond
	system := time.Duration(usage.Stime.Sec)*time.Second + time.Duration(usage.Stime.Usec)*time.Microsecond
	return user + system
}
