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

func TestReplaceVersionIsConsistentUnderConcurrentReads(t *testing.T) {
	store := NewStore()
	stop := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			store.Replace([]Flag{{Name: "f", Enabled: i%2 == 0}})
		}
		close(stop)
	}()
	for {
		select {
		case <-stop:
			return
		default:
			_ = store.Enabled("f")
			_ = store.Version()
		}
	}
}
