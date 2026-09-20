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

	"github.com/tjbdwanghaibo/roost-codegen/internal/dao"
	codeerr "github.com/tjbdwanghaibo/roost-codegen/internal/errcode"
	"github.com/tjbdwanghaibo/roost-codegen/internal/eventgen"
	"github.com/tjbdwanghaibo/roost-codegen/internal/protocol"
	"github.com/tjbdwanghaibo/roost-codegen/internal/tablegen"
)

// U-0118 (C4, classscan O-1): the orchestrator used to repeat every generator's
// default path as its own literal — "./db/def", "./protocol/player_bind",
// "/game/player_agent", ... — so a generator that moved its default would
// keep working on its own and silently disagree with `roost generate`. The
// generators now export those defaults and this package refers to them; a
// literal equal to one of them anywhere in this package is a regression.
func TestOrchestratorDoesNotRepeatGeneratorDefaultPaths(t *testing.T) {
	shared := map[string]string{
		dao.DefaultDefDir:                "dao.DefaultDefDir",
		dao.DefaultOutDir:                "dao.DefaultOutDir",
		eventgen.DefaultDefDir:           "eventgen.DefaultDefDir",
		eventgen.DefaultOutDir:           "eventgen.DefaultOutDir",
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
