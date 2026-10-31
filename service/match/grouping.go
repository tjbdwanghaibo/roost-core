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
// observed and cancelled outside the storage CAS.
type Grouping interface {
	// Group selects a group from candidates, or reports that no group can be
	// formed yet. candidates are waiting, unexpired tickets in queue order,
	// oldest first.
	//
	// 组的张数由 queue.FormableSize 决定：固定人数队列恰好 GroupSize 张；弹性人数队列凑得满就取 GroupSize 张，
	// 凑不满时只在最早一张候选票等满 FillAfterSeconds 后取 MinSize..GroupSize-1 张。Commit 按同一条规则校验，
	// 策略自己另算一套张数的组会被存储拒绝（ErrTicketInvalid / ErrGroupNotReady）。
	//
	// It must not return a ticket that is not in candidates, and must not
	// return the same ticket twice; the store validates both, but a policy
	// that relies on that is wrong.
	Group(queue Queue, candidates []Ticket) ([]Ticket, bool, error)
}

// FirstComeGrouping takes the oldest candidates. It is the default because it
// is the only policy that needs no game knowledge, and because a queue that
// matches strictly in arrival order has a bounded worst-case wait.
//
// 弹性人数队列上它取最早的 queue.FormableSize 张：凑得满取 GroupSize 张，凑不满时最早一张等满 FillAfterSeconds 后取全部候选。
type FirstComeGrouping struct {
	// NowUnix 是票所用时钟（存储的 Config.Now，match Mod 注入业务时钟）的当前 Unix 秒。弹性人数队列必须给：
	// 不满员的局要看最早一张票等了多久；固定人数队列不读它，零值 FirstComeGrouping{} 照旧可用。
	NowUnix func() int64
}

func (g FirstComeGrouping) Group(queue Queue, candidates []Ticket) ([]Ticket, bool, error) {
	if err := queue.Validate(); err != nil {
		return nil, false, err
	}
	if !queue.Flexible() {
		if len(candidates) < queue.GroupSize {
			return nil, false, nil
		}
		return candidates[:queue.GroupSize], true, nil
	}
	if g.NowUnix == nil {
		return nil, false, fmt.Errorf("%w: first-come grouping on flexible queue %s needs a clock", ErrQueueInvalid, queue.Key())
	}
	if len(candidates) == 0 {
		return nil, false, nil
	}
	// 候选按队列顺序，第一张是最早的，它一定在取出的组里；存储按组里最早一张判定，所以这里能成的组存储一定接受
	// （两边时钟一致时）。
	size := queue.FormableSize(len(candidates), waitedSeconds(candidates[0].CreatedAtUnix, g.NowUnix()))
	if size == 0 {
		return nil, false, nil
	}
	return candidates[:size], true, nil
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
	// NowUnix supplies the current time on the clock tickets are dated on (the
	// store's Config.Now — the business clock when the match Mod built it); required, because the window depends
	// on how long the oldest candidate has waited.
	NowUnix func() int64
}

func (g ScoreWindowGrouping) Group(queue Queue, candidates []Ticket) ([]Ticket, bool, error) {
	if err := queue.Validate(); err != nil {
		return nil, false, err
	}
	if g.NowUnix == nil {
		return nil, false, fmt.Errorf("%w: score window grouping needs a clock", ErrQueueInvalid)
	}
	// The window is a distance, and a distance is not negative. Refusing here
	// is what lets the arithmetic below stay in uint64.
	if g.InitialWindow < 0 || g.WidenPerSecond < 0 || g.MaxWindow < 0 {
		return nil, false, fmt.Errorf("%w: score window, widening and cap must not be negative (got %d, %d, %d)",
			ErrQueueInvalid, g.InitialWindow, g.WidenPerSecond, g.MaxWindow)
	}
	if len(candidates) < queue.MinGroupSize() {
		return nil, false, nil
	}
	// Anchor on the oldest candidate: that is whose wait the window is for,
	// and anchoring anywhere else lets a newcomer jump a starving player.
	// A clock that went backwards has not made anyone wait longer
	// (waitedSeconds counts it as zero).
	anchor := candidates[0]
	waited := waitedSeconds(anchor.CreatedAtUnix, g.NowUnix())
	window := g.window(uint64(waited))

	within := make([]Ticket, 0, len(candidates))
	for _, candidate := range candidates {
		if scoreDistance(candidate.Subject.Score, anchor.Subject.Score) <= window {
			within = append(within, candidate)
		}
	}
	// 张数按队列规则：窗口内的票够 GroupSize 取满员组；弹性队列凑不满时，锚点（最早的票，一定在窗口内）等满
	// FillAfterSeconds 后取窗口内全部。
	size := queue.FormableSize(len(within), waited)
	if size == 0 {
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
	return within[:size], true, nil
}

// window is InitialWindow + WidenPerSecond*waited, saturating, then capped.
//
// Score is an int64 whose meaning belongs to the caller, so the API cannot
// assume "reasonable" magnitudes: with signed arithmetic a large
// InitialWindow plus a little widening wrapped negative and the cap compared
// against garbage, and a large WidenPerSecond wrapped the product.
// Saturation is the right answer for a window: past
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
