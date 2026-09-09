package scenario_test

import (
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/robot/scenario"
)

// U-0149 · C2 · gap map core `robot/scenario` 2/10：nil 注册表不能注册；声明式树里的空节点报"node is required"。
func TestRegistryAndSpecRefuseNilNodes(t *testing.T) {
	var none *scenario.Registry
	if err := none.Register(nil); err == nil || !strings.Contains(err.Error(), "registry is nil") {
		t.Fatalf("Register on a nil registry = %v", err)
	}
	_, err := scenario.ParseSpec([]byte("scenarios:\n  - name: x\n    node:\n      sequence:\n        - action: a\n        - ~\n"))
	if err == nil || !strings.Contains(err.Error(), "node is required") {
		t.Fatalf("ParseSpec with a null sequence element = %v", err)
	}
}
