package nats

import (
	"errors"
	"testing"
)

// Permanent 对 nil 返回 nil，保留原因并带永久标记，对已是永久错误的错误原样返回（不再包一层）。
func TestPermanentIsIdempotentAndNilSafe(t *testing.T) {
	if err := Permanent(nil); err != nil {
		t.Fatalf("Permanent(nil) = %v", err)
	}
	base := errors.New("boom")
	once := Permanent(base)
	if !IsPermanent(once) || !errors.Is(once, base) {
		t.Fatalf("Permanent(base) = %v", once)
	}
	if twice := Permanent(once); twice != once {
		t.Fatalf("Permanent(permanent) rewrapped: %v", twice)
	}
}

func TestDefaultRetryPolicyDoesNotRetryNonIdempotentRPC(t *testing.T) {
	if got := DefaultRetryPolicy().MaxAttempts; got != 1 {
		t.Fatalf("default MaxAttempts = %d, want 1", got)
	}
}
