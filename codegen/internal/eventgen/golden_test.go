package eventgen

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/tjbdwanghaibo/roost-codegen/internal/genutil"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

// TestGoldenHandlerOutput locks the complete generated event-handler file for
// the shared testdata game module; see genutil.AssertGolden.
func TestGoldenHandlerOutput(t *testing.T) {
	gameDir, err := filepath.Abs("./testdata/game")
	if err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	playerDir := filepath.Join(outDir, "player")
	if err := os.MkdirAll(playerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join(gameDir, "player", "player.go"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(playerDir, "player.go"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := scanGameDir(outDir, "github.com/tjbdwanghaibo/cube/event", true, declaredEvents("PlayerOnLine", "PlayerOffLine")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(playerDir, "player_event_gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	genutil.AssertGolden(t, filepath.Join("testdata", "golden", "player_event_gen.go.golden"), got, *updateGolden)
}
