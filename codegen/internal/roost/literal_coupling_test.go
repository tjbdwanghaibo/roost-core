package roost

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/dao"
	codeerr "github.com/tjbdwanghaibo/roost-core/codegen/internal/errcode"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/protocol"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/tablegen"
)

// 编排器不重复各生成器的默认路径字面量（"./db/def"、"./protocol/player_bind"、"/game/player_agent" ……），
// 只引用生成器导出的常量：否则生成器改了默认值，它自己照常工作，却与 `roost generate` 静默不一致。
func TestOrchestratorDoesNotRepeatGeneratorDefaultPaths(t *testing.T) {
	shared := map[string]string{
		dao.DefaultDefDir:                "dao.DefaultDefDir",
		dao.DefaultOutDir:                "dao.DefaultOutDir",
		codeerr.DefaultOutFile:           "codeerr.DefaultOutFile",
		protocol.DefaultDefDir:           "protocol.DefaultDefDir",
		protocol.DefaultBindDir:          "protocol.DefaultBindDir",
		protocol.DefaultHandlerDir:       "protocol.DefaultHandlerDir",
		protocol.PlayerAgentImportSuffix: "protocol.PlayerAgentImportSuffix",
		protocol.PBImportSuffix:          "protocol.PBImportSuffix",
		tablegen.DefaultMetaDir:          "tablegen.DefaultMetaDir",
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			lit, ok := node.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if constant, coupled := shared[value]; coupled {
				t.Errorf("%s: literal %q repeats %s; refer to the constant instead", fset.Position(lit.Pos()), value, constant)
			}
			return true
		})
	}
	_ = filepath.Join
}
