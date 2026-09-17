package match

import (
	"errors"
	"math"
	"testing"
)

// U-0222 · C2 · RR-20260917-02：ScoreWindowGrouping 的距离与窗口算术不得溢出。
// 旧实现 abs(a-b) 用有符号减法：0 与 MinInt64 相减回绕、abs(MinInt64) 仍是负数，
// 远端分数被判进窗口；窗口 Initial + Widen*waited 先溢出为负再与 MaxWindow 比较，cap 失效。
// Score 的语义归调用方，API 没限制区间，所以通用实现不能把回绕当合法距离。
func TestScoreWindowArithmeticDoesNotOverflow(t *testing.T) {
	queue := Queue{Mode: "ranked", GroupSize: 2}
	ticket := func(id string, score int64) Ticket {
		return Ticket{ID: id, Queue: queue, State: TicketWaiting, ExpiresAtUnix: 2000, CreatedAtUnix: 1000,
			Subject: Subject{Kind: "player", ID: int64(len(id)), Score: score}}
	}
	for _, tc := range []struct {
		label      string
		grouping   ScoreWindowGrouping
		now        int64
		a, b       int64
		wantFormed bool
	}{
		{"zero vs MinInt64 outside a window of 10", ScoreWindowGrouping{InitialWindow: 10}, 1000, 0, math.MinInt64, false},
		{"MaxInt64 vs MinInt64 outside a window of 10", ScoreWindowGrouping{InitialWindow: 10}, 1000, math.MaxInt64, math.MinInt64, false},
		{"window addition saturates then is capped", ScoreWindowGrouping{InitialWindow: math.MaxInt64 - 5, WidenPerSecond: 10, MaxWindow: 100}, 1001, 0, 5, true},
		{"window multiplication saturates then is capped", ScoreWindowGrouping{InitialWindow: 10, WidenPerSecond: math.MaxInt64/2 + 1, MaxWindow: 100}, 1002, 0, 5, true},
		{"ordinary near scores still group", ScoreWindowGrouping{InitialWindow: 10}, 1000, 100, 105, true},
		{"ordinary far scores still wait", ScoreWindowGrouping{InitialWindow: 10}, 1000, 100, 200, false},
		{"negative neighbours group", ScoreWindowGrouping{InitialWindow: 10}, 1000, -100, -95, true},
		{"a clock that went backwards widens nothing", ScoreWindowGrouping{InitialWindow: 10, WidenPerSecond: 100}, 500, 100, 150, false},
	} {
		g := tc.grouping
		now := tc.now
		g.NowUnix = func() int64 { return now }
		_, formed, err := g.Group(queue, []Ticket{ticket("a", tc.a), ticket("bb", tc.b)})
		if err != nil {
			t.Fatalf("%s: %v", tc.label, err)
		}
		if formed != tc.wantFormed {
			t.Fatalf("%s: formed=%v, want %v", tc.label, formed, tc.wantFormed)
		}
	}
}

// Sorting must agree with the filter: with an overflowing distance the far
// candidate could sort ahead of a near one.
func TestScoreWindowOrdersByTrueDistance(t *testing.T) {
	queue := Queue{Mode: "ranked", GroupSize: 2}
	mk := func(id string, playerID, score, created int64) Ticket {
		return Ticket{ID: id, Queue: queue, State: TicketWaiting, ExpiresAtUnix: 9000, CreatedAtUnix: created,
			Subject: Subject{Kind: "player", ID: playerID, Score: score}}
	}
	g := ScoreWindowGrouping{InitialWindow: 0, NowUnix: func() int64 { return 1000 }}
	g.MaxWindow = 0
	// An uncapped window that has widened to "everything" makes ordering the
	// only thing that decides who plays.
	g.WidenPerSecond = math.MaxInt64
	group, formed, err := g.Group(queue, []Ticket{mk("anchor", 1, 0, 1), mk("far", 2, math.MinInt64, 2), mk("near", 3, 5, 3)})
	if err != nil || !formed {
		t.Fatalf("formed=%v err=%v", formed, err)
	}
	if group[1].ID != "near" {
		t.Fatalf("the far candidate sorted ahead of the near one: %v %v", group[0].ID, group[1].ID)
	}
}

func TestScoreWindowRefusesNegativeConfiguration(t *testing.T) {
	queue := Queue{Mode: "ranked", GroupSize: 2}
	for _, g := range []ScoreWindowGrouping{
		{InitialWindow: -1}, {WidenPerSecond: -1}, {MaxWindow: -1},
	} {
		g.NowUnix = func() int64 { return 1000 }
		if _, _, err := g.Group(queue, []Ticket{{ID: "a"}, {ID: "b"}}); !errors.Is(err, ErrQueueInvalid) {
			t.Fatalf("%+v accepted: %v", g, err)
		}
	}
}
