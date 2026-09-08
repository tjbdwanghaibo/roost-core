package spatial

import (
	"errors"
	"testing"
)

func interestConfig() InterestConfig {
	return InterestConfig{Bounds: Rect{Min: Point{0, 0}, Max: Point{1000, 1000}}, BlockSize: 100, EnterRadius: 60, LeaveRadius: 80, Bands: []int64{20, 40}}
}

// The interest manager's configuration is what keeps its box arithmetic from
// wrapping and its hysteresis from oscillating; every invalid shape is
// refused at construction.
func TestNewInterestManagerRefusesEachInvalidConfig(t *testing.T) {
	if _, err := NewInterestManager(interestConfig()); err != nil {
		t.Fatalf("baseline rejected: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*InterestConfig)
		want   error
	}{
		{"empty bounds", func(c *InterestConfig) { c.Bounds = Rect{Min: Point{5, 5}, Max: Point{5, 5}} }, ErrInvalidBounds},
		{"block size zero", func(c *InterestConfig) { c.BlockSize = 0 }, ErrInvalidBounds},
		{"enter radius zero", func(c *InterestConfig) { c.EnterRadius = 0 }, ErrInterestConfig},
		{"leave radius below enter", func(c *InterestConfig) { c.LeaveRadius = c.EnterRadius - 1 }, ErrInterestConfig},
		{"leave radius would wrap", func(c *InterestConfig) { c.LeaveRadius = maxInterestRadius }, ErrInterestConfig},
		{"band edge zero", func(c *InterestConfig) { c.Bands = []int64{0, 40} }, ErrInterestConfig},
		{"bands not ascending", func(c *InterestConfig) { c.Bands = []int64{40, 40} }, ErrInterestConfig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := interestConfig()
			tc.mutate(&cfg)
			if _, err := NewInterestManager(cfg); !errors.Is(err, tc.want) {
				t.Fatalf("NewInterestManager = %v, want %v", err, tc.want)
			}
		})
	}
}

// Subjects and observers live inside the bounds and are addressed by id;
// a point outside the world or an id nobody registered is refused rather
// than indexed into a block that does not exist.
func TestInterestManagerRefusesOutOfBoundsAndUnknownIDs(t *testing.T) {
	m, err := NewInterestManager(interestConfig())
	if err != nil {
		t.Fatal(err)
	}
	outside := Point{5000, 5}
	if err := m.AddSubject(1, outside); !errors.Is(err, ErrInvalidBounds) {
		t.Fatalf("AddSubject outside = %v", err)
	}
	if err := m.AddObserver(9, outside); !errors.Is(err, ErrInvalidBounds) {
		t.Fatalf("AddObserver outside = %v", err)
	}
	if err := m.MoveSubject(1, Point{1, 1}); !errors.Is(err, ErrInterestUnknown) {
		t.Fatalf("MoveSubject unknown = %v", err)
	}
	if err := m.RemoveSubject(1); !errors.Is(err, ErrInterestUnknown) {
		t.Fatalf("RemoveSubject unknown = %v", err)
	}
	if err := m.MoveObserver(9, Point{1, 1}); !errors.Is(err, ErrInterestUnknown) {
		t.Fatalf("MoveObserver unknown = %v", err)
	}
	if err := m.RemoveObserver(9); !errors.Is(err, ErrInterestUnknown) {
		t.Fatalf("RemoveObserver unknown = %v", err)
	}
	if err := m.AddSubject(1, Point{10, 10}); err != nil {
		t.Fatal(err)
	}
	if err := m.AddObserver(9, Point{12, 12}); err != nil {
		t.Fatal(err)
	}
	if err := m.MoveSubject(1, outside); !errors.Is(err, ErrInvalidBounds) {
		t.Fatalf("MoveSubject outside = %v", err)
	}
	if err := m.MoveObserver(9, outside); !errors.Is(err, ErrInvalidBounds) {
		t.Fatalf("MoveObserver outside = %v", err)
	}
	if got := m.Visible(9); len(got) != 1 || got[0] != 1 {
		t.Fatalf("refused moves must leave positions intact; visible=%v", got)
	}
}
