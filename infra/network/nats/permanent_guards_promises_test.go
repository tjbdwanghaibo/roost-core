package nats

import (
	"errors"
	"testing"
)

// U-0149 · C2 · gap map core `nats` 1/1：Permanent 对 nil 返回 nil，对已是永久错误的错误原样返回（不再包一层）。
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
