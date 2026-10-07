package servicerpc

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnsupportedWireShapesAreRejectedAtTheNamedField(t *testing.T) {
	for _, typ := range []string{"[4]byte", "interface{ Read() }", "*[]time.Duration", "**time.Duration", "[][]time.Time"} {
		t.Run(typ, func(t *testing.T) {
			src := strings.Replace(goodService, "req SendRequest", "req "+typ, 1)
			if _, err := ParseDir(writeDir(t, src)); err == nil || !strings.Contains(err.Error(), "field req") {
				t.Fatalf("unsupported %s escaped field validation: %v", typ, err)
			}
		})
	}
}
func TestReliableMarkerCannotSilentlyPromiseAProtocol(t *testing.T) {
	src := strings.Replace(goodService, "// Send stores an envelope.", "//roost:rpc reliable", 1)
	if _, err := ParseDir(writeDir(t, src)); err == nil || !strings.Contains(err.Error(), "nats.rpc.transport") {
		t.Fatalf("unsupported per-method reliable marker accepted: %v", err)
	}
}
func TestDerivedAffinityRejectsExpressionsInsteadOfMethodNames(t *testing.T) {
	for _, expr := range []string{"req.X.Y()", "req.Key(1)", "req.Key()()"} {
		src := strings.Replace(goodService, "// Send stores an envelope.", "//roost:rpc affinity="+expr, 1)
		if _, err := ParseDir(writeDir(t, src)); err == nil {
			t.Fatalf("invalid affinity accepted: %s", expr)
		}
	}
}
func TestTicketIDsWireNameKeepsPluralAcronymTogether(t *testing.T) {
	for name, want := range map[string]string{"TicketIDs": "ticket_ids", "URLs": "urls", "HTTPStatus": "http_status", "PlayerID": "player_id"} {
		if got := jsonName(name); got != want {
			t.Errorf("%s => %s, want %s", name, got, want)
		}
	}
}
func TestAssemblyCollisionIsCheckedWhereCodeWillBeWritten(t *testing.T) {
	src := writeDir(t, goodService+"\ntype Server struct{}\n")
	out := t.TempDir()
	if err := Run([]string{"-dir", src, "-out", out, "-emit", "assembly"}, io.Discard); err != nil {
		t.Fatalf("unrelated source Server blocked assembly: %v", err)
	}
	if err := os.WriteFile(filepath.Join(out, "manual.go"), []byte("package mail\ntype Server struct{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"-dir", src, "-out", out, "-emit", "assembly"}, io.Discard); err == nil || !strings.Contains(err.Error(), "Server") {
		t.Fatalf("output collision accepted: %v", err)
	}
}
