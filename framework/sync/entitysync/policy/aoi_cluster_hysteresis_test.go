package policy_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// Drive both layouts through the public API against the same explicit timeline.
func testClusterSeamLayouts(t *testing.T, limit int, run func(*testing.T, *policy.AOICluster)) {
	t.Helper()
	for _, mode := range consumerModes {
		for _, layout := range []string{"one_area", "two_areas"} {
			t.Run(string(mode)+"/"+layout, func(t *testing.T) {
				c, err := policy.NewAOICluster(policy.AOIConfig{IndexMode: mode, BlockSize: 8,
					EnterRadius: 8, LeaveRadius: 12, Bands: []int64{5, 10}, MaxVisible: limit})
				mustOK(t, err)
				rect := func(lo, hi int64) spatial.Rect { return spatial.Rect{Min: xy(lo, -32), Max: xy(hi, 32)} }
				if layout == "one_area" {
					mustOK(t, c.AddArea(1, rect(-32, 32)))
				} else {
					mustOK(t, c.AddArea(1, rect(-32, 0)))
					mustOK(t, c.AddArea(2, rect(0, 32)))
				}
				run(t, c)
			})
		}
	}
}

func TestClusterSeamHysteresis(t *testing.T) {
	for _, drain := range []string{"flush_each_move", "flush_after_moves"} {
		t.Run(drain, func(t *testing.T) {
			testClusterSeamLayouts(t, 0, func(t *testing.T, c *policy.AOICluster) {
				const o, s = 101, 2
				mustOK(t, c.AddObserver(o, 0, xy(-7, 0), policy.ObserverView{}))
				mustOK(t, c.AddObserver(102, s, xy(-7, 0), policy.ObserverView{})) // self never enters
				mustOK(t, c.AddObserver(103, 0, xy(0, 9), policy.ObserverView{}))  // never inside enter
				steps := []struct {
					x       int64
					visible bool
					events  []policy.InterestEvent
				}{
					{2, false, nil}, // first encounter at distance 9
					{-1, true, []policy.InterestEvent{enter(o, s, 1)}},
					{1, true, nil}, {-1, true, nil}, {2, true, nil}, {1, true, nil},
					{5, true, []policy.InterestEvent{bandChange(o, s, 2)}}, // inclusive leave radius
					{6, false, []policy.InterestEvent{leave(o, s)}},
					{-16, false, nil}, // crossed back into the annulus after a real Leave
					{1, true, []policy.InterestEvent{enter(o, s, 1)}},        // exact enter radius
					{-19, true, []policy.InterestEvent{bandChange(o, s, 2)}}, // cross to distance 12
					{2, true, []policy.InterestEvent{bandChange(o, s, 1)}},
					{-20, false, []policy.InterestEvent{leave(o, s)}}, // cross to distance 13
				}
				var pending []policy.InterestEvent
				for i, step := range steps {
					if i == 0 {
						mustOK(t, c.AddSubject(s, xy(step.x, 0)))
					} else {
						mustOK(t, c.MoveSubject(s, xy(step.x, 0)))
					}
					var want []int64
					if step.visible {
						want = []int64{s}
					}
					if got := c.Visible(o); !slices.Equal(got, want) {
						t.Fatalf("step %d x=%d Visible(%d)=%v want %v", i, step.x, o, got, want)
					}
					for _, excluded := range []int64{102, 103} {
						if got := c.Visible(excluded); len(got) != 0 {
							t.Fatalf("step %d x=%d Visible(%d)=%v want empty", i, step.x, excluded, got)
						}
					}
					pending = append(pending, step.events...)
					if drain == "flush_each_move" || i == len(steps)-1 {
						checkEvents(t, fmt.Sprintf("step %d x=%d", i, step.x), c.Flush(), pending...)
						pending = nil
					}
				}
				checkEvents(t, "drained", c.Flush())
			})
		})
	}
}

// A globally suppressed subject still has area visibility history. It must be available
// for promotion in the hysteresis annulus without moving again or displacing a nearer member.
func TestClusterSeamHysteresisUnderCapacity(t *testing.T) {
	testClusterSeamLayouts(t, 1, func(t *testing.T, c *policy.AOICluster) {
		const o, near, suppressed = 101, 1, 2
		mustOK(t, c.AddObserver(o, 0, xy(-7, 0), policy.ObserverView{}))
		mustOK(t, c.AddSubject(near, xy(-7, 0)))
		mustOK(t, c.AddSubject(suppressed, xy(-1, 0)))
		checkEvents(t, "nearest only", c.Flush(), enter(o, near, 0))
		mustOK(t, c.AddSubject(suppressed, xy(2, 0))) // re-adding an existing ID moves it across the seam
		if got := c.Visible(o); !slices.Equal(got, []int64{near}) {
			t.Fatalf("after crossing Visible=%v want [%d]", got, near)
		}
		checkEvents(t, "still suppressed", c.Flush())
		mustOK(t, c.RemoveSubject(near))
		if got := c.Visible(o); !slices.Equal(got, []int64{suppressed}) {
			t.Fatalf("after capacity opens Visible=%v want [%d]", got, suppressed)
		}
		checkEvents(t, "promote retained candidate", c.Flush(), leave(o, near), enter(o, suppressed, 1))
	})
}
