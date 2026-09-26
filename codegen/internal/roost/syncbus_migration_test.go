package roost

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestConsolidationMapSyncBusLegacyPaths(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "go.mod", legacyGoMod)
	file := writeProjectFile(t, root, "wiring.go", `package wiring
import (
 oldkit "github.com/tjbdwanghaibo/roost-core/kit/room"
 olddriver "github.com/tjbdwanghaibo/roost-core/room"
)
var _ = oldkit.NewRoomMod
var _ oldkit.RoomMod
var _ = olddriver.NewNatsSyncBus
`)
	_, err := ConsolidateProject(root, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"roost-core/kit/syncbus", "roost-core/sync/syncbus/driver", "oldkit.NewSyncBusMod", "oldkit.SyncBusMod"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("missing %s: %s", want, raw)
		}
	}
}

// RR-20260926-24（复核残留）：改写后保留在 kit 的 import 若包名变化（roost-kit/room →
// kit/syncbus，包名 room → syncbus），原来不带别名的 import 必须补上文件在用的别名；
// 否则 `room.NewSyncBusMod` 变成 `undefined: room`。探针见 REPRO-2026-09-26-03 §10。
func TestConsolidationKeepsTheFilesNameForARenamedKitPackage(t *testing.T) {
	cases := map[string]struct {
		src  string
		want []string
	}{
		"oldkit-kitonly-unaliased": {
			src: `package wiring
import "github.com/tjbdwanghaibo/roost-kit/room"
var _ = room.NewRoomMod(0)
`,
			want: []string{`room "github.com/tjbdwanghaibo/roost-core/kit/syncbus"`, "room.NewSyncBusMod(0)"},
		},
		"oldkit-mixed-unaliased": {
			src: `package wiring
import (
	"github.com/tjbdwanghaibo/roost-kit/room"
)
var _ = room.NewRoomMod(0)
var _ = room.NewNatsSyncBus
`,
			want: []string{`room "github.com/tjbdwanghaibo/roost-core/kit/syncbus"`, "room.NewSyncBusMod(0)", `"github.com/tjbdwanghaibo/roost-core/sync/syncbus/driver"`},
		},
		"corekitroom-unaliased": {
			src: `package wiring
import "github.com/tjbdwanghaibo/roost-core/kit/room"
var _ = room.NewRoomMod(0)
`,
			want: []string{`room "github.com/tjbdwanghaibo/roost-core/kit/syncbus"`, "room.NewSyncBusMod(0)"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeProjectFile(t, root, "go.mod", legacyGoMod)
			file := writeProjectFile(t, root, "wiring.go", tc.src)
			if _, err := ConsolidateProject(root, false, nil); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			assertPackageQualifiersResolve(t, raw)
			for _, want := range tc.want {
				if !strings.Contains(string(raw), want) {
					t.Fatalf("missing %s:\n%s", want, raw)
				}
			}
		})
	}
}

// assertPackageQualifiersResolve 检查每个 `X.Sel` 的包限定符 X 都由某个 import 声明：
// 显式别名，或该路径的真实包名（迁移表 layout 阶段登记的 package，否则取路径末段）。
func assertPackageQualifiersResolve(t *testing.T, src []byte) {
	t.Helper()
	_, m, err := loadConsolidationMap()
	if err != nil {
		t.Fatal(err)
	}
	packageNames := map[string]string{}
	for _, move := range m.Layout.Move {
		if move.Package != "" {
			packageNames[move.To] = move.Package
		}
	}
	file, err := parser.ParseFile(token.NewFileSet(), "wiring.go", src, 0)
	if err != nil {
		t.Fatalf("rewritten file does not parse: %v\n%s", err, src)
	}
	declared := map[string]bool{}
	for _, imp := range file.Imports {
		importPath, _ := strconv.Unquote(imp.Path.Value)
		switch {
		case imp.Name != nil:
			declared[imp.Name.Name] = true
		case packageNames[importPath] != "":
			declared[packageNames[importPath]] = true
		default:
			declared[path.Base(importPath)] = true
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Obj == nil && !declared[id.Name] {
			t.Errorf("undefined: %s (in %s.%s)\n%s", id.Name, id.Name, sel.Sel.Name, src)
		}
		return true
	})
}

