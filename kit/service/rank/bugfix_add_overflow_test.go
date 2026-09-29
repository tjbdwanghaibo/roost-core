package rank

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"
)

func TestBugfix5OverflowDoesNotChangeScoreOrConsumeRequest(t *testing.T) {
	for _, test := range []struct {
		name                 string
		initial, delta, safe int64
	}{{"positive", math.MaxInt64, 1, -1}, {"negative", math.MinInt64, -1, 1}} {
		t.Run(test.name, func(t *testing.T) {
			s := reviewRankStore(t)
			ctx := context.Background()
			if _, err := s.Submit(ctx, arena(), Score{OwnerID: 1, Value: test.initial, Tie: 7, Brief: []byte("original")}, UpdateSet, ""); err != nil {
				t.Fatal(err)
			}
			before, _, err := s.Rank(ctx, arena(), 1)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Submit(ctx, arena(), Score{OwnerID: 1, Value: test.delta, Tie: 8, Brief: []byte("rejected")}, UpdateAdd, "same-request"); !errors.Is(err, ErrScoreInvalid) {
				t.Fatal(err)
			}
			after, _, err := s.Rank(ctx, arena(), 1)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal(before, after, err)
			}
			got, err := s.Submit(ctx, arena(), Score{OwnerID: 1, Value: test.safe}, UpdateAdd, "same-request")
			if err != nil || got.Score.Value != test.initial+test.safe {
				t.Fatal(got, err)
			}
		})
	}
}

func TestBugfix5ConcurrentAddsCannotWrapAfterCASRetry(t *testing.T) {
	s := reviewRankStore(t)
	ctx := context.Background()
	if _, err := s.Submit(ctx, arena(), Score{OwnerID: 1, Value: math.MaxInt64 - 1}, UpdateSet, ""); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []string{"a", "b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.Submit(ctx, arena(), Score{OwnerID: 1, Value: 1}, UpdateAdd, id)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	success, rejected := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrScoreInvalid) {
			rejected++
		} else {
			t.Fatal(err)
		}
	}
	got, found, err := s.Rank(ctx, arena(), 1)
	if err != nil || !found || got.Score.Value != math.MaxInt64 || success != 1 || rejected != 1 {
		t.Fatal(got, found, err, success, rejected)
	}
}
