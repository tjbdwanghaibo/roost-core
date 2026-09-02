package goroutine

import (
	"errors"
	"strings"
	"testing"
)

func TestSafeFuncWithTryCountWrapsLastError(t *testing.T) {
	cause := errors.New("db down")
	calls := 0
	err := SafeFuncWithTryCount(3, func() error {
		calls++
		return cause
	})
	if calls != 3 {
		t.Fatalf("calls = %d", calls)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("err = %v, want wrapped cause", err)
	}
}

func TestSafeFuncWithTryCountRecoversPanic(t *testing.T) {
	err := SafeFuncWithTryCount(2, func() error { panic("boom") })
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

func TestSafeFuncWithTryCountZeroCountStillRunsOnce(t *testing.T) {
	calls := 0
	if err := SafeFuncWithTryCount(0, func() error { calls++; return nil }); err != nil {
		t.Fatalf("err = %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (zero count used to skip the function entirely)", calls)
	}
}
