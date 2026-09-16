package manager

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/app"

	"github.com/spf13/viper"
)

// fakeManager records lifecycle calls against a shared journal so the tests
// assert on real ordering rather than on per-manager booleans.
type fakeManager struct {
	name      string
	dependsOn []string
	journal   *[]string
	startErr  error
	onStart   func()
}

func (f *fakeManager) Name() string { return f.name }

func (f *fakeManager) Start(r *app.Registry) error {
	if r == nil {
		return fmt.Errorf("%s: nil registry", f.name)
	}
	*f.journal = append(*f.journal, "start:"+f.name)
	if f.onStart != nil {
		f.onStart()
	}
	return f.startErr
}

func (f *fakeManager) Stop() { *f.journal = append(*f.journal, "stop:"+f.name) }

func (f *fakeManager) DependsOn() []string { return f.dependsOn }

// ctxManager implements the optional bounded-shutdown hook.
type ctxManager struct {
	fakeManager
	stopCtxErr error
	sawContext context.Context
}

func (c *ctxManager) StopWithContext(ctx context.Context) error {
	c.sawContext = ctx
	*c.journal = append(*c.journal, "stopctx:"+c.name)
	return c.stopCtxErr
}

func newStartedEngine(t *testing.T, managers ...app.IManager) (*Engine, *app.Registry) {
	t.Helper()
	mod := NewEngine(managers...)
	registry := app.NewRegistry(viper.New())
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	return mod, registry
}

func TestEngineStartsInRegistrationOrderAndStopsInReverse(t *testing.T) {
	var journal []string
	mod, _ := newStartedEngine(t,
		&fakeManager{name: "a", journal: &journal},
		&fakeManager{name: "b", journal: &journal},
		&fakeManager{name: "c", journal: &journal},
	)
	if err := mod.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"start:a", "start:b", "start:c", "stop:c", "stop:b", "stop:a"}
	if got := fmt.Sprint(journal); got != fmt.Sprint(want) {
		t.Fatalf("lifecycle order:\n got %v\nwant %v", journal, want)
	}
}

// DependsOn must reorder the start sequence even when registration order
// contradicts it, and the stop sequence must be the exact reverse — a manager
// must never outlive something it depends on.
func TestEngineStartsDependenciesFirstAndTearsDownAfterDependents(t *testing.T) {
	var journal []string
	mod, _ := newStartedEngine(t,
		&fakeManager{name: "nest", dependsOn: []string{"entity", "save"}, journal: &journal},
		&fakeManager{name: "save", dependsOn: []string{"entity"}, journal: &journal},
		&fakeManager{name: "entity", journal: &journal},
	)
	if err := mod.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"start:entity", "start:save", "start:nest",
		"stop:nest", "stop:save", "stop:entity",
	}
	if got := fmt.Sprint(journal); got != fmt.Sprint(want) {
		t.Fatalf("dependency order:\n got %v\nwant %v", journal, want)
	}
}

// Independent managers must come out in registration order on every run.
// A map-seeded topological sort passes a single run and varies across runs,
// so this asserts stability across repeats rather than one lucky order.
func TestEngineOrderIsStableAcrossRunsForIndependentManagers(t *testing.T) {
	var first []string
	for run := 0; run < 50; run++ {
		var journal []string
		managers := make([]app.IManager, 0, 8)
		for index := 0; index < 8; index++ {
			managers = append(managers, &fakeManager{name: fmt.Sprintf("m%d", index), journal: &journal})
		}
		mod := NewEngine(managers...)
		if err := mod.Provide(app.NewRegistry(viper.New())); err != nil {
			t.Fatal(err)
		}
		if err := mod.Start(); err != nil {
			t.Fatal(err)
		}
		if run == 0 {
			first = journal
			continue
		}
		if fmt.Sprint(journal) != fmt.Sprint(first) {
			t.Fatalf("start order varied between runs:\n run 0: %v\n run %d: %v", first, run, journal)
		}
	}
}

// A failing Start must leave nothing running: the managers that did start are
// rolled back in reverse, and the error names the manager that failed.
func TestEngineRollsBackStartedManagersWhenOneFails(t *testing.T) {
	var journal []string
	mod := NewEngine(
		&fakeManager{name: "a", journal: &journal},
		&fakeManager{name: "b", journal: &journal},
		&fakeManager{name: "boom", journal: &journal, startErr: errors.New("disk on fire")},
		&fakeManager{name: "never", journal: &journal},
	)
	if err := mod.Provide(app.NewRegistry(viper.New())); err != nil {
		t.Fatal(err)
	}
	err := mod.Start()
	if err == nil {
		t.Fatal("Start succeeded despite a failing manager")
	}
	if got := err.Error(); got == "" || !errorContains(got, "manager boom start") || !errorContains(got, "disk on fire") {
		t.Fatalf("error does not name the failing manager and cause: %v", err)
	}
	// The failing manager is NOT stopped: it never completed Start, so Stop
	// would have to handle a half-built object. Start owns cleanup of its own
	// failure; the mod rolls back only what reported success.
	want := []string{"start:a", "start:b", "start:boom", "stop:b", "stop:a"}
	if got := fmt.Sprint(journal); got != fmt.Sprint(want) {
		t.Fatalf("rollback order:\n got %v\nwant %v", journal, want)
	}
	// "never" must not have been started at all.
	for _, entry := range journal {
		if entry == "start:never" {
			t.Fatal("a manager after the failure was started")
		}
	}
}

