package policy

import (
	"errors"
	"github.com/tjbdwanghaibo/roost-core/spatial"
	"testing"
)

// U-0240 · C4 · RR-20260918-08：一个观察者能订阅多少格必须有上界，而且要在
// 构造时就拒绝，不能等到运行起来才发现。
//
// 旧行为：validate 只查半径与分带，spatial.BlockIndex 只拦整图格数，而
// resubscribe 按 LeaveRadius 的包围盒遍历全部格子。Bounds 10000×10000、
// BlockSize 10、Leave 1000 是一份合法配置，每个观察者实际登记 40,401 个格
// ——每格两处 map 条目，且观察者每动一步都要对这个集合做一次差分。配错了不
// 报错，只是慢慢变慢。
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
