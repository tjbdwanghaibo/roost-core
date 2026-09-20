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