// Stop must prefer the bounded hook and pass the caller's context
// through, so a shutdown budget actually reaches the manager.
func TestEngineStopPrefersBoundedHookAndPassesContext(t *testing.T) {
	var journal []string
	bounded := &ctxManager{fakeManager: fakeManager{name: "bounded", journal: &journal}}
	mod, _ := newStartedEngine(t, bounded, &fakeManager{name: "plain", journal: &journal})

	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "budget")
	if err := mod.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if bounded.sawContext == nil || bounded.sawContext.Value(key{}) != "budget" {
		t.Fatal("Stop did not pass the caller's context to the manager")
	}
	want := []string{"start:bounded", "start:plain", "stop:plain", "stopctx:bounded"}
	if got := fmt.Sprint(journal); got != fmt.Sprint(want) {
		t.Fatalf("stop path:\n got %v\nwant %v", journal, want)
	}
}

// Every manager must be given a chance to stop even when an earlier one
// fails, and every failure must be reported — a shutdown that stops at the
// first error leaks the rest.
func TestEngineStopReportsEveryFailureAndStillStopsTheRest(t *testing.T) {
	var journal []string
	firstErr := errors.New("first refused")
	secondErr := errors.New("second refused")
	mod, _ := newStartedEngine(t,
		&fakeManager{name: "plain", journal: &journal},
		&ctxManager{fakeManager: fakeManager{name: "bad1", journal: &journal}, stopCtxErr: firstErr},
		&ctxManager{fakeManager: fakeManager{name: "bad2", journal: &journal}, stopCtxErr: secondErr},
	)
	err := mod.Stop(context.Background())
	if err == nil {
		t.Fatal("Stop reported success despite failing managers")
	}
	if !errors.Is(err, firstErr) || !errors.Is(err, secondErr) {
		t.Fatalf("Stop dropped a failure: %v", err)
	}
	for _, entry := range []string{"stopctx:bad2", "stopctx:bad1", "stop:plain"} {
		if !sliceHas(journal, entry) {
			t.Fatalf("%s never ran; a failure stopped the teardown: %v", entry, journal)
		}
	}
}

// Stop is idempotent: a second Stop must not stop anything twice, which for a
// manager holding OS resources is a double-close.
func TestEngineStopIsIdempotent(t *testing.T) {
	var journal []string
	mod, _ := newStartedEngine(t, &fakeManager{name: "a", journal: &journal})
	if err := mod.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := mod.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	stops := 0
	for _, entry := range journal {
		if entry == "stop:a" {
			stops++
		}
	}
	if stops != 1 {
		t.Fatalf("manager was stopped %d times, want 1: %v", stops, journal)
	}
}

// Registering after Start must fail loudly. Accepting it would add a manager
// that is never started and never stopped, and the only symptom would be a
// nil dependency somewhere far away.
func TestEngineRefusesRegisterAfterStart(t *testing.T) {
	var journal []string
	mod, _ := newStartedEngine(t, &fakeManager{name: "a", journal: &journal})

	err := mod.Register(&fakeManager{name: "late", journal: &journal})
	if !errors.Is(err, ErrRegisterAfterStart) {
		t.Fatalf("Register after Start returned %v, want ErrRegisterAfterStart", err)
	}
	if _, exists := mod.Manager("late"); exists {
		t.Fatal("the refused manager was still recorded")
	}

	// Also refused after the lifecycle has ended.
	if err := mod.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := mod.Register(&fakeManager{name: "later", journal: &journal}); !errors.Is(err, ErrRegisterAfterStart) {
		t.Fatalf("Register after Stop returned %v, want ErrRegisterAfterStart", err)
	}
}

// Start without Provide has no registry to hand the managers. Failing here
// beats handing every manager a nil registry and failing later.
func TestEngineStartBeforeProvideFails(t *testing.T) {
	var journal []string
	mod := NewEngine(&fakeManager{name: "a", journal: &journal})
	err := mod.Start()
	if err == nil {
		t.Fatal("Start succeeded without Provide")
	}
	if !errorContains(err.Error(), "Start before Provide") {
		t.Fatalf("error does not explain the cause: %v", err)
	}
	if len(journal) != 0 {
		t.Fatalf("a manager was started without a registry: %v", journal)
	}
}

// Managers() must hand out a copy: the returned slice drives nothing, and the
// mod's own list must not be editable through it.
func TestEngineManagersReturnsACopy(t *testing.T) {
	var journal []string
	mod := NewEngine(&fakeManager{name: "a", journal: &journal})
	snapshot := mod.Managers()
	if len(snapshot) != 1 {
		t.Fatalf("Managers() returned %d entries", len(snapshot))
	}
	snapshot[0] = nil
	if again := mod.Managers(); again[0] == nil {
		t.Fatal("Managers() aliases the mod's own list")
	}
}

