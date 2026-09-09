package hotcode

import (
	"errors"
	"strings"
	"testing"
)

// U-0149 · C2 · gap map core `hotcode` 5/11：管理命令注册缺注册表拒绝；未知补丁点报 ErrNotFound。
// 插件路径校验按平台分文件（`plugin_guards_promises_test.go` / `plugin_stub_guards_promises_test.go`）。`plugin.go:46`（插件里的 PatchBundle 不实现 Bundle）需要真的 .so 插件，
// 本仓测试不构建插件，记不可测。
func TestHotcodeEntryPointsRefuseMissingRegistryPathsAndPoints(t *testing.T) {
	if err := RegisterAdminCommands(nil); err == nil || !strings.Contains(err.Error(), "admin registry is required") {
		t.Fatalf("RegisterAdminCommands(nil) = %v", err)
	}
	if err := NewRegistry().Revert("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Revert of an unknown point = %v", err)
	}
}
