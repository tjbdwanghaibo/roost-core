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
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/configdata"
)

func load(t *testing.T) (*configdata.Store, *configdata.Snapshot, string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"drop.json", "monster.json", "world.json", "spawn.json", "item.json"} {
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
	if !ok || world.Width != 1024 || world.Height != 768 || world.Mode != "pve" {
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

// B10: required no longer needs a ref (the key must be present and not
// null), and min is enforced on reload — by the same configdata rule check
// tablegen's tags get.
func TestFieldRulesAreEnforcedOnReload(t *testing.T) {
	for body, want := range map[string]string{
		`[{"id":1,"scene_id":7,"drop_id":100,"range":5}]`:               "field name: required",
		`[{"id":1,"name":"wolf","scene_id":7,"drop_id":100,"range":0}]`: "field range: min",
	} {
		store, snapshot, dir := load(t)
		if err := os.WriteFile(filepath.Join(dir, "monster.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Reload(context.Background()); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("reload of %s: err = %v, want %q", body, err, want)
		}
		if store.Current() != snapshot {
			t.Fatal("a rejected reload moved the live snapshot")
		}
	}
}

// Round 12: a global's required / min / enum are enforced like a table's — a
// server refuses to start on them and a reload refuses them without moving
// the live snapshot. Before, cfggen rejected rules on globals and a world of
// width 0 loaded fine.
func TestGlobalRulesAreEnforcedOnLoadAndReload(t *testing.T) {
	for body, want := range map[string]string{
		`{"height":768,"mode":"pve"}`:              "field width: required",
		`{"width":0,"height":768,"mode":"pve"}`:    "field width: min",
		`{"width":1024,"height":0,"mode":"pve"}`:   "field height: min",
		`{"width":1024,"height":768,"mode":"pvz"}`: "field mode: enum",
	} {
		// Startup: Load fails on the bad file.
		dir := t.TempDir()
		for _, name := range []string{"drop.json", "monster.json", "spawn.json", "item.json"} {
			raw, err := os.ReadFile(filepath.Join("data", name))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "world.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		registry := configdata.NewRegistry()
		MustRegisterGeneratedConfigData(registry)
		if _, err := configdata.NewStore(registry, dir).Load(context.Background()); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("load of world %s: err = %v, want %q", body, err, want)
		}

		// Reload: refused, the live snapshot stays.
		store, snapshot, liveDir := load(t)
		if err := os.WriteFile(filepath.Join(liveDir, "world.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Reload(context.Background()); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("reload of world %s: err = %v, want %q", body, err, want)
		}
		if store.Current() != snapshot {
			t.Fatal("a rejected reload moved the live snapshot")
		}
		if world, ok := WorldFrom(store.Current()); !ok || world.Width != 1024 || world.Mode != "pve" {
			t.Fatalf("live world = %+v (ok=%v)", world, ok)
		}
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

func spawnIDs(rows []SpawnCfg) []int32 {
	out := make([]int32, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.ID)
	}
	return out
}

func sameIDs(got []int32, want ...int32) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// The index options round-trip through real JSON: skipempty keeps zero values
// (0, false) out while a string index without it keeps "" rows in file order;
// an explicit index name is what the accessor queries; uint64 at its maximum,
// negative int64 and bool stringify the way the runtime index does. spawn is
// declared before item, so its string-keyed ref points forward.
func TestIndexOptionsAndStringRefsRoundTrip(t *testing.T) {
	store, snapshot, dir := load(t)
	if got := spawnIDs(SpawnByZoneID(snapshot, math.MaxUint64)); !sameIDs(got, 1) {
		t.Fatalf("zone MaxUint64 = %v, want [1]", got)
	}
	if got := SpawnByZoneID(snapshot, 0); len(got) != 0 {
		t.Fatalf("skipempty indexed zone 0: %v", spawnIDs(got))
	}
	if got := spawnIDs(SpawnByElite(snapshot, true)); !sameIDs(got, 1) {
		t.Fatalf("elite=true = %v, want [1]", got)
	}
	if got := SpawnByElite(snapshot, false); len(got) != 0 {
		t.Fatalf("skipempty indexed elite=false: %v", spawnIDs(got))
	}
	if got := spawnIDs(SpawnByTag(snapshot, "")); !sameIDs(got, 1, 3) {
		t.Fatalf("tag \"\" = %v, want [1 3] (no skipempty keeps empty values)", got)
	}
	if got := ItemByRarity(snapshot, false); len(got) != 1 || got[0].Code != "rock" {
		t.Fatalf("explicit index rarity=false = %+v", got)
	}
	if got := ItemByLevel(snapshot, -5); len(got) != 1 || got[0].Code != "sword" {
		t.Fatalf("level -5 = %+v", got)
	}
	if got := ItemByLevel(snapshot, 0); len(got) != 1 || got[0].Code != "rock" {
		t.Fatalf("level 0 without skipempty = %+v", got)
	}

	// A dangling string ref and a removed target key are refused; the live
	// snapshot and its indexes stay as they were.
	for name, rewrite := range map[string][2]string{
		"dangling string ref": {"spawn.json", `[{"id":1,"item_code":"axe"}]`},
		"target key removed":  {"item.json", `[{"code":"rock"}]`},
	} {
		if err := os.WriteFile(filepath.Join(dir, rewrite[0]), []byte(rewrite[1]), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Reload(context.Background()); err == nil {
			t.Fatalf("%s: reload accepted it", name)
		}
		if got := spawnIDs(SpawnByZoneID(store.Current(), math.MaxUint64)); !sameIDs(got, 1) || store.Current().Version != snapshot.Version {
			t.Fatalf("%s: the refused reload moved the live snapshot (zone index %v)", name, got)
		}
		raw, err := os.ReadFile(filepath.Join("data", rewrite[0]))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rewrite[0]), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// A good reload rebuilds the indexes; the pinned snapshot keeps its own.
	if err := os.WriteFile(filepath.Join(dir, "spawn.json"), []byte(`[{"id":9,"zone_id":5,"item_code":"rock","tag":"x"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	next, err := store.Reload(context.Background())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := spawnIDs(SpawnByZoneID(next, 5)); !sameIDs(got, 9) {
		t.Fatalf("new zone 5 = %v", got)
	}
	if got := SpawnByZoneID(next, math.MaxUint64); len(got) != 0 {
		t.Fatalf("stale index entry survived the reload: %v", spawnIDs(got))
	}
	if got := spawnIDs(SpawnByZoneID(snapshot, math.MaxUint64)); !sameIDs(got, 1) {
		t.Fatalf("the pinned snapshot's index changed: %v", got)
	}
}
