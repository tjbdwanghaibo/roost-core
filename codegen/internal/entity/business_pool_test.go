package entity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGeneratedBusinessPoolRegistration(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "player.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, pool := range []string{"short", "long", "invalid"} {
		t.Run(pool, func(t *testing.T) {
			dir := t.TempDir()
			source := strings.Replace(string(raw), "//roost:entity entityKind=EntityKindPlayer", "//roost:entity entityKind=EntityKindPlayer businessPool="+pool, 1)
			if err := os.WriteFile(filepath.Join(dir, "player.go"), []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			defs, pkg, err := parseDir(dir)
			if pool == "invalid" {
				if err == nil {
					t.Fatal("invalid pool accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(dir, "player_gen_wire.go")
			if _, err := generate(defs[0], pkg, out, true); err != nil {
				t.Fatal(err)
			}
			generated, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			want := "entity.BusinessPoolShort"
			if pool == "long" {
				want = "entity.BusinessPoolLong"
			}
			if !strings.Contains(string(generated), want) {
				t.Fatalf("generated registration omitted %s", want)
			}
		})
	}
}
