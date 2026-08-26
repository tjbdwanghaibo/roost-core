package configdata

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	fctx "github.com/tjbdwanghaibo/cube-core/ctx"
)

func TestBeforeApplyPanicRollsBackOnlyPrepared(t *testing.T) {
	store, _ := newCommitTestStore(t)
	if _, err := store.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	var calls []string
	store.AddReloadListener(&recordingListener{name: "a", calls: &calls})
	store.AddReloadListener(&recordingListener{name: "b", calls: &calls, panicBefore: true})
	if _, err := store.Reload(context.Background()); err == nil || !strings.Contains(err.Error(), "panic: before boom") {
		t.Fatalf("panic swallowed: %v", err)
	}
	joined := strings.Join(calls, ",")
	if !strings.HasSuffix(joined, "b.before,a.rollback") {
		t.Fatalf("prepared-only rollback broken: %s", joined)
	}
}

func TestRevertRestoresAllGlobalSlots(t *testing.T) {
	store, _ := newCommitTestStore(t)
	first, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	prevDefault := DefaultStore()
	prevConfig := fctx.RuntimeConfig()
	var calls []string
	unregister := store.AddReloadListener(&recordingListener{name: "a", calls: &calls, failAfter: true})
	if _, err := store.Reload(context.Background()); err == nil {
		t.Fatal("failure swallowed")
	}
	unregister()
	if store.Current() != first {
		t.Fatal("current not reverted")
	}
	if DefaultStore() != prevDefault {
		t.Fatal("defaultStore not restored")
	}
	if fctx.RuntimeConfig() != prevConfig {
		t.Fatal("fctx runtime config not restored")
	}
	// previous must not have been polluted by the failed generation.
	if _, err := store.Rollback(context.Background(), "test"); err == nil {
		snapAfter := store.Current()
		if snapAfter.Version >= first.Version {
			t.Fatalf("rollback after failed reload landed on %d", snapAfter.Version)
		}
	}
}

func TestFirstLoadFailureLeavesNoTypedNilInRuntimeConfig(t *testing.T) {
	store, _ := newCommitTestStore(t)
	var calls []string
	store.AddReloadListener(&recordingListener{name: "a", calls: &calls, failAfter: true})
	if _, err := store.Load(context.Background()); err == nil {
		t.Fatal("failure swallowed")
	}
	if cfg := fctx.RuntimeConfig(); cfg != nil {
		if snap, ok := cfg.(*Snapshot); ok && snap == nil {
			t.Fatal("typed-nil *Snapshot parked in runtime config slot")
		}
	}
}

