package policy

import (
	"errors"
	"github.com/tjbdwanghaibo/roost-core/spatial"
	"testing"
)

func interestConfig() AOIConfig {
	return AOIConfig{Bounds: spatial.Rect{Min: spatial.Point{X: 0, Y: 0}, Max: spatial.Point{X: 1000, Y: 1000}}, BlockSize: 100, EnterRadius: 60, LeaveRadius: 80, Bands: []int64{20, 40}}
}

// The interest manager's configuration is what keeps its box arithmetic from
// wrapping and its hysteresis from oscillating; every invalid shape is
// refused at construction.
func TestNewInterestManagerRefusesEachInvalidConfig(t *testing.T) {
	if _, err := NewAOI(interestConfig()); err != nil {
		t.Fatalf("baseline rejected: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*AOIConfig)
		want   error
	}{
		{"empty bounds", func(c *AOIConfig) {
			c.Bounds = spatial.Rect{Min: spatial.Point{X: 5, Y: 5}, Max: spatial.Point{X: 5, Y: 5}}
		}, spatial.ErrInvalidBounds},
		{"block size zero", func(c *AOIConfig) { c.BlockSize = 0 }, spatial.ErrInvalidBounds},
		{"enter radius zero", func(c *AOIConfig) { c.EnterRadius = 0 }, ErrInterestConfig},
		{"leave radius below enter", func(c *AOIConfig) { c.LeaveRadius = c.EnterRadius - 1 }, ErrInterestConfig},
		{"leave radius would wrap", func(c *AOIConfig) { c.LeaveRadius = maxInterestRadius }, ErrInterestConfig},
		{"band edge zero", func(c *AOIConfig) { c.Bands = []int64{0, 40} }, ErrInterestConfig},
		{"bands not ascending", func(c *AOIConfig) { c.Bands = []int64{40, 40} }, ErrInterestConfig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := interestConfig()
			tc.mutate(&cfg)
			if _, err := NewAOI(cfg); !errors.Is(err, tc.want) {
				t.Fatalf("NewAOI = %v, want %v", err, tc.want)
			}
		})
	}
}

// Subjects and observers live inside the bounds and are addressed by id;
// a point outside the world or an id nobody registered is refused rather
// than indexed into a block that does not exist.
func TestInterestManagerRefusesOutOfBoundsAndUnknownIDs(t *testing.T) {
	m, err := NewAOI(interestConfig())
	if err != nil {
		t.Fatal(err)
	}
	outside := spatial.Point{X: 5000, Y: 5}
	if err := m.AddSubject(1, outside); !errors.Is(err, spatial.ErrInvalidBounds) {
		t.Fatalf("AddSubject outside = %v", err)
	}
	if err := m.AddObserver(9, outside); !errors.Is(err, spatial.ErrInvalidBounds) {
		t.Fatalf("AddObserver outside = %v", err)
	}
	if err := m.MoveSubject(1, spatial.Point{X: 1, Y: 1}); !errors.Is(err, ErrInterestUnknown) {
		t.Fatalf("MoveSubject unknown = %v", err)
	}
	if err := m.RemoveSubject(1); !errors.Is(err, ErrInterestUnknown) {
		t.Fatalf("RemoveSubject unknown = %v", err)
	}
	if err := m.MoveObserver(9, spatial.Point{X: 1, Y: 1}); !errors.Is(err, ErrInterestUnknown) {
		t.Fatalf("MoveObserver unknown = %v", err)
	}
	if err := m.RemoveObserver(9); !errors.Is(err, ErrInterestUnknown) {
		t.Fatalf("RemoveObserver unknown = %v", err)
	}
	if err := m.AddSubject(1, spatial.Point{X: 10, Y: 10}); err != nil {
		t.Fatal(err)
	}
	if err := m.AddObserver(9, spatial.Point{X: 12, Y: 12}); err != nil {
		t.Fatal(err)
	}
	if err := m.MoveSubject(1, outside); !errors.Is(err, spatial.ErrInvalidBounds) {
		t.Fatalf("MoveSubject outside = %v", err)
	}
	if err := m.MoveObserver(9, outside); !errors.Is(err, spatial.ErrInvalidBounds) {
		t.Fatalf("MoveObserver outside = %v", err)
	}
	if got := m.Visible(9); len(got) != 1 || got[0] != 1 {
		t.Fatalf("refused moves must leave positions intact; visible=%v", got)
	}
}
