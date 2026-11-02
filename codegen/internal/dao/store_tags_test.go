package dao

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreTagIsRequiredAndBSONIsNotAnAlias(t *testing.T) {
	for _, tc := range []struct{ name, field, want string }{
		{"missing", "Value int64", "missing required store tag"},
		{"shared-declaration", "A, B int64 `store:\"1\"`", "one named field per declaration"},
		{"empty", "Value int64 `store:\"\"`", "missing required store tag"},
		{"bson", "Value int64 `bson:\"1\"`", "use store:"},
		{"both", "Value int64 `store:\"1\" bson:\"2\"`", "use store:"},
		{"transient", "Value int64 `dao:\"nopersist,nosync\"`", "missing required store tag"},
		{"nested", "Value Item `store:\"1\"` }\n\ntype Item struct { Value int64", "Item.Value: missing required store tag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, out := t.TempDir(), t.TempDir()
			writeDef(t, dir, "package def\n//roost:dao coll=players db=game\ntype PlayerDao struct {"+tc.field+"}\n")
			err := Run([]string{"-def", dir, "-out", out, "-pkg", "db"}, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v; want %s", err, tc.want)
			}
			files, err := os.ReadDir(out)
			if err != nil || len(files) != 0 {
				t.Fatalf("invalid definitions wrote partial output: %v %v", files, err)
			}
		})
	}
}

func TestStoreTagChangeWarnsForSwapsOptionsAndTransientFields(t *testing.T) {
	for _, tc := range []struct{ name, marker, before, after, want string }{
		{"swap", "coll=players db=game", "A int64 `store:\"1\"`; B int64 `store:\"2\"`", "A int64 `store:\"2\"`; B int64 `store:\"1\"`", `PlayerDao.A: store tag changed "1" -> "2"`},
		{"options", "coll=players db=game", "A int64 `store:\"1\"`", "A int64 `store:\"1,omitempty\"`", `store tag changed "1" -> "1,omitempty"`},
		{"nocoll", "nocoll", "A int64 `store:\"1\" dao:\"nopersist,sync\"`", "A int64 `store:\"2\" dao:\"nopersist,sync\"`", `store tag changed "1" -> "2"`},
		{"transient", "coll=players db=game", "A int64 `store:\"1\" dao:\"nopersist,nosync\"`", "A int64 `store:\"2\" dao:\"nopersist,nosync\"`", `store tag changed "1" -> "2"`},
		{"nested", "coll=players db=game", "A Item `store:\"1\"` }\n\ntype Item struct { X int64 `store:\"1\"`", "A Item `store:\"1\"` }\n\ntype Item struct { X int64 `store:\"2\"`", `Item.X: store tag changed "1" -> "2"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, out := t.TempDir(), t.TempDir()
			run := func(fields string) string {
				t.Helper()
				writeDef(t, dir, "package def\n//roost:dao "+tc.marker+"\ntype PlayerDao struct {"+fields+"}\n")
				var log bytes.Buffer
				if err := Run([]string{"-def", dir, "-out", out, "-pkg", "db", "-force"}, &log); err != nil {
					t.Fatal(err)
				}
				return log.String()
			}
			run(tc.before)
			if log := run(tc.after); !strings.Contains(log, tc.want) {
				t.Fatalf("missing %q: %s", tc.want, log)
			}
			if log := run(tc.after); strings.Contains(log, "WARN:") {
				t.Fatalf("unchanged tag warned: %s", log)
			}
			baseline, err := os.ReadFile(filepath.Join(out, "gen_player_dao.go"))
			if err != nil {
				t.Fatal(err)
			}
			writeDef(t, dir, "package def\n//roost:dao coll=players db=game\ntype PlayerDao struct { A int64 }\n")
			if err := Run([]string{"-def", dir, "-out", out, "-pkg", "db"}, io.Discard); err == nil {
				t.Fatal("missing tag accepted")
			}
			after, err := os.ReadFile(filepath.Join(out, "gen_player_dao.go"))
			if err != nil || !bytes.Equal(baseline, after) {
				t.Fatal("failed generation destroyed warning baseline")
			}
		})
	}
}
