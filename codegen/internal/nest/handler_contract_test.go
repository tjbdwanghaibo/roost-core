package nest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Handler声明、远端别名和持久策略的当前契约。
func TestParseFileRefusesValueReceiversAndBadTargetDeclarations(t *testing.T) {
	cases := []struct{ label, source, want string }{
		{"value receiver", `package capability
type BagOwner interface { Bag() any }
type Handler struct{}
//roost:nest target=player
func (h Handler) handlerUse(owner BagOwner) error { return nil }
`, "method handler receiver must be a pointer"},
		{"target and targets together", `package capability
type BagOwner interface { Bag() any }
//roost:nest target=player targets=player
func handlerBoth(owner BagOwner) error { return nil }
`, "target and targets cannot be used together"},
		{"empty target name", `package capability
type BagOwner interface { Bag() any }
type GuildOwner interface { Guild() any }
//roost:nest targets=player,,alliance
func handlerBlank(owner BagOwner, guild GuildOwner) error { return nil }
`, "target names must not be empty"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			path := writeTempGoFile(t, tc.source)
			if _, _, err := parseFile(path); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parseFile = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestParseFileRejectsDuplicateRemoteAliasesWithinAndAcrossFiles(t *testing.T) {
	const head = "package invalid\nimport \"github.com/tjbdwanghaibo/roost-core/framework/entity\"\ntype IPlayerEntity interface{ ID() int64 }\n"
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

func TestGeneratePipelinedHandlerMeta(t *testing.T) {
	path := writeTempGoFile(t, `package capability
type Player interface { ID() int64 }
//roost:nest target=player rollback=undo durability=pipelined
func handlerSave(player Player) error { return nil }
`)
	funcs, pkg, err := parseFile(path)
	if err != nil {
		t.Fatalf("pipelined marker rejected: %v", err)
	}
	out := filepath.Join(t.TempDir(), "handler_nest_gen.go")
	if _, err := generate(funcs, pkg, out, true, false, "RegisterHandlers"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityPipelined}") {
		t.Fatalf("pipelined registration missing: %s", data)
	}
}
