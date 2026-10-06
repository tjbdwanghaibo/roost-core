package operation

import (
	"context"
	"errors"
	"testing"
	"time"
)

// RR-20261006-10：Serial 让并发的停止调用串行，后到者的等待受它自己的 ctx 约束。
func TestSerialLaterCallerWaitsWithinItsOwnContext(t *testing.T) {
	var serial Serial
	if err := serial.Lock(context.Background()); err != nil {
		t.Fatalf("first Lock = %v", err)
	}
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := serial.Lock(expired); !errors.Is(err, context.Canceled) {
		t.Fatalf("Lock while held with an expired ctx = %v, want context.Canceled", err)
	}

	acquired := make(chan error, 1)
	go func() { acquired <- serial.Lock(context.Background()) }()
	select {
	case err := <-acquired:
		t.Fatalf("Lock returned %v while the first holder had not unlocked", err)
	case <-time.After(50 * time.Millisecond):
	}
	serial.Unlock()
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("Lock after Unlock = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Lock still blocked after Unlock")
	}
	serial.Unlock()

	// 空闲时即使 ctx 已结束也取得（不在两个就绪分支间随机）。
	for range 64 {
		if err := serial.Lock(expired); err != nil {
			t.Fatalf("Lock on an idle Serial with an expired ctx = %v, want nil", err)
		}
		serial.Unlock()
	}
}
