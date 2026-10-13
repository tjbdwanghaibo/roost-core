package eventgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanFile(t *testing.T) {
	filePath, err := filepath.Abs("./testdata/game/player/player.go")
	if err != nil {
		t.Fatal(err)
	}

	handlers, err := scanFile(filePath, "github.com/tjbdwanghaibo/cube/event")
	if err != nil {
		t.Fatalf("scanFile: %v", err)
	}

	if len(handlers) != 1 {
		t.Fatalf("expected 1 handler, got %d", len(handlers))
	}

	h := handlers[0]
	if h.Receiver != "Player" {
		t.Fatalf("expected receiver 'Player', got %q", h.Receiver)
	}
	if h.Package != "player" {
		t.Fatalf("expected package 'player', got %q", h.Package)
	}
	if len(h.Events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(h.Events))
	}

	suffixes := make(map[string]bool)
	for _, ev := range h.Events {
		suffixes[ev.Suffix] = true
	}
	if !suffixes["PlayerOnLine"] {
		t.Error("missing DealEventPlayerOnLine")
	}
	if !suffixes["PlayerOffLine"] {
		t.Error("missing DealEventPlayerOffLine")
	}
}

func TestScanGameDir(t *testing.T) {
	gameDir, err := filepath.Abs("./testdata/game")
	if err != nil {
		t.Fatal(err)
	}

	outDir := t.TempDir()

	// Copy testdata to temp so we can generate into it
	playerDir := filepath.Join(outDir, "player")
	if err := os.MkdirAll(playerDir, 0755); err != nil {
		t.Fatal(err)
	}
	src, _ := os.ReadFile(filepath.Join(gameDir, "player", "player.go"))
	if err := os.WriteFile(filepath.Join(playerDir, "player.go"), src, 0644); err != nil {
		t.Fatal(err)
	}

	if err := scanGameDir(outDir, "github.com/tjbdwanghaibo/cube/event", true, declaredEvents("PlayerOnLine", "PlayerOffLine")); err != nil {
		t.Fatalf("scanGameDir: %v", err)
	}

	// Check generated file
	genFile := filepath.Join(playerDir, "player_event_gen.go")
	content, err := os.ReadFile(genFile)
	if err != nil {
		t.Fatalf("generated file not found: %v", err)
	}

	s := string(content)

	checks := []string{
		"package player",
		"func (p *Player) InitSub()",
		"p.SubEvent(event.EventTypePlayerOffLine)",
		"p.SubEvent(event.EventTypePlayerOnLine)",
		"func (p *Player) SyncHandleEvent(d frameworkEvent.EventData)",
		"case *event.EventPlayerOnLine:",
		"p.DealEventPlayerOnLine(d.(*event.EventPlayerOnLine))",
		"case *event.EventPlayerOffLine:",
		"p.DealEventPlayerOffLine(d.(*event.EventPlayerOffLine))",
	}
	for _, check := range checks {
		if !strings.Contains(s, check) {
			t.Errorf("generated handler missing: %q\n\nFull content:\n%s", check, s)
		}
	}
}

func TestRecvVar(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Player", "p"},
		{"Activity", "a"},
		{"DataMgr", "e"}, // 'd' conflicts with param
		{"", "r"},
	}
	for _, c := range cases {
		got := recvVar(c.in)
		if got != c.want {
			t.Errorf("recvVar(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

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

// 解析不了的源文件要带文件名与位置报错，不能跳过：跳过会让它声明的 handler 从生成的分发里消失，事件永远送不到。
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
