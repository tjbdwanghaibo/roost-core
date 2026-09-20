package eventgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeGameFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// A source file the parser cannot read used to be skipped without a word.
// The handlers it declares then vanish from the generated dispatch, and the
// only symptom is an event that is never delivered. The generator has the
// file name and the position; it must say so.
func TestScanGameDirRefusesUnparseableSource(t *testing.T) {
	root := t.TempDir()
	playerDir := filepath.Join(root, "player")
	writeGameFile(t, playerDir, "player.go", "package player\n\ntype Player struct{}\n\nfunc (p *Player) DealEventPlayerOnLine(e *event.EventPlayerOnLine) {}\n")
	broken := writeGameFile(t, playerDir, "broken.go", "package player\n\nfunc (p *Player) DealEventPlayerOffLine(e *event.EventPlayerOffLine) {\n")

	err := scanGameDirTo(root, "example.com/game/event", true, declaredEvents("PlayerOnLine", "PlayerOffLine"), nil)
	if err == nil {
		t.Fatal("a syntax error in a game source file must fail generation, not drop the file")
	}
	if !strings.Contains(err.Error(), filepath.Base(broken)) {
		t.Fatalf("error must name the unparseable file; got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(playerDir, "player_event_gen.go")); statErr == nil {
		t.Fatal("no dispatch file may be written when a source file could not be read")
	}
}

// DealEventGhost with no EventGhost declared compiled into
// `case *event.EventGhost:` — a build error in generated code, pointing at a
// file the user did not write. The generator already parsed the declarations
// in phase 1; the handler scan must check against them and point at the
// method instead.
func TestScanGameDirRejectsHandlerForUndeclaredEvent(t *testing.T) {
	root := t.TempDir()
	playerDir := filepath.Join(root, "player")
	source := writeGameFile(t, playerDir, "player.go", "package player\n\ntype Player struct{}\n\nfunc (p *Player) DealEventPlayerOnLine(e *event.EventPlayerOnLine) {}\n\nfunc (p *Player) DealEventGhost(e *event.EventGhost) {}\n")

	err := scanGameDirTo(root, "example.com/game/event", true, declaredEvents("PlayerOnLine"), nil)
	if err == nil {
		t.Fatal("a handler for an undeclared event must fail generation")
	}
	for _, want := range []string{"DealEventGhost", "EventGhost", filepath.Base(source), "Player"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error must mention %q; got %v", want, err)
		}
	}
	if _, statErr := os.Stat(filepath.Join(playerDir, "player_event_gen.go")); statErr == nil {
		t.Fatal("no dispatch file may be written when a handler references an undeclared event")
	}
}

// The check is on the declaration set, not on the name shape: a handler whose
// event is declared is still accepted in a directory that also has unrelated
// methods on the receiver.
func TestScanGameDirAcceptsDeclaredHandlers(t *testing.T) {
	root := t.TempDir()
	playerDir := filepath.Join(root, "player")
	writeGameFile(t, playerDir, "player.go", "package player\n\ntype Player struct{}\n\nfunc (p *Player) DealEventPlayerOnLine(e *event.EventPlayerOnLine) {}\n\nfunc (p *Player) Deal() {}\n")
	if err := scanGameDirTo(root, "example.com/game/event", true, declaredEvents("PlayerOnLine"), nil); err != nil {
		t.Fatalf("declared handler rejected: %v", err)
	}
	if _, err := os.Stat(filepath.Join(playerDir, "player_event_gen.go")); err != nil {
		t.Fatalf("dispatch file not written: %v", err)
	}
}
