package configdata

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func newCommitTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"name":"wolf","scene_id":7}]`)
	reg := NewRegistry()
	MustRegisterTable(reg, TableDef[int32, testMonsterCfg]{
		Name: "monster", File: "monster.json",
		Key: func(v testMonsterCfg) int32 { return v.ID },
	})
	return NewStore(reg, dir), dir
}

// recordingListener tracks which callbacks ran, and can fail or panic per phase.
type recordingListener struct {
	name        string
	calls       *[]string
	failBefore  bool
	failAfter   bool
	panicAfter  bool
	panicBefore bool
}

func (l *recordingListener) Name() string { return l.name }
func (l *recordingListener) ValidateReload(context.Context, ReloadEvent) error {
	*l.calls = append(*l.calls, l.name+".validate")
	return nil
}
func (l *recordingListener) BeforeApplyReload(context.Context, ReloadEvent) error {
	*l.calls = append(*l.calls, l.name+".before")
	if l.panicBefore {
		panic("before boom")
	}
	if l.failBefore {
		return errFail
	}
	return nil
}
func (l *recordingListener) AfterApplyReload(context.Context, ReloadEvent) error {
	*l.calls = append(*l.calls, l.name+".after")
	if l.panicAfter {
		panic("after boom")
	}
	if l.failAfter {
		return errFail
	}
	return nil
}
func (l *recordingListener) RollbackReload(context.Context, ReloadEvent, error) {
	*l.calls = append(*l.calls, l.name+".rollback")
}

var errFail = &validationError{"listener failed"}

func TestListenerPanicFailsReloadAndKeepsStateConsistent(t *testing.T) {
	store, _ := newCommitTestStore(t)
	first, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	unregister := store.AddReloadListener(&recordingListener{name: "a", calls: &calls, panicAfter: true})

	if _, err := store.Reload(context.Background()); err == nil || !strings.Contains(err.Error(), "panic: after boom") {
		t.Fatalf("panic not converted to error: %v", err)
	}
	unregister()
	// The store fully reverted: current, version and previous are the
	// pre-reload generation, not a mixed one.
	if got := store.Current(); got != first {
		t.Fatalf("current = v%d, want the pre-reload snapshot v%d", got.Version, first.Version)
	}
	// A subsequent reload gets a fresh, non-colliding version: the failed
	// generation BURNED its number (it was already exposed to listeners),
	// so the next successful reload is first+2, never a reused first+1.
	snap, err := store.Reload(context.Background())
	if err != nil {
		t.Fatalf("reload after recovered panic: %v", err)
	}
	if snap.Version != first.Version+2 {
		t.Fatalf("version after recovered panic = %d, want %d (burned number must not be reused)", snap.Version, first.Version+2)
	}
}

func TestBeforeApplyFailureRollsBackOnlyPreparedListeners(t *testing.T) {
	store, _ := newCommitTestStore(t)
	if _, err := store.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	var calls []string
	store.AddReloadListener(&recordingListener{name: "a", calls: &calls})
	store.AddReloadListener(&recordingListener{name: "b", calls: &calls, failBefore: true})
	store.AddReloadListener(&recordingListener{name: "c", calls: &calls})

	if _, err := store.Reload(context.Background()); err == nil {
		t.Fatal("failure swallowed")
	}
	joined := strings.Join(calls, ",")
	// a prepared -> a must be rolled back; b failed its own prepare and c
	// never prepared -> neither gets a rollback.
	if !strings.Contains(joined, "a.rollback") {
		t.Fatalf("prepared listener a not rolled back: %s", joined)
	}
	if strings.Contains(joined, "b.rollback") || strings.Contains(joined, "c.rollback") {
		t.Fatalf("unprepared listeners rolled back: %s", joined)
	}
	if strings.Contains(joined, "c.before") {
		t.Fatalf("before-apply continued past the failure: %s", joined)
	}
}

func TestAfterApplyFailureRollsBackAllPreparedInReverse(t *testing.T) {
	store, _ := newCommitTestStore(t)
	first, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	store.AddReloadListener(&recordingListener{name: "a", calls: &calls})
	store.AddReloadListener(&recordingListener{name: "b", calls: &calls, failAfter: true})

	if _, err := store.Reload(context.Background()); err == nil {
		t.Fatal("failure swallowed")
	}
	joined := strings.Join(calls, ",")
	// Rollback pairs with BeforeApply: both prepared, both roll back, b first.
	if !strings.HasSuffix(joined, "b.rollback,a.rollback") {
		t.Fatalf("rollback order wrong: %s", joined)
	}
	if store.Current() != first {
		t.Fatal("store not reverted")
	}
}

func TestFirstLoadFailureSkipsRollbackCallbacks(t *testing.T) {
	store, _ := newCommitTestStore(t)
	var calls []string
	store.AddReloadListener(&recordingListener{name: "a", calls: &calls, failAfter: true})

	if _, err := store.Load(context.Background()); err == nil {
		t.Fatal("failure swallowed")
	}
	// No previous generation exists: rollback callbacks must not fire (a
	// listener must never interpret Old == nil as "revert to defaults").
	if strings.Contains(strings.Join(calls, ","), "rollback") {
		t.Fatalf("rollback fired on first load: %v", calls)
	}
	if store.Current() != nil {
		t.Fatal("current not nil after failed first load")
	}
}

func TestDuplicateRegistrationIsAnError(t *testing.T) {
	reg := NewRegistry()
	def := TableDef[int32, testMonsterCfg]{
		Name: "monster", File: "monster.json",
		Key: func(v testMonsterCfg) int32 { return v.ID },
	}
	if err := RegisterTable(reg, def); err != nil {
		t.Fatal(err)
	}
	if err := RegisterTable(reg, def); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("same-kind duplicate silently dropped: %v", err)
	}
}

func TestReadJSONRejectsNullAndAmbiguousWrappers(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry()
	MustRegisterTable(reg, TableDef[int32, testMonsterCfg]{
		Name: "monster", File: "monster.json",
		Key: func(v testMonsterCfg) int32 { return v.ID },
	})
	store := NewStore(reg, dir)

	writeFile(t, filepath.Join(dir, "monster.json"), `null`)
	if _, err := store.Load(context.Background()); err == nil || !strings.Contains(err.Error(), "null") {
		t.Fatalf("null document accepted: %v", err)
	}
	writeFile(t, filepath.Join(dir, "monster.json"), `{"rows":[{"id":1,"name":"a","scene_id":1}],"data":[{"id":2,"name":"b","scene_id":1}]}`)
	if _, err := store.Load(context.Background()); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous wrappers accepted: %v", err)
	}
}

func TestStrictJSONRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	// The classic silent-drift scenario: the column was renamed in the data.
	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"name":"wolf","sceneId":7}]`)
	reg := NewRegistry()
	MustRegisterTable(reg, TableDef[int32, testMonsterCfg]{
		Name: "monster", File: "monster.json",
		Key: func(v testMonsterCfg) int32 { return v.ID },
	})
	store := NewStore(reg, dir)
	if _, err := store.Load(context.Background()); err != nil {
		t.Fatalf("lenient mode must accept unknown fields: %v", err)
	}
	store.SetStrictJSON(true)
	if _, err := store.Reload(context.Background()); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("strict mode accepted renamed column: %v", err)
	}
}

