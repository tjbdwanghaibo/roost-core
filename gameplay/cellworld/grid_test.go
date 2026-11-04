package cellworld

import (
	"errors"
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 2×2 网格，格边长 1000，边界带 100，迟滞 20；sid 按 101 + row*2 + col。
func testGrid2x2(t *testing.T) *Grid {
	t.Helper()
	g, err := NewGrid(GridConfig{
		Origin: spatial.Point{X: 0, Y: 0}, CellSize: 1000, Cols: 2, Rows: 2, GhostBand: 100, Hysteresis: 20,
		Owners: map[CellID]int32{{0, 0}: 101, {1, 0}: 102, {0, 1}: 103, {1, 1}: 104},
	})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestGridRejectsInvalidConfig(t *testing.T) {
	owners := map[CellID]int32{{0, 0}: 1, {1, 0}: 2}
	for name, cfg := range map[string]GridConfig{
		"zero size":        {CellSize: 0, Cols: 2, Rows: 1, GhostBand: 1, Owners: owners},
		"band >= size":     {CellSize: 10, Cols: 2, Rows: 1, GhostBand: 10, Owners: owners},
		"zero band":        {CellSize: 10, Cols: 2, Rows: 1, GhostBand: 0, Owners: owners},
		"hysteresis>=band": {CellSize: 10, Cols: 2, Rows: 1, GhostBand: 3, Hysteresis: 3, Owners: owners},
		"missing owner":    {CellSize: 10, Cols: 2, Rows: 1, GhostBand: 3, Owners: map[CellID]int32{{0, 0}: 1}},
		"duplicate sid":    {CellSize: 10, Cols: 2, Rows: 1, GhostBand: 3, Owners: map[CellID]int32{{0, 0}: 1, {1, 0}: 1}},
		"owner outside":    {CellSize: 10, Cols: 2, Rows: 1, GhostBand: 3, Owners: map[CellID]int32{{0, 0}: 1, {1, 0}: 2, {5, 5}: 3}},
	} {
		if _, err := NewGrid(cfg); !errors.Is(err, ErrGridConfig) {
			t.Errorf("%s: err = %v, want ErrGridConfig", name, err)
		}
	}
}

// 分区映射：半开区间，边界线属于右 / 上侧；世界外没有格；sid 双向映射。
func TestGridCellAtAndOwners(t *testing.T) {
	g := testGrid2x2(t)
	cases := []struct {
		p    spatial.Point
		want CellID
		ok   bool
	}{
		{spatial.Point{X: 0, Y: 0}, CellID{0, 0}, true},
		{spatial.Point{X: 999, Y: 999}, CellID{0, 0}, true},
		{spatial.Point{X: 1000, Y: 0}, CellID{1, 0}, true},
		{spatial.Point{X: 0, Y: 1000}, CellID{0, 1}, true},
		{spatial.Point{X: 1999, Y: 1999}, CellID{1, 1}, true},
		{spatial.Point{X: 2000, Y: 0}, CellID{}, false},
		{spatial.Point{X: -1, Y: 0}, CellID{}, false},
	}
	for _, c := range cases {
		got, ok := g.CellAt(c.p)
		if got != c.want || ok != c.ok {
			t.Errorf("CellAt(%v) = %v,%v want %v,%v", c.p, got, ok, c.want, c.ok)
		}
	}
	for _, id := range g.Cells() {
		back, ok := g.CellOf(g.SID(id))
		if !ok || back != id {
			t.Errorf("CellOf(SID(%v)) = %v,%v", id, back, ok)
		}
	}
	if got := g.Rect(CellID{1, 1}); got != (spatial.Rect{Min: spatial.Point{X: 1000, Y: 1000}, Max: spatial.Point{X: 2000, Y: 2000}}) {
		t.Errorf("Rect(1,1) = %v", got)
	}
}

// 边界带判定：离边界 ≤ G 的点向邻格发 Ghost；四格交点附近同时命中三个邻格；带外不发；越界未交接时目标格也命中。
func TestGridGhostTargetsAndHandoff(t *testing.T) {
	g := testGrid2x2(t)
	a := CellID{0, 0}
	cases := []struct {
		name string
		p    spatial.Point
		want []CellID
	}{
		{"interior", spatial.Point{X: 500, Y: 500}, nil},
		{"east band", spatial.Point{X: 950, Y: 500}, []CellID{{1, 0}}},
		{"band edge inclusive", spatial.Point{X: 900, Y: 500}, []CellID{{1, 0}}},
		{"just outside band", spatial.Point{X: 899, Y: 500}, nil},
		{"north band", spatial.Point{X: 500, Y: 950}, []CellID{{0, 1}}},
		{"corner", spatial.Point{X: 950, Y: 950}, []CellID{{1, 0}, {0, 1}, {1, 1}}},
		{"crossed within hysteresis", spatial.Point{X: 1010, Y: 500}, []CellID{{1, 0}}},
	}
	for _, c := range cases {
		got := g.GhostTargets(a, c.p, nil)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: GhostTargets(%v) = %v want %v", c.name, c.p, got, c.want)
		}
	}
	if !g.InBand(a, spatial.Point{X: 1099, Y: 500}) || g.InBand(a, spatial.Point{X: 1100, Y: 500}) {
		t.Error("InBand: band of (0,0) extends to x < 1100")
	}
	for _, c := range []struct {
		p    spatial.Point
		want CellID
		ok   bool
	}{
		{spatial.Point{X: 999, Y: 500}, CellID{}, false},
		{spatial.Point{X: 1019, Y: 500}, CellID{}, false}, // 迟滞内
		{spatial.Point{X: 1020, Y: 500}, CellID{1, 0}, true},
		{spatial.Point{X: 1020, Y: 1020}, CellID{1, 1}, true},
		{spatial.Point{X: -100, Y: 500}, CellID{}, false}, // 世界外
	} {
		got, ok := g.ShouldHandoff(a, c.p)
		if got != c.want || ok != c.ok {
			t.Errorf("ShouldHandoff(%v) = %v,%v want %v,%v", c.p, got, ok, c.want, c.ok)
		}
	}
}

// 版本序：先比 epoch 再比 seq；相等不算更新。
func TestVersionOrdering(t *testing.T) {
	cases := []struct {
		v, old Version
		newer  bool
	}{
		{Version{1, 5}, Version{1, 4}, true},
		{Version{1, 4}, Version{1, 4}, false},
		{Version{1, 3}, Version{1, 4}, false},
		{Version{2, 0}, Version{1, 99}, true},
		{Version{1, 99}, Version{2, 0}, false},
	}
	for _, c := range cases {
		if got := c.v.Newer(c.old); got != c.newer {
			t.Errorf("%v.Newer(%v) = %v want %v", c.v, c.old, got, c.newer)
		}
	}
	if !(Version{1, 4}).AtLeast(Version{1, 4}) || (Version{1, 3}).AtLeast(Version{1, 4}) {
		t.Error("AtLeast")
	}
}
