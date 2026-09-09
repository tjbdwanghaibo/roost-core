//go:build darwin || linux || freebsd

package hotcode

import (
	"strings"
	"testing"
)

// U-0149 · C2 · plugin.go:25 / 28：插件路径为空 / 非 .so 在触达 plugin.Open 之前拒绝。
// `plugin.go:46`（插件里的 PatchBundle 不实现 Bundle）需要真的 .so 插件，本仓测试不构建插件，记不可测。
func TestLoadPluginRefusesEmptyAndNonSharedObjectPaths(t *testing.T) {
	if _, err := LoadPlugin(""); err == nil || !strings.Contains(err.Error(), "plugin path required") {
		t.Fatalf("LoadPlugin(\"\") = %v", err)
	}
	if _, err := LoadPlugin("patch.txt"); err == nil || !strings.Contains(err.Error(), "must be a .so file") {
		t.Fatalf("LoadPlugin(non .so) = %v", err)
	}
}