func TestBuildPanicInCustomBecomesError(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry()
	MustRegisterCustom(reg, CustomDef[int]{
		Name:  "boom",
		Build: func(*BuildContext) (int, error) { panic("builder exploded") },
	})
	store := NewStore(reg, dir)
	if _, err := store.Load(context.Background()); err == nil || !strings.Contains(err.Error(), "panic: builder exploded") {
		t.Fatalf("custom build panic escaped: %v", err)
	}
}

func TestCustomBuildRunsAfterTableValidation(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "drop.json"), `[{"id":100,"bonus":1}]`)
	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"name":"wolf","scene_id":7,"drop_id":999}]`)
	reg := NewRegistry()
	MustRegisterAutoTable[int32, autoDropCfg](reg, WithAutoName("drop"))
	MustRegisterAutoTable[int32, autoMonsterCfg](reg, WithAutoName("monster"), WithAutoFile("monster.json"))
	built := false
	MustRegisterCustom(reg, CustomDef[int]{
		Name:  "derived",
		Build: func(*BuildContext) (int, error) { built = true; return 1, nil },
	})
	store := NewStore(reg, dir)
	_, err := store.Load(context.Background())
	if err == nil || !strings.Contains(err.Error(), "missing drop key 999") {
		t.Fatalf("dangling ref not surfaced: %v", err)
	}
	// The custom builder must not have consumed unvalidated table data.
	if built {
		t.Fatal("custom built before table validation")
	}
}
