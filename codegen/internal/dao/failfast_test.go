package dao

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parseSource writes one definition file into a temp dir and parses it.
func parseSource(t *testing.T, source string) (*Definitions, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "def.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	return parseDefDir(dir)
}

func TestParseRejectsOrphanMarker(t *testing.T) {
	// Regression: a marker separated from its struct used to be dropped
	// silently — a DAO that silently does not persist.
	_, err := parseSource(t, `package def

//roost:dao coll=heroes db=game

var unrelated = 1

type HeroDao struct {
	Name string
}
`)
	if err == nil || !strings.Contains(err.Error(), "not attached to a struct") {
		t.Fatalf("err=%v, want orphan marker error", err)
	}
}

func TestParseRejectsMarkerBindingMultipleStructs(t *testing.T) {
	// Regression: one marker above a grouped type declaration used to bind
	// silently to every struct in the group — several types writing the same
	// collection with zero warning.
	_, err := parseSource(t, `package def

//roost:dao coll=heroes db=game
type (
	AlphaDao struct{ Name string }
	BetaDao  struct{ Name string }
)
`)
	if err == nil || !strings.Contains(err.Error(), "binds to multiple structs") {
		t.Fatalf("err=%v, want multi-bind error", err)
	}
}

func TestRunSweepsOrphansWhenAllDefinitionsRemoved(t *testing.T) {
	// Regression: deleting the last definition used to return early before
	// the orphan sweep, leaving the final generated files behind forever —
	// and generate --check could not see them either.
	defDir := t.TempDir()
	outDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(defDir, "def.go"), []byte(`package def

//roost:dao coll=heroes db=game
type HeroDao struct {
	Name string
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"-def", defDir, "-out", outDir, "-pkg", "out"}, os.Stdout); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(defDir, "def.go"), []byte("package def\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"-def", defDir, "-out", outDir, "-pkg", "out"}, os.Stdout); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "gen_hero_dao.go")); !os.IsNotExist(err) {
		t.Fatalf("last generated file must be swept when all definitions are gone: %v", err)
	}
}

func TestParseAcceptsMarkerAboveDocComment(t *testing.T) {
	// Doc comments between marker and struct are ordinary Go practice and
	// must not unbind the marker.
	defs, err := parseSource(t, `package def

//roost:dao coll=heroes db=game
// HeroDao is the persistent hero aggregate.
// It spans several doc lines to prove the distance rule
// no longer breaks on documentation.
type HeroDao struct {
	Name string
}
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs.Daos) != 1 || defs.Daos[0].Name != "HeroDao" {
		t.Fatalf("daos=%+v", defs.Daos)
	}
}

func TestParseRejectsMarkerWithoutParameters(t *testing.T) {
	_, err := parseSource(t, `package def

//roost:dao
type HeroDao struct {
	Name string
}
`)
	if err == nil || !strings.Contains(err.Error(), "missing parameters") {
		t.Fatalf("err=%v, want missing parameters error", err)
	}
}

func TestParseRejectsMarkerWithoutRequiredParams(t *testing.T) {
	_, err := parseSource(t, `package def

//roost:dao coll=heroes
type HeroDao struct {
	Name string
}
`)
	if err == nil || !strings.Contains(err.Error(), "requires coll= and db=") {
		t.Fatalf("err=%v, want required-params error", err)
	}
}

func TestParseRejectsDaoTagTraps(t *testing.T) {
	// Regression set: these used to be silent data-loss traps — an unrelated
	// option zeroed persist/sync, unknown options and map modes were ignored.
	cases := []struct {
		tag  string
		want string
	}{
		{"`dao:\"map=fast\"`", "does not state persist/sync intent"},
		{"`dao:\"persits,sync\"`", "unknown option"},
		{"`dao:\"persist,sync,map=huge\"`", "unknown map mode"},
		{"`dao:\"persist,nopersist\"`", "both persist and nopersist"},
		{"`dao:\"\"`", "does not state persist/sync intent"},
	}
	for _, tc := range cases {
		_, err := parseSource(t, `package def

//roost:dao coll=heroes db=game
type HeroDao struct {
	Items map[int64]int32 `+tc.tag+`
}
`)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("tag %s: err=%v, want %q", tc.tag, err, tc.want)
		}
	}
}

