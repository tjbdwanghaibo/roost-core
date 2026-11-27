package registry

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompanionPairValidation(t *testing.T) {
	for _, tc := range []struct{ name, parent, child, extra, message string }{
		{"missing", "", "companionOf=Missing", "", "found 0"},
		{"ambiguous", "", "companionOf=Archive", "Archive", "found 2"},
		{"self", "", "companionOf=Journal", "", "itself a companion"},
		{"cycle", "companionOf=Journal", "companionOf=Archive", "", "itself a companion"},
		{"chain", "companionOf=Third", "companionOf=Archive", "Third", "itself a companion"},
		{"different services", "service=auth", "service=game companionOf=Archive", "", "incompatible service owners"},
		{"foreign kind expr", "entityKind=ids.Kind", "companionOf=Archive", "", "package-local kind"},
		// 共享 kind 运行期 RegisterCompanionKind 一定拒绝：生成期就要报出是哪一端，不能编译通过后起服才失败。
		{"shared principal", "remote=managed ownership=shared", "companionOf=Archive", "", "Archive is ownership=shared"},
		{"shared companion", "", "remote=managed ownership=shared companionOf=Archive", "", "Journal is ownership=shared"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			fixture := func(pkg, name, opts string) {
				t.Helper()
				dir := filepath.Join(root, "game", "entities", pkg)
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "entity.go"), []byte(fmt.Sprintf("package %s\n//roost:entity %s\ntype %s struct{}\n", pkg, opts, name)), 0644); err != nil {
					t.Fatal(err)
				}
			}
			fixture("journal", "Journal", tc.child)
			fixture("archive", "Archive", tc.parent)
			if tc.extra != "" {
				fixture("another", tc.extra, "")
			}
			if _, err := companionPairs(root, "test.local"); err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("want %s, got %v", tc.message, err)
			}
		})
	}
}
