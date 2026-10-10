package policy

import (
	"errors"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
	"testing"
)

func TestObserverBlockBudgetRefusesARatioThatSubscribesTheWholeMap(t *testing.T) {
	config := AOIConfig{
		Bounds:      spatial.Rect{Max: spatial.Point{X: 10000, Y: 10000}},
		BlockSize:   10,
		EnterRadius: 900,
		LeaveRadius: 1000,
	}
	if _, err := NewAOI(config); !errors.Is(err, ErrInterestBudget) {
		t.Fatalf("a configuration where one observer covers 40401 blocks was accepted: err=%v", err)
	}
}

// The budget is a configuration, not a constant: a deployment that really
// wants a wide view says so.
func TestObserverBlockBudgetIsConfigurable(t *testing.T) {
	config := AOIConfig{
		Bounds:            spatial.Rect{Max: spatial.Point{X: 10000, Y: 10000}},
		BlockSize:         10,
		EnterRadius:       900,
		LeaveRadius:       1000,
		MaxObserverBlocks: 100000,
	}
	if _, err := NewAOI(config); err != nil {
		t.Fatalf("an explicit budget did not admit the configuration: %v", err)
	}
}

// The ordinary shape — a block about the size of the view — is nowhere near
// the default budget, and the worst case is computed rather than sampled: a
// manager that only refused after an observer arrived would refuse in
// production and pass every start-up check.
func TestTheNineBlockShapeFitsTheDefaultBudget(t *testing.T) {
	manager, err := NewAOI(AOIConfig{
		Bounds:      spatial.Rect{Max: spatial.Point{X: 10000, Y: 10000}},
		BlockSize:   150,
		EnterRadius: 120,
		LeaveRadius: 150,
	})
	if err != nil {
		t.Fatalf("the classic nine-block configuration was refused: %v", err)
	}
	if err := manager.AddObserver(1, spatial.Point{X: 5000, Y: 5000}); err != nil {
		t.Fatalf("add observer: %v", err)
	}
	if blocks := len(manager.observers[1].blocks); blocks > 16 {
		t.Fatalf("a block-sized view subscribed to %d blocks", blocks)
	}
}

// A radius so large that the box arithmetic would overflow must be refused by
// the budget rather than wrap into a small number.
func TestObserverBlockBudgetSaturatesInsteadOfWrapping(t *testing.T) {
	config := AOIConfig{
		Bounds:      spatial.Rect{Max: spatial.Point{X: 1 << 40, Y: 1 << 40}},
		BlockSize:   1,
		EnterRadius: 1 << 40,
		LeaveRadius: 1 << 41,
	}
	if _, err := NewAOI(config); err == nil {
		t.Fatal("a configuration whose observer box covers the whole map was accepted")
	}
}

func TestInterestRefusesUnknownObserversAndZeroBlocks(t *testing.T) {
	manager := interestFixture(t, AOIConfig{})
	if err := manager.MoveObserver(999, spatial.Point{}); !errors.Is(err, ErrInterestUnknown) {
		t.Fatalf("MoveObserver of an unknown observer = %v", err)
	}
	if _, err := NewAOICluster(AOIConfig{EnterRadius: 10, LeaveRadius: 20}); !errors.Is(err, spatial.ErrInvalidBounds) {
		t.Fatalf("NewAOICluster without a block size = %v", err)
	}
}