func TestParseAcceptsExplicitTagIntents(t *testing.T) {
	defs, err := parseSource(t, `package def

//roost:dao coll=heroes db=game
type HeroDao struct {
	A int64            `+"`dao:\"persist\"`"+`
	B int64            `+"`dao:\"nopersist,sync\"`"+`
	C map[int64]int32  `+"`dao:\"persist,sync,map=fast\"`"+`
	D int64            `+"`dao:\"-\"`"+`
}
`)
	if err != nil {
		t.Fatal(err)
	}
	fields := defs.Daos[0].Fields
	if len(fields) != 3 {
		t.Fatalf("fields=%+v", fields)
	}
	if !fields[0].Tag.Persist || fields[0].Tag.Sync {
		t.Fatalf("A tag=%+v, want persist only", fields[0].Tag)
	}
	if fields[1].Tag.Persist || !fields[1].Tag.Sync {
		t.Fatalf("B tag=%+v, want sync only", fields[1].Tag)
	}
	if !fields[2].Tag.Persist || !fields[2].Tag.Sync || fields[2].Tag.Map != "fast" {
		t.Fatalf("C tag=%+v", fields[2].Tag)
	}
}

func TestRunRemovesOrphansAndCollapsesDaoSuffix(t *testing.T) {
	defDir := t.TempDir()
	outDir := t.TempDir()
	write := func(dir, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(defDir, "def.go", `package def

//roost:dao coll=heroes db=game
type HeroDao struct {
	Name string
}
`)
	// A stale generated file from a renamed definition, and a hand-written
	// file that happens to match the pattern but lacks the header.
	write(outDir, "gen_player_dao_dao.go", generatedHeader+"\npackage out\n")
	write(outDir, "gen_manual_dao.go", "package out\n\n// hand-written, must survive\n")

	if err := Run([]string{"-def", defDir, "-out", outDir, "-pkg", "out"}, os.Stdout); err != nil {
		t.Fatal(err)
	}

	// Regression: HeroDao used to produce gen_hero_dao_dao.go.
	if _, err := os.Stat(filepath.Join(outDir, "gen_hero_dao.go")); err != nil {
		t.Fatalf("expected gen_hero_dao.go: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "gen_player_dao_dao.go")); !os.IsNotExist(err) {
		t.Fatalf("orphan generated file was not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "gen_manual_dao.go")); err != nil {
		t.Fatalf("hand-written file must survive orphan cleanup: %v", err)
	}

	// Rename the DAO: the old generated file becomes an orphan and is
	// replaced in the same run.
	write(defDir, "def.go", `package def

//roost:dao coll=heroes db=game
type ChampionDao struct {
	Name string
}
`)
	if err := Run([]string{"-def", defDir, "-out", outDir, "-pkg", "out"}, os.Stdout); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "gen_champion_dao.go")); err != nil {
		t.Fatalf("expected gen_champion_dao.go: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "gen_hero_dao.go")); !os.IsNotExist(err) {
		t.Fatalf("renamed definition left an orphan behind: %v", err)
	}
}

func TestParseRejectsUnsupportedFieldTypes(t *testing.T) {
	// Regression: these shapes used to fall through as KindBasic and emit
	// code that only failed when the downstream project compiled it.
	cases := []struct {
		field string
		want  string
	}{
		{"Arr [3]int32", "fixed-size arrays"},
		{"Ptr *map[int64]int32", "pointers to named local types"},
		{"Ch chan int", "unsupported type"},
	}
	for _, tc := range cases {
		_, err := parseSource(t, `package def

//roost:dao coll=heroes db=game
type HeroDao struct {
	`+tc.field+`
}
`)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("field %q: err=%v, want %q", tc.field, err, tc.want)
		}
	}
}
