//go:build cfgruntime

package cfg

// The cfggen runtime gate. roost-codegen has no runtime dependency, so its
// own tests can only compare the generated text — the same blind spot that
// let U-0224 (dao nested structs with no BSON representation) stay invisible
// for months: the text was exactly what the goldens said, and what it
// persisted was an empty document.
//
// This file is compiled by scripts/cfggen-golden-runtime.sh in a throwaway
// module together with the package cfggen just generated and a pinned
// roost-core, so it exercises the real configdata runtime: registration,
// loading JSON from disk, primary keys, secondary indexes, cross-table refs,
// bean slices, globals, and the reload path that must reject bad data
// without disturbing the live snapshot.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/configdata"
)

func load(t *testing.T) (*configdata.Store, *configdata.Snapshot, string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"drop.json", "monster.json", "world.json"} {
		raw, err := os.ReadFile(filepath.Join("data", name))
		if err != nil {
			t.Fatalf("read fixture %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	registry := configdata.NewRegistry()
	MustRegisterGeneratedConfigData(registry)
	store := configdata.NewStore(registry, dir)
	snapshot, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return store, snapshot, dir
}

// The generated accessors must return the data that is on disk: the table by
// primary key, the bean slice inside a row, the secondary index, and the
// global. A generator that emits compiling code with the wrong json tags or
// the wrong key type fails here and nowhere else.
func TestGeneratedAccessorsReadTheDataOnDisk(t *testing.T) {
	_, snapshot, _ := load(t)

	monsters, ok := MonsterTableFrom(snapshot)
	if !ok {
		t.Fatal("monster table is not in the snapshot")
	}
	wolf, ok := monsters.Get(1)
	if !ok || wolf.Name != "wolf" || wolf.SceneID != 7 || wolf.DropID != 100 {
		t.Fatalf("monster#1 = %+v (ok=%v)", wolf, ok)
	}
	// A Go keyword as a field name must survive as data, not just as syntax.
	if wolf.Range != 5 {
		t.Fatalf("monster#1 range = %d, want 5", wolf.Range)
	}
	drops, ok := DropTableFrom(snapshot)
	if !ok {
		t.Fatal("drop table is not in the snapshot")
	}
	group, ok := drops.Get(100)
	if !ok || len(group.Items) != 2 || group.Items[0].ItemID != 1001 || group.Items[0].Weight != 70 {
		t.Fatalf("drop#100 = %+v (ok=%v)", group, ok)
	}
	if empty, ok := drops.Get(200); !ok || len(empty.Items) != 0 {
		t.Fatalf("drop#200 = %+v (ok=%v)", empty, ok)
	}
	scene7 := MonsterBySceneID(snapshot, 7)
	if len(scene7) != 2 {
		t.Fatalf("scene 7 has %d monsters, want 2", len(scene7))
	}
	if scene9 := MonsterBySceneID(snapshot, 9); len(scene9) != 1 || scene9[0].Name != "crab" {
		t.Fatalf("scene 9 = %+v", scene9)
	}
	if missing := MonsterBySceneID(snapshot, 42); len(missing) != 0 {
		t.Fatalf("scene 42 = %+v, want none", missing)
	}
	world, ok := WorldFrom(snapshot)
	if !ok || world.Width != 1024 || world.Height != 768 {
		t.Fatalf("world = %+v (ok=%v)", world, ok)
	}
	if snapshot.Hash == "" || snapshot.Version == 0 {
		t.Fatalf("snapshot has no identity: version=%d hash=%q", snapshot.Version, snapshot.Hash)
	}
}

// ref is a promise the generator makes to the runtime: a row pointing at a
// key no table holds must be refused. If the generated registration does not
// carry the ref, this passes silently in every text comparison and lets bad
// configuration reach players.
func TestDanglingRefIsRejectedAndTheLiveSnapshotSurvives(t *testing.T) {
	store, snapshot, dir := load(t)
	before := snapshot.Version

	if err := os.WriteFile(filepath.Join(dir, "monster.json"), []byte(`[{"id":1,"name":"wolf","scene_id":7,"drop_id":999,"range":5}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Reload(context.Background()); err == nil {
		t.Fatal("reload accepted a monster whose drop_id points at no drop group")
	}
	current := store.Current()
	if current.Version != before {
		t.Fatalf("a refused reload moved the live snapshot: %d → %d", before, current.Version)
	}
	monsters, ok := MonsterTableFrom(current)
	if !ok {
		t.Fatal("monster table vanished from the live snapshot")
	}
	if wolf, ok := monsters.Get(1); !ok || wolf.DropID != 100 {
		t.Fatalf("live snapshot changed: %+v (ok=%v)", wolf, ok)
	}
}

// required is the other promise: the field must be present and non-zero.
func TestRequiredFieldIsEnforcedByTheRuntime(t *testing.T) {
	store, _, dir := load(t)
	if err := os.WriteFile(filepath.Join(dir, "monster.json"), []byte(`[{"id":1,"name":"wolf","scene_id":7,"range":5}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Reload(context.Background()); err == nil {
		t.Fatal("reload accepted a monster with no drop_id although the field is required")
	}
}

// A good reload must move the snapshot: new content, new hash, new version.
func TestReloadPublishesNewContent(t *testing.T) {
	store, snapshot, dir := load(t)
	if err := os.WriteFile(filepath.Join(dir, "monster.json"), []byte(`[{"id":1,"name":"dire wolf","scene_id":7,"drop_id":100,"range":9}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	reloaded, err := store.Reload(context.Background())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Version <= snapshot.Version || reloaded.Hash == snapshot.Hash {
		t.Fatalf("reload did not publish: version %d → %d, hash %q → %q", snapshot.Version, reloaded.Version, snapshot.Hash, reloaded.Hash)
	}
	monsters, _ := MonsterTableFrom(reloaded)
	renamed, ok := monsters.Get(1)
	if !ok || renamed.Name != "dire wolf" || renamed.Range != 9 {
		t.Fatalf("reloaded monster#1 = %+v (ok=%v)", renamed, ok)
	}
	// The old snapshot is immutable: a request that pinned it still reads
	// the old row.
	old, _ := MonsterTableFrom(snapshot)
	if wolf, ok := old.Get(1); !ok || wolf.Name != "wolf" {
		t.Fatalf("the pinned snapshot changed under the reader: %+v", wolf)
	}
}
