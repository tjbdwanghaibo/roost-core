package match

import (
	"errors"
	"fmt"
	"testing"
)

func TestBugfix5GroupingValidatesBeforeReadingCandidates(t *testing.T) {
	policies := map[string]Grouping{"fifo": FirstComeGrouping{}, "score": ScoreWindowGrouping{NowUnix: func() int64 { return 1700000000 }}}
	for name, policy := range policies {
		t.Run(name, func(t *testing.T) {
			for _, size := range []int{-1, 0, 1, MaxGroupSize + 1} {
				for _, count := range []int{0, 2} {
					t.Run(fmt.Sprintf("size_%d_candidates_%d", size, count), func(t *testing.T) {
						q := ranked()
						q.GroupSize = size
						group, formed, err := policy.Group(q, make([]Ticket, count))
						if !errors.Is(err, ErrQueueInvalid) || formed || len(group) != 0 {
							t.Fatal(group, formed, err)
						}
					})
				}
			}
			q := ranked()
			q.Mode = ""
			if _, _, err := policy.Group(q, nil); !errors.Is(err, ErrQueueInvalid) {
				t.Fatal(err)
			}
			for _, size := range []int{2, MaxGroupSize} {
				q := ranked()
				q.GroupSize = size
				candidates := make([]Ticket, size)
				for i := range candidates {
					candidates[i] = Ticket{ID: fmt.Sprint(i), Subject: player(int64(i+1), 100)}
				}
				group, ok, err := policy.Group(q, candidates)
				if err != nil || !ok || len(group) != size {
					t.Fatal(group, ok, err)
				}
				if _, ok, err := policy.Group(q, candidates[:size-1]); err != nil || ok {
					t.Fatal(ok, err)
				}
			}
		})
	}
}
