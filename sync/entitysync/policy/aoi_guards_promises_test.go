package policy

import (
	"errors"
	"github.com/tjbdwanghaibo/roost-core/spatial"
	"testing"
)

// U-0149 · C2 · gap map core `spatial` 3/20：移动未知观察者报 ErrInterestUnknown；兴趣簇的块尺寸必须为正。
func TestInterestRefusesUnknownObserversAndZeroBlocks(t *testing.T) {
	manager := interestFixture(t, AOIConfig{})
	if err := manager.MoveObserver(999, spatial.Point{}); !errors.Is(err, ErrInterestUnknown) {
		t.Fatalf("MoveObserver of an unknown observer = %v", err)
	}
	if _, err := NewAOICluster(AOIConfig{EnterRadius: 10, LeaveRadius: 20}); !errors.Is(err, spatial.ErrInvalidBounds) {
		t.Fatalf("NewAOICluster without a block size = %v", err)
	}
}
