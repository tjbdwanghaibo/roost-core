package activity

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

type reviewLostMark struct {
	versionstore.Store[RequestKey, ProgressReservation]
	fail bool
}

func (s *reviewLostMark) Update(ctx context.Context, k RequestKey, m versionstore.Mutate[ProgressReservation]) (versionstore.Versioned[ProgressReservation], bool, error) {
	if k.RequestID == "lost" && s.fail {
		s.fail = false
		return versionstore.Versioned[ProgressReservation]{}, false, fmt.Errorf("injected mark failure")
	}
	return s.Store.Update(ctx, k, m)
}

func TestReviewLastDispatchRetryKeepsAckWindow(t *testing.T) {
	s, _ := newActivityService(t, func(c *Config) { c.DispatchMaxAttempts = 1 })
	ctx := context.Background()
	key := activityKey("last-attempt")
	openActivity(t, s, key, 1)
	notify(t, s, key, 1)
	first, err := s.AttemptDispatch(ctx, key, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, retryErr := s.AttemptDispatch(ctx, key, 1)
	_, ackErr := s.AckDispatch(ctx, key, 1, first.Token)
	if !errors.Is(retryErr, ErrDispatchNotDue) || ackErr != nil {
		t.Fatalf("unelapsed final ACK window was closed: retry=%v ack=%v", retryErr, ackErr)
	}
}
func TestReviewUnconfirmedProgressSurvivesRingEviction(t *testing.T) {
	s, _ := newActivityService(t, func(c *Config) { c.Ledger = &reviewLostMark{c.Ledger, true} })
	ctx := context.Background()
	key := activityKey("ring")
	openActivity(t, s, key, 1)
	if _, err := s.ApplyProgress(ctx, key, "p", "lost", ProgressDelta{Score: 1}); err == nil {
		t.Fatal("expected mark failure")
	}
	for i := 0; i < MaxProgressWindow; i++ {
		if _, err := s.ApplyProgress(ctx, key, "p", fmt.Sprintf("later-%d", i), ProgressDelta{Score: 1}); err != nil {
			t.Fatal(err)
		}
	}
	before, _, _ := s.LookupParticipant(ctx, key, "p")
	if _, err := s.ApplyProgress(ctx, key, "p", "lost", ProgressDelta{Score: 1}); err != nil {
		t.Fatal(err)
	}
	after, _, _ := s.LookupParticipant(ctx, key, "p")
	if after.Score != before.Score {
		t.Fatalf("unconfirmed request reapplied inside TTL: %d -> %d", before.Score, after.Score)
	}
}
