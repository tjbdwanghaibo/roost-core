package entity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// U-0091 (C2): remote=managed without *entity.RemoteEntityBase must be refused
// by generate itself, before any wire file is written — a managed entity that
// embeds the plain base would compile but never take part in remote commits.
func TestGenerateRefusesManagedRemoteWithoutRemoteBase(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("testdata", "player.go"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(source),
		"//roost:entity entityKind=EntityKindPlayer sync=true",
		"//roost:entity entityKind=EntityKindPlayer remote=managed sync=true", 1)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "player.go"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	entities, pkg, err := parseDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entities) != 1 || entities[0].RemoteBase {
		t.Fatalf("fixture must embed the plain base: %+v", entities)
	}
	out := filepath.Join(dir, "player_gen_wire.go")
	written, err := generate(entities[0], pkg, out, true)
	if err == nil || !strings.Contains(err.Error(), "remote=managed requires embedding *entity.RemoteEntityBase") {
		t.Fatalf("generate = %v, %v; want the RemoteEntityBase refusal", written, err)
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Fatal("refused generate still wrote the wire file")
	}
}

// A marker key given twice must be refused rather than last-one-wins.
func TestParseDirRejectsMarkerParameterGivenTwice(t *testing.T) {
	dir, _ := writeEntitySource(t, "package game\n\n//roost:entity entityKind=EntityKindPlayer sync=true sync=false\ntype Player struct{}\n")
	expectParseError(t, dir, `parameter "sync" given twice`)
}