// NewEngine must copy its argument slice too, so a caller reusing the
// backing array cannot rewrite the lifecycle after construction.
func TestNewEngineCopiesItsArguments(t *testing.T) {
	var journal []string
	managers := []app.IManager{&fakeManager{name: "a", journal: &journal}}
	mod := NewEngine(managers...)
	managers[0] = &fakeManager{name: "swapped", journal: &journal}
	if _, exists := mod.Manager("a"); !exists {
		t.Fatal("NewEngine aliased the caller's slice")
	}
}

// Stop can arrive from the signal handler while Start is still working through
// the managers. Neither may stop a manager twice nor lose one.
func TestEngineStopConcurrentWithStartStopsEachManagerAtMostOnce(t *testing.T) {
	var mu sync.Mutex
	stops := map[string]int{}
	countStop := func(name string) {
		mu.Lock()
		stops[name]++
		mu.Unlock()
	}

	slow := make(chan struct{})
	managers := []app.IManager{
		&countingManager{name: "a", onStop: countStop},
		&countingManager{name: "b", onStop: countStop, onStart: func() { close(slow); time.Sleep(50 * time.Millisecond) }},
		&countingManager{name: "c", onStop: countStop},
	}
	mod := NewEngine(managers...)
	if err := mod.Provide(app.NewRegistry(viper.New())); err != nil {
		t.Fatal(err)
	}

	startDone := make(chan error, 1)
	go func() { startDone <- mod.Start() }()
	<-slow
	stopDone := make(chan error, 1)
	go func() { stopDone <- mod.Stop(context.Background()) }()

	for _, ch := range []chan error{startDone, stopDone} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("Start/Stop deadlocked against each other")
		}
	}
	// Whatever the interleaving, no manager may be stopped twice.
	if err := mod.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for name, count := range stops {
		if count > 1 {
			t.Fatalf("manager %s was stopped %d times", name, count)
		}
	}
}

type countingManager struct {
	name    string
	onStart func()
	onStop  func(string)
}

func (c *countingManager) Name() string { return c.name }
func (c *countingManager) Start(*app.Registry) error {
	if c.onStart != nil {
		c.onStart()
	}
	return nil
}
func (c *countingManager) Stop() { c.onStop(c.name) }

func errorContains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func sliceHas(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// A shutdown that arrives mid-startup must abort the startup, not race it.
// Without this, Stop drains the managers started so far and Start then keeps
// starting the rest — leaving managers running after shutdown reported done.
func TestEngineShutdownDuringStartAbortsTheRemainingManagers(t *testing.T) {
	var mu sync.Mutex
	var journal []string
	record := func(entry string) {
		mu.Lock()
		journal = append(journal, entry)
		mu.Unlock()
	}

	reachedSecond := make(chan struct{})
	stopObserved := make(chan struct{})
	managers := []app.IManager{
		&hookManager{name: "first", onStart: func() { record("start:first") }, onStop: func() { record("stop:first") }},
		&hookManager{
			name: "slow",
			onStart: func() {
				record("start:slow")
				close(reachedSecond)
				// Hold here until the test has issued Stop, so the abort
				// check runs with the shutdown already requested.
				<-stopObserved
			},
			onStop: func() { record("stop:slow") },
		},
		&hookManager{name: "third", onStart: func() { record("start:third") }, onStop: func() { record("stop:third") }},
	}
	mod := NewEngine(managers...)
	if err := mod.Provide(app.NewRegistry(viper.New())); err != nil {
		t.Fatal(err)
	}

	startDone := make(chan error, 1)
	go func() { startDone <- mod.Start() }()
	<-reachedSecond

	stopDone := make(chan error, 1)
	go func() {
		stopDone <- mod.Stop(context.Background())
		close(stopObserved)
	}()

	var startErr error
	select {
	case startErr = <-startDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Start never returned after shutdown was requested")
	}
	select {
	case <-stopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop never returned")
	}

	if startErr == nil {
		t.Fatal("Start reported success even though shutdown aborted it")
	}
	if !errorContains(startErr.Error(), "aborted by shutdown") {
		t.Fatalf("Start error does not say it was aborted: %v", startErr)
	}

	mu.Lock()
	defer mu.Unlock()
	if sliceHas(journal, "start:third") {
		t.Fatalf("a manager was started after shutdown began: %v", journal)
	}
	// "slow" completed Start, so it must have been torn down by one of the
	// two paths rather than left running.
	if !sliceHas(journal, "stop:slow") {
		t.Fatalf("the manager that finished starting was left running: %v", journal)
	}
}

type hookManager struct {
	name    string
	onStart func()
	onStop  func()
}

func (h *hookManager) Name() string { return h.name }
func (h *hookManager) Start(*app.Registry) error {
	h.onStart()
	return nil
}
func (h *hookManager) Stop() { h.onStop() }
