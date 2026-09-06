package skill

import (
	"context"
	"sync"
	"testing"
	"time"
)

// RestoreRuntime must not take NewRuntime's fast-forward path: that path
// compacts every host event up to the current frontier, which would delete
// the events emitted between the checkpoint and the crash — exactly the ones a
// restored runtime replays. A temporary revert to NewRuntime left every test
// green (U-0028); this one pins it with a compacting host.
func TestRestoreRuntimeKeepsTheEventsEmittedAfterTheCheckpoint(t *testing.T) {
	host := NewMemoryHostWithOptions(AuthorityIdentity{Revision: "test", Digest: "test"}, MemoryHostOptions{CompactEvents: true})
	runtime := NewRuntime(host, RuntimeOptions{})
	if err := runtime.Advance(0); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	// Three events land after the checkpoint and before the "crash".
	host.mutex.Lock()
	for i := 0; i < 3; i++ {
		host.appendEventLocked("after-checkpoint", 0, 0)
	}
	host.mutex.Unlock()
	before := host.Events(0)
	if len(before) != 3 {
		t.Fatalf("host holds %d events before restore, want 3", len(before))
	}

	restored, err := RestoreRuntime(host, RuntimeOptions{}, checkpoint, ProgramResolverFunc(func(string, string) (*Program, error) { return nil, ErrCheckpointProgram }))
	if err != nil {
		t.Fatal(err)
	}
	after := host.Events(0)
	if len(after) != 3 {
		t.Fatalf("restore compacted the host's events: %d left of 3; a restored runtime can no longer replay what happened after its checkpoint", len(after))
	}
	if restored.eventCursor != 0 {
		t.Fatalf("restored cursor = %d, want the checkpoint's 0 so the three events are replayed", restored.eventCursor)
	}
}

// gatedLoader parks the preload of one asset until released, so a second
// Acquire of the same plan is caught waiting on the entry's ready channel.
type gatedLoader struct {
	cacheVisualLoader
	gate    chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (loader *gatedLoader) Preload(ctx context.Context, asset VisualAsset) error {
	if asset.Key == "fallback" {
		loader.once.Do(func() { close(loader.entered) })
		<-loader.gate
	}
	return loader.cacheVisualLoader.Preload(ctx, asset)
}

// An Acquire that waits for a plan another Acquire is still loading holds a
// reference from the moment it starts waiting. Without that reservation the
// first holder's Release finds refs == 0, evicts the entry and unloads the
// assets while the waiter is about to hand them out. A temporary revert of
// the reservation left every test green (U-0028).
func TestAWaitingAcquireIsNotEvictedByTheFirstHoldersRelease(t *testing.T) {
	loader := &gatedLoader{gate: make(chan struct{}), entered: make(chan struct{})}
	cache, err := NewVisualPlanCache(VisualPlanCacheOptions{Resolver: cacheVisualResolver{}, Trust: TrustedVisualCatalogs{"r1": "d1"}, Loader: loader, RequireTrust: true, MaxIdlePlans: 0})
	if err != nil {
		t.Fatal(err)
	}
	plan := PresentationPlan{Identity: ProgramIdentityView{PresentationDigest: "plan-race"}, Manifest: SkillVisualManifest{CatalogRevision: "r1", CatalogDigest: "d1", Entries: []VisualView{{Index: 0, Category: "impact", Theme: "default", Elements: []string{"fire"}}}}, Effects: []PresentationMount{{VisualIndex: 0}}}

	type result struct {
		lease *VisualPlanLease
		err   error
	}
	first := make(chan result, 1)
	go func() {
		lease, err := cache.Acquire(context.Background(), plan)
		first <- result{lease, err}
	}()
	<-loader.entered // the first Acquire is inside the loader
	second := make(chan result, 1)
	go func() {
		lease, err := cache.Acquire(context.Background(), plan)
		second <- result{lease, err}
	}()
	time.Sleep(50 * time.Millisecond) // let the second Acquire reach the ready wait
	close(loader.gate)

	a := <-first
	if a.err != nil {
		t.Fatal(a.err)
	}
	if err := a.lease.Release(); err != nil {
		t.Fatal(err)
	}
	b := <-second
	if b.err != nil {
		t.Fatalf("the waiting Acquire failed: %v", b.err)
	}
	loader.mutex.Lock()
	unloaded := len(loader.unloaded)
	loader.mutex.Unlock()
	if unloaded != 0 {
		t.Fatalf("the first holder's release unloaded %v while a second lease was being handed out", loader.unloaded)
	}
	if got := b.lease.Plan().Assets[0].Asset.Key; got != "fallback" {
		t.Fatalf("second lease asset = %q", got)
	}
	if err := b.lease.Release(); err != nil {
		t.Fatal(err)
	}
	loader.mutex.Lock()
	defer loader.mutex.Unlock()
	if len(loader.unloaded) != 1 {
		t.Fatalf("after the last release the plan's asset was not unloaded: %v", loader.unloaded)
	}
}
