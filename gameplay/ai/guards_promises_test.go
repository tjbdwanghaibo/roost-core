package ai

import (
	"errors"
	"strings"
	"testing"
)

type refusingStrategy struct{ controllerTestStrategy }

func (*refusingStrategy) CanStopByNext(Strategy) bool { return false }

// U-0148 · C2 · gap map core `ai` 7/20：nil 控制器 / 切换中 / 当前策略拒绝被替换三种 SetStrategy
// 拒绝；ParseTree 无注册表、时间节点无时钟、谓词过深、inverter 谓词缺 child 各报 ErrTreeInvalid。
func TestSetStrategyRefusesNilReentrantAndRejectedSwitches(t *testing.T) {
	var none *Controller
	if err := none.SetStrategy(&controllerTestStrategy{name: "x"}); !errors.Is(err, ErrStrategyInit) {
		t.Fatalf("SetStrategy on a nil controller = %v", err)
	}
	controller := NewController(ControllerHooks{})
	controller.switching = true
	if err := controller.SetStrategy(&controllerTestStrategy{name: "x"}); !errors.Is(err, ErrReentrantSwitch) {
		t.Fatalf("SetStrategy while switching = %v", err)
	}
	controller.switching = false
	holder := &refusingStrategy{controllerTestStrategy{name: "holder"}}
	if err := controller.SetStrategy(holder); err != nil {
		t.Fatal(err)
	}
	if err := controller.SetStrategy(&controllerTestStrategy{name: "next"}); !errors.Is(err, ErrStrategyRejected) {
		t.Fatalf("SetStrategy over a strategy that refuses to stop = %v", err)
	}
	if controller.Strategy() != holder || holder.stopCount != 0 {
		t.Fatal("a rejected switch disturbed the current strategy")
	}
}

func TestParseTreeRefusesMissingRegistryClockAndMalformedPredicates(t *testing.T) {
	leaf := `{"node":"condition","name":"hp_below","args":{}}`
	if _, err := ParseTree[treeCtx]([]byte(`{"schema":"roost.ai/v1","root":`+leaf+`}`), nil); !errors.Is(err, ErrTreeInvalid) || !strings.Contains(err.Error(), "registry is required") {
		t.Fatalf("ParseTree(nil registry) = %v", err)
	}
	clockless := NewRegistry[treeCtx](nil)
	if _, err := ParseTree([]byte(`{"schema":"roost.ai/v1","root":{"node":"cooldown","ticks":3,"child":`+leaf+`}}`), clockless); !errors.Is(err, ErrTreeInvalid) || !strings.Contains(err.Error(), "no tick clock") {
		t.Fatalf("cooldown without a registry clock = %v", err)
	}
	registry := wireRegistry(t)
	deep := leaf
	for i := 0; i <= maxTreeDepth; i++ {
		deep = `{"node":"inverter","child":` + deep + `}`
	}
	if _, err := ParseTree([]byte(`{"schema":"roost.ai/v1","root":{"node":"guard","condition":`+deep+`,"child":`+leaf+`}}`), registry); !errors.Is(err, ErrTreeInvalid) || !strings.Contains(err.Error(), "exceeds depth") || !strings.Contains(err.Error(), "condition") {
		t.Fatalf("guard predicate nested too deep = %v", err)
	}
	if _, err := ParseTree([]byte(`{"schema":"roost.ai/v1","root":{"node":"guard","condition":{"node":"inverter"},"child":`+leaf+`}}`), registry); !errors.Is(err, ErrTreeInvalid) || !strings.Contains(err.Error(), "condition.child is required") {
		t.Fatalf("inverter predicate without a child = %v", err)
	}
	if _, err := ParseTree([]byte(`{"schema":"roost.ai/v1","root":{"node":"guard","condition":{"node":"inverter","child":`+leaf+`},"child":`+leaf+`}}`), registry); err != nil {
		t.Fatalf("valid inverter predicate refused: %v", err)
	}
}
