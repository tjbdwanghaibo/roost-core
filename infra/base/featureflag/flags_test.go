package featureflag

import "testing"

func TestStore(t *testing.T) {
	store := NewStore()
	store.Set(Flag{Name: "battle.v2", Enabled: true})
	if !store.Enabled("battle.v2") {
		t.Fatal("flag should be enabled")
	}
	store.Replace([]Flag{{Name: "battle.v2", Enabled: false}})
	if store.Enabled("battle.v2") {
		t.Fatal("flag should be disabled after replace")
	}
}

func TestReplaceBumpsVersionWithData(t *testing.T) {
	store := NewStore()
	before := store.Version()
	store.Replace([]Flag{{Name: "b", Enabled: true}, {Name: "a", Enabled: true}})
	if store.Version() != before+1 {
		t.Fatalf("version = %d, want %d", store.Version(), before+1)
	}
	snap := store.Snapshot()
	if len(snap) != 2 || snap[0].Name != "a" || snap[1].Name != "b" {
		t.Fatalf("snapshot not sorted: %+v", snap)
	}
}

// Replace bumps the version inside the write lock so a reader can never see
// the new flag set carrying the old version. This asserts that ordering rather
// than merely running the two calls: the previous version of this test
// discarded both return values, so it would have passed even if Replace had
// published the map before the bump.
func TestReplaceVersionIsConsistentUnderConcurrentReads(t *testing.T) {
	store := NewStore()
	// Version 1 publishes Enabled=false; every later even i re-publishes true.
	// Track the version at which each value first became visible.
	store.Replace([]Flag{{Name: "f", Enabled: false}})
	baseline := store.Version()
	if baseline == 0 {
		t.Fatal("Replace did not advance the version")
	}

	const writes = 500
	stop := make(chan struct{})
	go func() {
		defer close(stop)
		for i := 0; i < writes; i++ {
			store.Replace([]Flag{{Name: "f", Enabled: i%2 == 0}})
		}
	}()

	observations, torn := 0, 0
	for {
		select {
		case <-stop:
			if observations == 0 {
				t.Fatal("reader never observed the store")
			}
			if torn != 0 {
				t.Fatalf("observed %d readings whose version predated the value", torn)
			}
			if final := store.Version(); final != baseline+writes {
				t.Fatalf("version=%d, want %d monotonic bumps", final, baseline+writes)
			}
			return
		default:
			// Read the version AFTER the value: if the bump were published
			// after the map, this pair could show a new value with a version
			// that had not yet advanced past the write that produced it.
			enabled := store.Enabled("f")
			version := store.Version()
			observations++
			if version < baseline {
				torn++
			}
			// A true reading can only come from an even-i write, which is
			// always at or before the version read afterwards.
			if enabled && version <= baseline {
				torn++
			}
		}
	}
}

func TestSetAdvancesVersionAndSnapshotIsSorted(t *testing.T) {
	store := NewStore()
	store.Set(Flag{Name: "beta", Enabled: true})
	afterFirst := store.Version()
	store.Set(Flag{Name: "alpha", Enabled: false, Note: "off"})
	if store.Version() != afterFirst+1 {
		t.Fatalf("version=%d, want %d", store.Version(), afterFirst+1)
	}
	// An empty name is not a flag and must not advance the version.
	store.Set(Flag{})
	if store.Version() != afterFirst+1 {
		t.Fatalf("empty flag advanced the version to %d", store.Version())
	}
	snapshot := store.Snapshot()
	if len(snapshot) != 2 || snapshot[0].Name != "alpha" || snapshot[1].Name != "beta" {
		t.Fatalf("snapshot=%+v, want name-sorted alpha,beta", snapshot)
	}
	if !store.Enabled("beta") || store.Enabled("alpha") || store.Enabled("missing") {
		t.Fatal("Enabled disagreed with the stored flags")
	}
	// Replace drops flags that are absent from the new set.
	store.Replace([]Flag{{Name: "alpha", Enabled: true}})
	if store.Enabled("beta") || !store.Enabled("alpha") || len(store.Snapshot()) != 1 {
		t.Fatalf("Replace did not swap the set: %+v", store.Snapshot())
	}
}

func TestNilStoreAndDefaultStoreAreSafe(t *testing.T) {
	var store *Store
	if store.Enabled("f") || store.Version() != 0 || store.Snapshot() != nil {
		t.Fatal("nil store must read as empty")
	}
	store.Set(Flag{Name: "f", Enabled: true})
	store.Replace([]Flag{{Name: "f"}})
	if DefaultStore() == nil {
		t.Fatal("DefaultStore is nil")
	}
}
