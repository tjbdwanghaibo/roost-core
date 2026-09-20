package nest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// U-0116 · C2（空洞测试）· nightly gap map `internal/nest` 8/20。
//
// 远端别名在一个处理器参数类型上必须唯一——同一结构体内两处同名别名，或同
// 一类型名在包内两个文件里各带一处同名别名，都会让生成的访问器互相覆盖；
// 模块发现必须在 go.mod 缺 module 行、向上找不到 go.mod、目录在模块根之外时
// 各自报错，而不是生成一个 import 路径错误的 bootstrap。

func TestParseFileRejectsDuplicateRemoteAliasesWithinAndAcrossFiles(t *testing.T) {
	const head = "package invalid\nimport \"github.com/tjbdwanghaibo/roost-core/entity\"\ntype IPlayerEntity interface{ ID() int64 }\n"
	dir := t.TempDir()
	write := func(name, src string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	// 同一结构体：两个字段显式声明同一个 alias。
	within := write("within.go", head+"type Req struct {\n\tA entity.RemoteViewRef `remote:\"alias=target,view.PlayerViewMapSnapshot\"`\n\tB entity.RemoteViewRef `remote:\"alias=target,view.OtherSnapshot\"`\n}\n//roost:nest\nfunc handlerWithin(p IPlayerEntity, req Req) {}\n")
	if _, _, err := parseFile(within); err == nil || !strings.Contains(err.Error(), `duplicate remote alias "target" on Req`) {
		t.Fatalf("duplicate alias within one struct = %v", err)
	}
	if err := os.Remove(within); err != nil {
		t.Fatal(err)
	}

	// 跨文件：包内另一个文件对同名类型再声明一次同一个 alias（解析器不做类型检查，
	// 只按类型名聚合）。
	handler := write("handler.go", head+"type Req struct {\n\tA entity.RemoteViewRef `remote:\"alias=target,view.PlayerViewMapSnapshot\"`\n}\n//roost:nest\nfunc handlerAcross(p IPlayerEntity, req Req) {}\n")
	write("other.go", head+"type Req struct {\n\tB entity.RemoteViewRef `remote:\"alias=target,view.OtherSnapshot\"`\n}\n")
	if _, _, err := parseFile(handler); err == nil || !strings.Contains(err.Error(), `duplicate remote alias "target" on Req`) {
		t.Fatalf("duplicate alias across files = %v", err)
	}
}

func TestModuleDiscoveryRejectsBrokenOrMissingGoMod(t *testing.T) {
	root := t.TempDir()
	// （"module" 后面只有空白的行在整行 TrimSpace 之后不再带 "module " 前缀，
	// 所以 "empty module path" 这条守卫从文件内容不可达；这里只钉能到的两条。）
	// 一路向上都没有 go.mod。
	orphan := filepath.Join(t.TempDir(), "a", "b")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := findModuleInfo(orphan); err == nil || !strings.Contains(err.Error(), "go.mod not found from") {
		t.Fatalf("no go.mod anywhere = %v", err)
	}
	// 目录在模块根之外。
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/game\n\ngo 1.27.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := importPathForDir(root, "example.com/game", orphan); err == nil || !strings.Contains(err.Error(), "is outside module root") {
		t.Fatalf("directory outside the module root = %v", err)
	}
	inside := filepath.Join(root, "internal", "nest")
	if got, err := importPathForDir(root, "example.com/game", inside); err != nil || got != "example.com/game/internal/nest" {
		t.Fatalf("import path inside the module = (%q, %v)", got, err)
	}
}