// RR-20260926-24（复核残留）：v1.16.1 的 game-demo 升级后还引用已删除 / 改名 / 换包的
// 符号（spatial.InterestEvent、statesync.Reassembler / SessionID、room.DecodeRoomWireFrame……），
// 旧实现改完 import 就退出码 0，留给编译器报 `undefined`。无法机械改写的符号必须由
// upgrade 自己列出 文件:行 与迁移指引，并以错误退出；用户改完重跑即通过。
func TestConsolidationReportsRemovedSymbolsInsteadOfSucceeding(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "go.mod", "module example.com/planet\n\ngo 1.27.0\n\nrequire github.com/tjbdwanghaibo/roost-core v1.16.1\n")
	writeProjectFile(t, root, "game/scene/scene.go", `package scene

import "github.com/tjbdwanghaibo/roost-core/spatial"

type Scene struct {
	events []spatial.InterestEvent
	point  spatial.Vec2
}
`)
	writeProjectFile(t, root, "cmd/loadtest/main.go", `package main

import (
	corestate "github.com/tjbdwanghaibo/roost-core/statesync"
	"github.com/tjbdwanghaibo/roost-core/room"
)

var reassembler = corestate.NewReassembler
var session corestate.SessionID

func main() { _, _ = room.DecodeRoomWireFrame(nil) }
`)
	var stdout strings.Builder
	_, err := ConsolidateProject(root, false, &stdout)
	if err == nil {
		t.Fatalf("upgrade reported success although the project still uses removed symbols:\n%s", stdout.String())
	}
	for _, want := range []string{
		"game/scene/scene.go:6: spatial.InterestEvent",
		"cmd/loadtest/main.go:8: corestate.NewReassembler",
		"cmd/loadtest/main.go:9: corestate.SessionID",
		"cmd/loadtest/main.go:11: room.DecodeRoomWireFrame",
		"policy",       // spatial → sync/entitysync/policy
		"nettransport", // statesync.SessionID → sync/nettransport
		"entitysync.DecodeFrame",
		"CHANGELOG",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("report does not mention %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "spatial.Vec2") {
		t.Errorf("a symbol that still exists was reported:\n%v", err)
	}
	// The mechanical part is done either way, so a rerun only has the manual
	// part left.
	raw, err := os.ReadFile(filepath.Join(root, "cmd/loadtest/main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"github.com/tjbdwanghaibo/roost-core/sync/frame"`) {
		t.Fatalf("imports were not rewritten:\n%s", raw)
	}
	// The user follows the guide; the next run succeeds.
	writeProjectFile(t, root, "cmd/loadtest/main.go", `package main

import "github.com/tjbdwanghaibo/roost-core/sync/entitysync"

func main() { _ = entitysync.DecodeFrame }
`)
	writeProjectFile(t, root, "game/scene/scene.go", `package scene

import (
	"github.com/tjbdwanghaibo/roost-core/spatial"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync/policy"
)

type Scene struct {
	events []policy.InterestEvent
	point  spatial.Vec2
}
`)
	if _, err := ConsolidateProject(root, false, nil); err != nil {
		t.Fatalf("a project with no removed symbols left still fails: %v", err)
	}
}

// 迁移表里每个“已删除”符号在它登记的新路径上确实不存在，且每组都有指引——表不能列出
// 仍然存在的符号（会误报），也不能是一句空话。
func TestConsolidationRemovedSymbolsAreReallyGone(t *testing.T) {
	_, m, err := loadConsolidationMap()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Layout.Removed) == 0 {
		t.Fatal("the layout stage lists no removed symbols")
	}
	for _, group := range m.Layout.Removed {
		if strings.TrimSpace(group.Guide) == "" || len(group.Symbols) == 0 {
			t.Errorf("%s: a removed-symbol group needs a guide and symbols", group.Package)
		}
		dir := filepath.Join("..", "..", "..", filepath.FromSlash(strings.TrimPrefix(group.Package, "github.com/tjbdwanghaibo/roost-core/")))
		exported := exportedTopLevel(t, dir)
		for _, symbol := range group.Symbols {
			if exported[symbol] {
				t.Errorf("%s.%s is listed as removed but still exists", group.Package, symbol)
			}
		}
	}
}

func exportedTopLevel(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("%s: %v", dir, err)
	}
	out := map[string]bool{}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if decl.Recv == nil {
					out[decl.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					switch spec := spec.(type) {
					case *ast.TypeSpec:
						out[spec.Name.Name] = true
					case *ast.ValueSpec:
						for _, id := range spec.Names {
							out[id.Name] = true
						}
					}
				}
			}
		}
	}
	return out
}
