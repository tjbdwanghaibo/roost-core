package nest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// F02-4 / F03-9：正式标记应能选择运行期已支持的 pipelined，不得拒绝或降级为 async。
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
