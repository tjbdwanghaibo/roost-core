package match

import (
	"fmt"
	"sort"
)

// Grouping decides which waiting tickets form a match. It is the caller's
// tool: a matchmaker reads Candidates, asks a Grouping for a group, and
// Commits it. The store does not run it — Enqueue only queues, Commit only
// commits the tickets it is given — so a policy lives where it can be retried,
// observed and cancelled outside the storage CAS (RR-20260916-05).
type Grouping interface {
	// Group selects exactly queue.GroupSize tickets from candidates, or
	// reports that no group can be formed yet. candidates are waiting,
	// unexpired tickets in queue order, oldest first.
	//
	// It must not return a ticket that is not in candidates, and must not
	// return the same ticket twice; the store validates both, but a policy
	// that relies on that is wrong.
	Group(queue Queue, candidates []Ticket) ([]Ticket, bool, error)
}

// FirstComeGrouping takes the oldest candidates. It is the default because it
// is the only policy that needs no game knowledge, and because a queue that
// matches strictly in arrival order has a bounded worst-case wait.
type FirstComeGrouping struct{}

func (FirstComeGrouping) Group(queue Queue, candidates []Ticket) ([]Ticket, bool, error) {
	if len(candidates) < queue.GroupSize {
		return nil, false, nil
	}
	return candidates[:queue.GroupSize], true, nil
}

// ScoreWindowGrouping matches the oldest candidate with the closest scores,
// but only while they fall inside a widening window.
//
// The window widens with the oldest candidate's wait so a lone outlier
// eventually matches instead of starving — the property a fixed window lacks
// and the reason a rating system cannot be expressed as "sort by score".
type ScoreWindowGrouping struct {
	// InitialWindow is the score distance tolerated immediately.
	InitialWindow int64
	// WidenPerSecond grows the window with the oldest candidate's wait.
	WidenPerSecond int64
	// MaxWindow caps it; zero means uncapped.
	MaxWindow int64
	// NowUnix supplies the current time; required, because the window depends
	// on how long the oldest candidate has waited.
	NowUnix func() int64
}

func (g ScoreWindowGrouping) Group(queue Queue, candidates []Ticket) ([]Ticket, bool, error) {
	if g.NowUnix == nil {
		return nil, false, fmt.Errorf("%w: score window grouping needs a clock", ErrQueueInvalid)
	}
	if len(candidates) < queue.GroupSize {
		return nil, false, nil
	}
	// Anchor on the oldest candidate: that is whose wait the window is for,
	// and anchoring anywhere else lets a newcomer jump a starving player.
	anchor := candidates[0]
	waited := g.NowUnix() - anchor.CreatedAtUnix
	if waited < 0 {
		waited = 0
	}
	window := g.InitialWindow + g.WidenPerSecond*waited
	if g.MaxWindow > 0 && window > g.MaxWindow {
		window = g.MaxWindow
	}

	within := make([]Ticket, 0, len(candidates))
	for _, candidate := range candidates {
		if absInt64(candidate.Subject.Score-anchor.Subject.Score) <= window {
			within = append(within, candidate)
		}
	}
	if len(within) < queue.GroupSize {
		return nil, false, nil
	}
	// Closest scores to the anchor first, ties by arrival order so the result
	// is deterministic.
	sort.SliceStable(within, func(i, j int) bool {
		di := absInt64(within[i].Subject.Score - anchor.Subject.Score)
		dj := absInt64(within[j].Subject.Score - anchor.Subject.Score)
		if di != dj {
			return di < dj
		}
		return within[i].CreatedAtUnix < within[j].CreatedAtUnix
	})
	return within[:queue.GroupSize], true, nil
}

func absInt64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

var (
	_ Grouping = FirstComeGrouping{}
	_ Grouping = ScoreWindowGrouping{}
)
