package webroute

import (
	"strings"
	"testing"
)

// U-0149 · C2 · gap map core `webroute` 1/6：nil 模块拒绝注册，且之前的模块已注册、之后的不再注册。
func TestRegisterModulesRefusesANilModule(t *testing.T) {
	calls := 0
	err := RegisterModules(nil, testModule{calls: &calls}, nil, testModule{calls: &calls})
	if err == nil || !strings.Contains(err.Error(), "route module is required") {
		t.Fatalf("RegisterModules with a nil module = %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (stop at the nil module)", calls)
	}
}
