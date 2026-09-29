package mail

import (
	"context"
	"fmt"
	"testing"
)

type reviewEnvelopeOnceFailure struct {
	EnvelopeStore
	failed bool
}

func (s *reviewEnvelopeOnceFailure) Create(ctx context.Context, e Envelope) (bool, error) {
	if !s.failed {
		s.failed = true
		return false, fmt.Errorf("injected transient failure before envelope write")
	}
	return s.EnvelopeStore.Create(ctx, e)
}
func TestReviewTransientEnvelopeFailureCanRetrySameRequest(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Envelopes = &reviewEnvelopeOnceFailure{EnvelopeStore: c.Envelopes} })
	ctx := context.Background()
	req := directTo(1)
	first, firstErr := h.service.Send(ctx, req)
	second, err := h.service.Send(ctx, req)
	if err != nil {
		t.Fatalf("store recovered but same send cannot recover: %v", err)
	}
	if firstErr == nil && first.ID != second.ID {
		t.Fatalf("successful send replay changed mail id: %s -> %s", first.ID, second.ID)
	}
}