func TestRollbackTwiceIsRejected(t *testing.T) {
	store, dir := newCommitTestStore(t)
	if _, err := store.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"name":"bear","scene_id":7}]`)
	v2, err := store.Reload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	back, err := store.Rollback(context.Background(), "oops")
	if err != nil {
		t.Fatal(err)
	}
	if back.Version >= v2.Version {
		t.Fatalf("rollback landed on %d", back.Version)
	}
	// The bad generation must not be reachable as "previous" anymore: a
	// second rollback (operator double-click) must fail, not republish it.
	if _, err := store.Rollback(context.Background(), "double-click"); err == nil {
		t.Fatal("second rollback republished the rejected generation")
	}
	// And versions stay monotonic afterwards: v2 burned, rollback burned
	// nothing new, next reload allocates past everything seen so far.
	next, err := store.Reload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if next.Version <= v2.Version {
		t.Fatalf("version reused after rollback: %d <= %d", next.Version, v2.Version)
	}
}

func TestWrapperDocumentsHappyPathAndGuards(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry()
	MustRegisterTable(reg, TableDef[int32, testMonsterCfg]{
		Name: "monster", File: "monster.json",
		Key: func(v testMonsterCfg) int32 { return v.ID },
	})
	store := NewStore(reg, dir)

	writeFile(t, filepath.Join(dir, "monster.json"), `{"rows":[{"id":1,"name":"wolf","scene_id":7}]}`)
	snap, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("wrapped rows document rejected: %v", err)
	}
	table, _ := TableFrom[int32, testMonsterCfg](snap, "monster")
	if table.Len() != 1 {
		t.Fatalf("wrapped rows not loaded: %d", table.Len())
	}
	// {"rows": null} must not sneak past the null guard.
	writeFile(t, filepath.Join(dir, "monster.json"), `{"rows":null}`)
	if _, err := store.Reload(context.Background()); err == nil || !strings.Contains(err.Error(), "null") {
		t.Fatalf("null wrapper accepted: %v", err)
	}
}

type strictWorldCfg struct {
	Width int32 `json:"width"`
}

func TestStrictModeWrapperObjectAndTrailingJunk(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry()
	MustRegisterObject(reg, ObjectDef[strictWorldCfg]{Name: "world", File: "world.json"})
	store := NewStore(reg, dir)
	store.SetStrictJSON(true)

	// A wrapped object document must decode identically in strict and
	// lenient mode (lenient used to zero it silently).
	writeFile(t, filepath.Join(dir, "world.json"), `{"data":{"width":42}}`)
	snap, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("strict wrapped object rejected: %v", err)
	}
	world, _ := ObjectFrom[strictWorldCfg](snap, "world")
	if world.Width != 42 {
		t.Fatalf("strict wrapped object zeroed: %+v", world)
	}
	store.SetStrictJSON(false)
	snap2, err := store.Reload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	world2, _ := ObjectFrom[strictWorldCfg](snap2, "world")
	if world2.Width != 42 {
		t.Fatalf("lenient wrapped object zeroed (mode divergence): %+v", world2)
	}
	// Strict must reject trailing junk (it used to be MORE lenient here).
	store.SetStrictJSON(true)
	writeFile(t, filepath.Join(dir, "world.json"), `{"width":42}{"junk":1}`)
	if _, err := store.Reload(context.Background()); err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("strict accepted trailing junk: %v", err)
	}
}

func TestEmbeddedRefPositiveAndShadowingRejected(t *testing.T) {
	// Positive: promoted ref passes with a valid value.
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "drop.json"), `[{"id":100,"bonus":1}]`)
	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"name":"wolf","drop_id":100}]`)
	reg := NewRegistry()
	MustRegisterAutoTable[int32, autoDropCfg](reg, WithAutoName("drop"))
	MustRegisterAutoTable[int32, embeddedMonsterCfg](reg, WithAutoName("monster"), WithAutoFile("monster.json"))
	if _, err := NewStore(reg, dir).Load(context.Background()); err != nil {
		t.Fatalf("valid embedded ref rejected: %v", err)
	}

	// Outer field shadows an embedded cfg-tagged field: must be a
	// registration error, never a silently zero key.
	type keyBase struct {
		ID int32 `json:"id" cfg:"key"`
	}
	type shadowed struct {
		keyBase
		ID int32 `json:"id"`
	}
	if err := RegisterAutoTable[int32, shadowed](NewRegistry()); err == nil || !strings.Contains(err.Error(), "shadowed") {
		t.Fatalf("shadowed cfg key accepted: %v", err)
	}

	// Embedded field with a json name is NOT promoted by encoding/json —
	// cfg tags inside it must be rejected.
	type namedEmbed struct {
		keyBase `json:"base"`
		Name    string `json:"name"`
	}
	if err := RegisterAutoTable[int32, namedEmbed](NewRegistry()); err == nil || !strings.Contains(err.Error(), "json name") {
		t.Fatalf("json-named embed with cfg tags accepted: %v", err)
	}
}

func TestIndexNameContainingRequiredIsAccepted(t *testing.T) {
	// Regression for the substring parsing bug: "required" inside an index
	// name must not be treated as the required directive.
	type levels struct {
		ID    int32 `json:"id" cfg:"key"`
		Level int32 `json:"level" cfg:"index=required_level"`
	}
	if err := RegisterAutoTable[int32, levels](NewRegistry()); err != nil {
		t.Fatalf("index=required_level rejected: %v", err)
	}
	type twoRefs struct {
		ID int32 `json:"id" cfg:"key"`
		A  int32 `json:"a" cfg:"ref=x,ref=y"`
	}
	if err := RegisterAutoTable[int32, twoRefs](NewRegistry()); err == nil {
		t.Fatal("multiple ref directives accepted")
	}
}

