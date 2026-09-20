package entity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeEntitySource(t *testing.T, content string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "player.go")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

func expectParseError(t *testing.T, dir string, wants ...string) {
	t.Helper()
	entities, _, err := parseDir(dir)
	if err == nil {
		t.Fatalf("parseDir accepted the source and produced %d entities; want an error mentioning %q", len(entities), wants)
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error must mention %q; got %v", want, err)
		}
	}
}

// A marker that no struct follows used to vanish: no wire file, no error.
// The usual way to get there is a doc comment between the marker and the
// type, or a marker placed above an interface. The generator knows the
// position; it must report it.
func TestParseDirRejectsMarkerNotAttachedToStruct(t *testing.T) {
	dir, path := writeEntitySource(t, "package game\n\n//roost:entity entityKind=EntityKindPlayer\n// Player is the aggregate.\n// It has a long doc comment.\n// Long enough to push the struct away.\ntype Player struct{}\n")
	expectParseError(t, dir, filepath.Base(path)+":3", "not followed by a struct")
}

func TestParseDirRejectsMarkerAboveNonStruct(t *testing.T) {
	dir, path := writeEntitySource(t, "package game\n\n//roost:entity entityKind=EntityKindPlayer\ntype Player interface{}\n")
	expectParseError(t, dir, filepath.Base(path)+":3", "not followed by a struct")
}

// Typos in parameter names were the same as omitting them: `remot=managed`
// produced a local entity, `noPersist` without `=true` produced a persisted
// one. Unknown keys and bare tokens are errors that list the known keys.
func TestParseDirRejectsUnknownMarkerParameters(t *testing.T) {
	dir, _ := writeEntitySource(t, "package game\n\n//roost:entity entityKind=EntityKindPlayer remot=managed\ntype Player struct{}\n")
	expectParseError(t, dir, "remot", "unknown parameter", "remote")

	dir, _ = writeEntitySource(t, "package game\n\n//roost:entity entityKind=EntityKindPlayer noPersist\ntype Player struct{}\n")
	expectParseError(t, dir, "noPersist", "key=value")
}

// Values outside the accepted spellings fell back to defaults: remote=bogus
// generated a local entity, lifetime=forever a persisted one, sync=ture a
// non-synced one. Each is an error naming the parameter and the value.
func TestParseDirRejectsInvalidMarkerValues(t *testing.T) {
	cases := []struct{ marker, want string }{
		{"//roost:entity entityKind=EntityKindPlayer remote=bogus", `remote="bogus"`},
		{"//roost:entity entityKind=EntityKindPlayer lifetime=forever", `lifetime="forever"`},
		{"//roost:entity entityKind=EntityKindPlayer sync=ture", `sync="ture"`},
		{"//roost:entity entityKind=EntityKindPlayer noPersist=maybe", `noPersist="maybe"`},
	}
	for _, tc := range cases {
		dir, _ := writeEntitySource(t, "package game\n\n"+tc.marker+"\ntype Player struct{}\n")
		expectParseError(t, dir, tc.want)
	}
}

// Every spelling the parsers accept today stays accepted, including the id=
// parameter written by `roost add entity`.
func TestParseDirAcceptsEveryDocumentedMarkerForm(t *testing.T) {
	for _, marker := range []string{
		"//roost:entity id=1 entityKind=EntityKindPlayer",
		"//roost:entity entityKind=EntityKindPlayer remote=managed sync=true",
		"//roost:entity entityKind=EntityKindPlayer remote=mirror lifetime=mirror-cache",
		"//roost:entity entityKind=EntityKindPlayer noPersist=true lifetime=ephemeral",
		// The two packer spellings are alternatives, never both at once: they
		// name one field (RR-20260918-01).
		"//roost:entity entityKind=EntityKindPlayer sync=on syncTopic=\"player\" subjectPacker=clientsync.PlayerSubjectPacker",
		"//roost:entity entityKind=EntityKindPlayer sync=on syncTopic=\"player\" syncPacker=clientsync.PlayerPacker",
		"//roost:entity entityKind=EntityKindPlayer remote=no lifetime=hot_cold",
	} {
		dir, _ := writeEntitySource(t, "package game\n\n"+marker+"\ntype Player struct{}\n")
		entities, _, err := parseDir(dir)
		if err != nil {
			t.Fatalf("%s rejected: %v", marker, err)
		}
		if len(entities) != 1 || entities[0].Name != "Player" {
			t.Fatalf("%s produced %+v", marker, entities)
		}
	}
}
