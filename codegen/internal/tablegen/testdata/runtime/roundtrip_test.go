//go:build tablegenruntime

package generated

// The tablegen runtime gate (codegen/scripts/tablegen-runtime.sh): the
// generated loader against the real configdata runtime. RR-20261005-NC-75:
// `ref:"<table>"` was printed into the CSV rule row and enforced by nothing —
// validateRows left it to "the generated loader", which never checked it, so a
// monster pointing at a scene no table holds loaded and reloaded fine.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/configdata"
)

func load(t *testing.T) (*configdata.Store, *configdata.Snapshot, string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"scene.json", "monster.json"} {
		raw, err := os.ReadFile(filepath.Join("data", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	registry := configdata.NewRegistry()
	if err := RegisterGeneratedConfigData(registry); err != nil {
		t.Fatal(err)
	}
	store := configdata.NewStore(registry, dir)
	snapshot, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return store, snapshot, dir
}

func TestGeneratedLoaderReadsTheTables(t *testing.T) {
	_, snapshot, _ := load(t)
	monsters, ok := MonsterTableFrom(snapshot)
	if !ok {
		t.Fatal("monster table missing")
	}
	if m, ok := monsters.Get(11); !ok || m.SceneID != 2 || m.Level != 5 {
		t.Fatalf("monster 11 = %+v ok=%v", m, ok)
	}
	// scene_id 0 means "no scene": a ref only constrains non-zero values.
	if m, ok := monsters.Get(12); !ok || m.SceneID != 0 {
		t.Fatalf("monster 12 = %+v ok=%v", m, ok)
	}
}

func TestDanglingRefIsRejectedOnLoadAndReload(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"monster points at no scene":        {"monster.json": `[{"id":10,"scene_id":9,"level":3}]`},
		"the scene it points at is removed": {"scene.json": `[{"id":2,"name":"cave"}]`},
	} {
		t.Run(name, func(t *testing.T) {
			store, snapshot, dir := load(t)
			for file, body := range files {
				if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			_, err := store.Reload(context.Background())
			if err == nil || !strings.Contains(err.Error(), "scene") {
				t.Fatalf("reload with a dangling scene_id = %v, want a reference error", err)
			}
			if store.Current().Version != snapshot.Version {
				t.Fatal("a refused reload moved the live snapshot")
			}
			registry := configdata.NewRegistry()
			if err := RegisterGeneratedConfigData(registry); err != nil {
				t.Fatal(err)
			}
			if _, err := configdata.NewStore(registry, dir).Load(context.Background()); err == nil {
				t.Fatal("a cold load accepted the dangling scene_id")
			}
		})
	}
}
