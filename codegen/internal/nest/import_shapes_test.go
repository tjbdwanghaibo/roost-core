package nest

import (
	"reflect"
	"testing"
)

// 外部类型被指针、切片或 map 包装后仍需保留 import；否则生成校验会拒绝合法 handler。
func TestCollectUsedImportsHandlesWrappedPackageTypes(t *testing.T) {
	imports := []ImportInfo{{Path: "example.com/policy"}, {Path: "example.com/sync", Alias: "entitysync"}, {Path: "example.com/unused"}}
	funcs := []*FuncInfo{{Params: []NonEntityParam{{Type: "*policy.Interest"}, {Type: "map[string][]*entitysync.Manager"}}}}
	got := collectUsedImports(funcs, imports)
	if !reflect.DeepEqual(got, imports[:2]) {
		t.Fatalf("imports=%+v want %+v", got, imports[:2])
	}
}
