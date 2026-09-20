package protocol

import (
	"flag"
	"path/filepath"
	"testing"

	"github.com/tjbdwanghaibo/roost-codegen/internal/genutil"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

// TestGoldenProtocolOutput locks the complete .proto and pb.go outputs for
// the shared test definition; see genutil.AssertGolden.
func TestGoldenProtocolOutput(t *testing.T) {
	root := t.TempDir()
	defDir := filepath.Join(root, "protocol", "def")
	writeProtocolTestFile(t, filepath.Join(defDir, "game.go"), testProtocolDef)
	defs, err := parseDefDir(defDir)
	if err != nil {
		t.Fatal(err)
	}
	proto, err := generateProto(defs)
	if err != nil {
		t.Fatal(err)
	}
	genutil.AssertGolden(t, filepath.Join("testdata", "golden", "game.proto"), proto, *updateGolden)
	pb, err := generatePBGo(defs)
	if err != nil {
		t.Fatal(err)
	}
	genutil.AssertGolden(t, filepath.Join("testdata", "golden", "game_pb.go.golden"), pb, *updateGolden)
}
