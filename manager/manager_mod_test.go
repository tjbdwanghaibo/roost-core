package manager

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/app"
	coremanager "github.com/tjbdwanghaibo/roost-core/manager"
	"github.com/tjbdwanghaibo/roost-kit/mods"

	"github.com/spf13/viper"
)

// The lifecycle tests (ordering, rollback, abort on shutdown, idempotent stop)
// moved to roost-core/manager with the engine (M-09). What is asserted here is
// the Mod shape around it: the name, what Provide publishes, and that every
// Mod method reaches the engine.
type fakeManager struct {
	name      string
	dependsOn []string
	journal   *[]string
}

func (f *fakeManager) Name() string { return f.name }
func (f *fakeManager) Start(r *app.Registry) error {
	if r == nil {
		return fmt.Errorf("%s: nil registry", f.name)
	}
	*f.journal = append(*f.journal, "start:"+f.name)
	return nil
}
func (f *fakeManager) Stop()               { *f.journal = append(*f.journal, "stop:"+f.name) }
func (f *fakeManager) DependsOn() []string { return f.dependsOn }

func TestManagerModPublishesItselfUnderTheManagerModName(t *testing.T) {
	mod := NewManagerMod()
	if mod.Name() != mods.ModManager {
		t.Fatalf("Name() = %q, want %q", mod.Name(), mods.ModManager)
	}
	if err := mod.Provide(nil); err == nil || !contains(err.Error(), "registry is nil") {
		t.Fatalf("Provide(nil) = %v", err)
	}
	registry := app.NewRegistry(viper.New())
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	published, ok := app.Lookup[*ManagerMod](registry, mods.ModManager)
	if !ok || published != mod {
		t.Fatalf("Provide did not publish the mod under %q", mods.ModManager)
	}
}

// Start / StopWithContext / Register / Manager all forward to the engine: the
// dependency order shows up in the journal, and the engine's sentinel comes
// back under the kit name.
func TestManagerModForwardsTheLifecycleToTheEngine(t *testing.T) {
	var journal []string
	mod := NewManagerMod(
		&fakeManager{name: "nest", dependsOn: []string{"entity"}, journal: &journal},
		&fakeManager{name: "entity", journal: &journal},
	)
	if err := mod.Start(); err == nil || !contains(err.Error(), "Start before Provide") {
		t.Fatalf("Start before Provide = %v", err)
	}
	if err := mod.Provide(app.NewRegistry(viper.New())); err != nil {
		t.Fatal(err)
	}
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	if _, ok := mod.Manager("entity"); !ok || len(mod.Managers()) != 2 {
		t.Fatalf("Manager / Managers do not reach the engine: %v", mod.Managers())
	}
	err := mod.Register(&fakeManager{name: "late", journal: &journal})
	if !errors.Is(err, ErrManagerRegisterAfterStart) || !errors.Is(err, coremanager.ErrRegisterAfterStart) {
		t.Fatalf("Register after Start = %v, want the engine's sentinel under both names", err)
	}
	if err := mod.StopWithContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Stop() after StopWithContext is the idempotent second stop: nothing more
	// in the journal.
	mod.Stop()
	want := []string{"start:entity", "start:nest", "stop:nest", "stop:entity"}
	if fmt.Sprint(journal) != fmt.Sprint(want) {
		t.Fatalf("lifecycle through the mod:\n got %v\nwant %v", journal, want)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
