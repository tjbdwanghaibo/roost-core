//go:build !darwin && !linux && !freebsd

package hotcode

import (
	"strings"
	"testing"
)

// 没有 Go plugin 支持的平台（Windows 等）：LoadPlugin 必须明确说"不支持"，而不是装作加载了什么。
func TestLoadPluginReportsUnsupportedPlatforms(t *testing.T) {
	if _, err := LoadPlugin("patch.so"); err == nil || !strings.Contains(err.Error(), "not supported on this platform") {
		t.Fatalf("LoadPlugin on an unsupported platform = %v", err)
	}
}
