// 热更提交：监听器准备 / 提交 / 回滚的顺序与范围、panic 转错误、全局槽位恢复、回滚只一次、JSON 包装与严格模式、并发热更。

package configdata

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
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

type blockingFailListener struct {
	entered chan struct{}
	release chan struct{}
}

func (l *blockingFailListener) Name() string                                         { return "blocking-fail" }
func (l *blockingFailListener) ValidateReload(context.Context, ReloadEvent) error    { return nil }
func (l *blockingFailListener) BeforeApplyReload(context.Context, ReloadEvent) error { return nil }
func (l *blockingFailListener) AfterApplyReload(context.Context, ReloadEvent) error {
	close(l.entered)
	<-l.release
	return errFail
}
func (l *blockingFailListener) RollbackReload(context.Context, ReloadEvent, error) {}

func TestFailedCommitDoesNotRollBackAnotherStorePublication(t *testing.T) {
	previousDefault := DefaultStore()
	previousConfig := fctx.RuntimeConfig()
	defer func() {
		publishMu.Lock()
		SetDefaultStore(previousDefault)
		fctx.SetRuntimeConfig(previousConfig)
		publishMu.Unlock()
	}()

	storeA, _ := newCommitTestStore(t)
	storeB, _ := newCommitTestStore(t)
	oldA, err := storeA.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storeB.Load(context.Background()); err != nil {
		t.Fatal(err)
	}

	listener := &blockingFailListener{entered: make(chan struct{}), release: make(chan struct{})}
	storeA.AddReloadListener(listener)
	aResult := make(chan error, 1)
	go func() {
		_, err := storeA.Reload(context.Background())
		aResult <- err
	}()
	awaitChan(t, listener.entered, "the listener to enter commit")

	bTarget, err := storeB.Reload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	close(listener.release)
	if err := awaitChan(t, aResult, "store A's commit result"); err == nil {
		t.Fatal("store A reload unexpectedly succeeded")
	}

	if storeA.Current() != oldA {
		t.Fatal("store A did not revert its own snapshot")
	}
	if DefaultStore() != storeB || fctx.RuntimeConfig() != bTarget {
		t.Fatal("store A rollback overwrote store B's successful publication")
	}
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
	if cfg := fctx.RuntimeConfig(); cfg != nil {
		if snap, ok := cfg.(*Snapshot); ok && snap == nil {
			t.Fatal("typed-nil *Snapshot parked in runtime config slot")
		}
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

// awaitChan receives from ch with an upper bound. A bare receive made a broken
// property fail as a go test timeout — a stack dump after the default ten
// minutes, naming no expectation — so every wait that IS the assertion is
// bounded and says what it was waiting for.
func awaitChan[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

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
