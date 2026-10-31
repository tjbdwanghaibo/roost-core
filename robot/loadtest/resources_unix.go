//go:build darwin || linux

package loadtest

import (
	"golang.org/x/sys/unix"
	"runtime"
)

func processUsage() (float64, uint64, bool) {
	var usage unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &usage); err != nil {
		return 0, 0, false
	}
	cpu := float64(usage.Utime.Sec+usage.Stime.Sec) + float64(usage.Utime.Usec+usage.Stime.Usec)/1e6
	rss := uint64(max(0, usage.Maxrss))
	if runtime.GOOS == "linux" {
		rss *= 1024
	}
	return cpu, rss, true
}
