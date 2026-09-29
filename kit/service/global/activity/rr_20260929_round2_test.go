package activity

import (
	"context"
	"errors"
	"math"
	"testing"
)

func TestReviewPositiveProgressCannotWrapNegative(t *testing.T) {
	s, _ := newActivityService(t)
	ctx := context.Background()
	key := activityKey("overflow")
	openActivity(t, s, key, 1)
	if _, err := s.ApplyProgress(ctx, key, "p", "first", ProgressDelta{Score: math.MaxInt64, Progress: math.MaxInt64}); err != nil {
		t.Fatal(err)
	}
	_, applyErr := s.ApplyProgress(ctx, key, "p", "second", ProgressDelta{Score: 1, Progress: 1})
	p, found, err := s.LookupParticipant(ctx, key, "p")
	if err != nil || !found {
		t.Fatal(err)
	}
	if !errors.Is(applyErr, ErrRequestInvalid) || p.Score != math.MaxInt64 || p.Progress != math.MaxInt64 || p.Applies != 1 {
		t.Fatalf("positive delta persisted negative standing: score=%d progress=%d", p.Score, p.Progress)
	}
}
