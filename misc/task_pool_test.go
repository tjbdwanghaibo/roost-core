package misc

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestTaskPoolNormalizesZeroWorkerCount(t *testing.T) {
	pool := NewTaskPool(&TaskPoolConfig{}) // used to build zero workers and panic on Submit
	pool.Start()
	defer pool.Shutdown()
	var ran atomic.Int32
	done := make(chan struct{})
	if err := pool.SubmitFunc(-42, "probe", func() error { // negative id: index must stay valid
		ran.Add(1)
		close(done)
		return nil
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("task never ran")
	}
	if ran.Load() != 1 {
		t.Fatalf("ran = %d", ran.Load())
	}
}
