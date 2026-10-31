//go:build !darwin && !linux

package loadtest

func processUsage() (float64, uint64, bool) { return 0, 0, false }
