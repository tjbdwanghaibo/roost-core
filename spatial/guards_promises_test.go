package spatial

import (
	"errors"
	"testing"
)

// U-0149 · C2 · gap map core `spatial` 3/20：移动未知观察者报 ErrInterestUnknown；兴趣簇的块尺寸必须为正。
func TestInterestRefusesUnknownObserversAndZeroBlocks(t *testing.T) {
	manager := interestFixture(t, InterestConfig{})
	if err := manager.MoveObserver(999, Point{}); !errors.Is(err, ErrInterestUnknown) {
		t.Fatalf("MoveObserver of an unknown observer = %v", err)
	}
	if _, err := NewInterestCluster(InterestConfig{EnterRadius: 10, LeaveRadius: 20}); !errors.Is(err, ErrInvalidBounds) {
		t.Fatalf("NewInterestCluster without a block size = %v", err)
	}
}
