package nest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePromiseSource(t *testing.T, src string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "handler.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Three remote-tag rules and the one-receiver-per-file rule had no test of
// their own: a temporary revert of each left the suite green (U-0035). The
// fixture is the valid remote-tag handler with exactly one thing broken.
func TestParseFileRejectsRemainingRemoteTagViolations(t *testing.T) {
	const head = "package invalid\nimport \"github.com/tjbdwanghaibo/roost-core/entity\"\ntype IPlayerEntity interface{ ID() int64 }\n"
	cases := []struct{ label, tag, want string }{
		{"missing snapshot type", "cached", "missing snapshot type"},
		{"unknown k=v option", "mode=lazy,view.PlayerViewMapSnapshot", "unknown remote tag option \"mode\""},
		{"duplicate snapshot type", "view.PlayerViewMapSnapshot,view.OtherSnapshot", "duplicate remote snapshot type"},
	}
	for _, testCase := range cases {
		t.Run(testCase.label, func(t *testing.T) {
			src := head + "type Req struct { TargetPlayerViewRef entity.RemoteViewRef `remote:\"" + testCase.tag + "\"` }\n//roost:nest\nfunc handlerBad(p IPlayerEntity, req Req) {}\n"
			_, _, err := parseFile(writePromiseSource(t, src))
			if err == nil {
				t.Fatalf("accepted remote tag %q", testCase.tag)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error %q does not contain %q", err, testCase.want)
			}
		})
	}
}

// One source file generates one handler aggregate, so every marked method in
// it must share a receiver type; two receivers would generate a register
// function that binds half the handlers to the wrong object.
func TestOneSourceFileCannotMixHandlerReceivers(t *testing.T) {
	src := `package mixed
type IPlayerEntity interface{ ID() int64 }
type PlayerHandler struct{}
type GuildHandler struct{}
//roost:nest
func (h *PlayerHandler) handlerA(p IPlayerEntity, id int64) error { return nil }
//roost:nest
func (h *GuildHandler) handlerB(p IPlayerEntity, id int64) error { return nil }
`
	funcs, _, err := parseFile(writePromiseSource(t, src))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := commonReceiverType(funcs); err == nil || !strings.Contains(err.Error(), "cannot mix receiver") {
		t.Fatalf("two receivers in one file: err=%v", err)
	}
}
