package eventgen

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestRunRetiresEventTypesAndHandlerAfterLastDefinition(t *testing.T) {
	root := t.TempDir()
	defs, out, game := filepath.Join(root, "def"), filepath.Join(root, "event"), filepath.Join(root, "game")
	for _, dir := range []string{defs, out, game} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	defFile := filepath.Join(defs, "events.go")
	if err := os.WriteFile(defFile, []byte("package def\ntype EventPing struct{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := filepath.Join(game, "player.go")
	if err := os.WriteFile(handler, []byte("package game\ntype Player struct{}\nfunc (p *Player) DealEventPing(e *event.EventPing) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"-def", defs, "-out", out, "-game", game, "-eventpkg", "example.com/game/event"}
	if err := run(args, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(defFile, []byte("package def\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(args, io.Discard); err == nil {
		t.Fatal("undeclared handler must block retirement")
	}
	for _, name := range []string{"event_def_gen.go", "event_type_gen.go", "event_type_impl_gen.go"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Fatalf("retired on validation failure: %s: %v", name, err)
		}
	}
	if err := os.WriteFile(handler, []byte("package game\ntype Player struct{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(args, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(out, "event_def_gen.go"), filepath.Join(out, "event_type_gen.go"), filepath.Join(out, "event_type_impl_gen.go"), filepath.Join(game, "player_event_gen.go")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("retired output remains %s: %v", path, err)
		}
	}
}

func TestScanGameDirRetiresOnlyRemovedReceiver(t *testing.T) {
	game := t.TempDir()
	source := filepath.Join(game, "handlers.go")
	if err := os.WriteFile(source, []byte("package game\ntype Player struct{}\ntype World struct{}\nfunc (p *Player) DealEventPing(e *event.EventPing) {}\nfunc (w *World) DealEventPing(e *event.EventPing) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := scanGameDirTo(game, "example.com/game/event", false, declaredEvents("Ping"), io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("package game\ntype Player struct{}\ntype World struct{}\nfunc (p *Player) DealEventPing(e *event.EventPing) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := scanGameDirTo(game, "example.com/game/event", false, declaredEvents("Ping"), io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(game, "world_event_gen.go")); !os.IsNotExist(err) {
		t.Fatalf("removed receiver remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(game, "player_event_gen.go")); err != nil {
		t.Fatalf("active receiver removed: %v", err)
	}
}