func TestOpaqueCustomWithoutFingerprintFailsBuild(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry()
	MustRegisterCustom(reg, CustomDef[*opaqueTables]{
		Name: "opaque",
		Build: func(*BuildContext) (*opaqueTables, error) {
			return &opaqueTables{rows: map[int32]string{1: "x"}}, nil
		},
	})
	if _, err := NewStore(reg, dir).Load(context.Background()); err == nil || !strings.Contains(err.Error(), "serializes opaquely") {
		t.Fatalf("opaque custom silently hashed as {}: %v", err)
	}
}

func TestUnknownFingerprintNameFailsBuild(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry()
	MustRegisterCustom(reg, CustomDef[int]{
		Name: "real",
		Build: func(ctx *BuildContext) (int, error) {
			ctx.Snapshot.SetFingerprint("tpyo", "abc") // misspelled member
			return 1, nil
		},
	})
	if _, err := NewStore(reg, dir).Load(context.Background()); err == nil || !strings.Contains(err.Error(), "unknown member") {
		t.Fatalf("misspelled fingerprint silently ignored: %v", err)
	}
}

func TestSetFingerprintIsSealedAfterFinalize(t *testing.T) {
	store, _ := newCommitTestStore(t)
	snap, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	before := snap.Hash
	snap.SetFingerprint("monster", "hijack") // must be a no-op on a published snapshot
	if snap.Hash != before {
		t.Fatal("published snapshot mutated")
	}
	if _, err := store.Reload(context.Background()); err != nil {
		t.Fatalf("reload after sealed SetFingerprint attempt: %v", err)
	}
}

func TestMustGetPanicsWithErrorsIsChain(t *testing.T) {
	table := &Table[int32, testMonsterCfg]{name: "monster", byKey: map[int32]int{}}
	defer func() {
		r := recover()
		err, ok := r.(error)
		if !ok || !errors.Is(err, ErrTableNotFound) {
			t.Fatalf("MustGet panic lost the error chain: %v", r)
		}
	}()
	table.MustGet(1)
}

func TestConcurrentReloadDryRunAndSetDir(t *testing.T) {
	store, dir := newCommitTestStore(t)
	if _, err := store.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			_, _ = store.Reload(context.Background())
		}()
		go func() {
			defer wg.Done()
			_, _ = store.DryRun(context.Background(), "probe")
		}()
		go func() {
			defer wg.Done()
			store.SetDir(dir)
		}()
	}
	wg.Wait()
	if store.Current() == nil {
		t.Fatal("store lost its snapshot under concurrency")
	}
}

func TestExternalFingerprintIsOrderIndependent(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.json"), `{"a":1}`)
	writeFile(t, filepath.Join(dir, "b.json"), `{"b":2}`)
	loadHash := func(order []string, parallel bool) string {
		reg := NewRegistry()
		MustRegisterExternalTables(reg, "ext", func(read func(string) ([]byte, error)) (int, error) {
			if parallel {
				var wg sync.WaitGroup
				for _, f := range order {
					wg.Add(1)
					go func(name string) { defer wg.Done(); _, _ = read(name) }(f)
				}
				wg.Wait()
				return 1, nil
			}
			for _, f := range order {
				if _, err := read(f); err != nil {
					return 0, err
				}
			}
			return 1, nil
		})
		snap, err := NewStore(reg, dir).Load(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return snap.Hash
	}
	forward := loadHash([]string{"a.json", "b.json"}, false)
	backward := loadHash([]string{"b.json", "a.json"}, false)
	concurrent := loadHash([]string{"a.json", "b.json"}, true)
	if forward != backward || forward != concurrent {
		t.Fatalf("fingerprint order-dependent: %s / %s / %s", forward, backward, concurrent)
	}
}
