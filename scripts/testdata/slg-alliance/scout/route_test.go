package scout

import (
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
	"math"
	"testing"
)

func TestRouteBoundsAndInterpolation(t *testing.T) {
	r := Route{From: spatial.Point{X: 950, Y: 950}, To: spatial.Point{X: 700, Y: 700}, Start: 1000, End: 11000}
	for _, tt := range []struct{ now, want int64 }{{math.MinInt64, 950}, {1000, 950}, {6000, 825}, {10000, 725}, {11000, 700}, {math.MaxInt64, 700}} {
		p, e := r.At(tt.now)
		if e != nil || p.X != tt.want || p.Y != tt.want {
			t.Fatalf("at %d = %+v %v", tt.now, p, e)
		}
	}
	for _, bad := range []Route{{Start: 0, End: 1}, {Start: 1, End: 1}, {Start: 2, End: 1}, {Start: 1, End: math.MaxInt64}, {From: spatial.Point{X: -1}, Start: 1, End: 2}} {
		if _, e := bad.At(1); e == nil {
			t.Fatal("invalid route accepted", bad)
		}
	}
}
