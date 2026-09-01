package nats

import "testing"

func TestDefaultRetryPolicyDoesNotRetryNonIdempotentRPC(t *testing.T) {
	if got := DefaultRetryPolicy().MaxAttempts; got != 1 {
		t.Fatalf("default MaxAttempts = %d, want 1", got)
	}
}
