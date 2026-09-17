package match

import (
	"fmt"
	"math"
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
	// The window is a distance, and a distance is not negative. Refusing here
	// is what lets the arithmetic below stay in uint64 (RR-20260917-02).
	if g.InitialWindow < 0 || g.WidenPerSecond < 0 || g.MaxWindow < 0 {
		return nil, false, fmt.Errorf("%w: score window, widening and cap must not be negative (got %d, %d, %d)",
			ErrQueueInvalid, g.InitialWindow, g.WidenPerSecond, g.MaxWindow)
	}
	if len(candidates) < queue.GroupSize {
		return nil, false, nil
	}
	// Anchor on the oldest candidate: that is whose wait the window is for,
	// and anchoring anywhere else lets a newcomer jump a starving player.
	anchor := candidates[0]
	waited := g.NowUnix() - anchor.CreatedAtUnix
	if waited < 0 {
		// A clock that went backwards has not made anyone wait longer.
		waited = 0
	}
	window := g.window(uint64(waited))

	within := make([]Ticket, 0, len(candidates))
	for _, candidate := range candidates {
		if scoreDistance(candidate.Subject.Score, anchor.Subject.Score) <= window {
			within = append(within, candidate)
		}
	}
	if len(within) < queue.GroupSize {
		return nil, false, nil
	}
	// Closest scores to the anchor first, ties by arrival order so the result
	// is deterministic. The same distance function as the filter, so what is
	// admitted and how it is ranked cannot disagree.
	sort.SliceStable(within, func(i, j int) bool {
		di := scoreDistance(within[i].Subject.Score, anchor.Subject.Score)
		dj := scoreDistance(within[j].Subject.Score, anchor.Subject.Score)
		if di != dj {
			return di < dj
		}
		return within[i].CreatedAtUnix < within[j].CreatedAtUnix
	})
	return within[:queue.GroupSize], true, nil
}

// window is InitialWindow + WidenPerSecond*waited, saturating, then capped.
//
// Score is an int64 whose meaning belongs to the caller, so the API cannot
// assume "reasonable" magnitudes: with signed arithmetic a large
// InitialWindow plus a little widening wrapped negative and the cap compared
// against garbage, and a large WidenPerSecond wrapped the product
// (RR-20260917-02). Saturation is the right answer for a window: past
// math.MaxUint64 everything is inside anyway, and the cap applies after.
func (g ScoreWindowGrouping) window(waited uint64) uint64 {
	window := uint64(g.InitialWindow)
	widen := uint64(g.WidenPerSecond)
	if widen > 0 && waited > 0 {
		grow := widen * waited
		if grow/widen != waited {
			grow = math.MaxUint64
		}
		if window > math.MaxUint64-grow {
			window = math.MaxUint64
		} else {
			window += grow
		}
	}
	if g.MaxWindow > 0 && window > uint64(g.MaxWindow) {
		window = uint64(g.MaxWindow)
	}
	return window
}

// scoreDistance is |a-b| for any two int64 scores, without overflow: the
// difference of two int64 values always fits in a uint64, and two's
// complement subtraction of the unsigned images yields exactly it once the
// larger operand is on the left. absInt64(a-b) did not: a-b wrapped for
// operands of opposite sign far apart, and abs(math.MinInt64) is negative.
func scoreDistance(a, b int64) uint64 {
	if a >= b {
		return uint64(a) - uint64(b)
	}
	return uint64(b) - uint64(a)
}

var (
	_ Grouping = FirstComeGrouping{}
	_ Grouping = ScoreWindowGrouping{}
)
