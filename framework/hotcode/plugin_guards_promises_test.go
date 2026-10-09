//go:build darwin || linux || freebsd

package hotcode

import (
	"strings"
	"testing"
)

// U-0149 · C2 · plugin.go:25 / 28：插件路径为空 / 非 .so 在触达 plugin.Open 之前拒绝。
// 真实 .so 的加载、PatchBundle 解析与部分应用回滚在独立测试包 hotcode/plugintest 里现场构建插件验证
// （RR-20261005-NC-244 / NC-245）。
func TestLoadPluginRefusesEmptyAndNonSharedObjectPaths(t *testing.T) {
	if _, err := LoadPlugin(""); err == nil || !strings.Contains(err.Error(), "plugin path required") {
		t.Fatalf("LoadPlugin(\"\") = %v", err)
	}
	if _, err := LoadPlugin("patch.txt"); err == nil || !strings.Contains(err.Error(), "must be a .so file") {
		t.Fatalf("LoadPlugin(non .so) = %v", err)
	}
}
