package lock

import (
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	flock "github.com/tjbdwanghaibo/roost-core/lock"
	"github.com/tjbdwanghaibo/roost-kit/mods"
)

func TestLockModProvidesReentrantLockManager(t *testing.T) {
	mod := NewLockMod()
	if mod.Name() != mods.ModLock {
		t.Fatalf("name=%s", mod.Name())
	}
	if err := mod.Init(viper.New()); err != nil {
		t.Fatal(err)
	}
	registry := app.NewRegistry(viper.New())
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	manager, ok := app.Lookup[*flock.LockManager](registry, mods.ModLock)
	if !ok || manager == nil {
		t.Fatal("lock manager capability not registered")
	}
	// The default factory yields per-id reentrant mutexes.
	mu := manager.GetLock(7)
	if mu == nil || mu.LockId() != 7 {
		t.Fatalf("lock=%v", mu)
	}
	mu.Lock()
	mu.Lock() // reentrant
	mu.Unlock()
	mu.Unlock()
	if same := manager.GetLock(7); same != mu {
		t.Fatal("same id must return the same lock instance")
	}

	// Duplicate capability registration must fail loudly, not overwrite.
	if err := mod.Provide(registry); err == nil {
		t.Fatal("second Provide must be rejected")
	}
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	mod.Stop()
}
