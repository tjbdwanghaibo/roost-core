package syncbus

import (
	"strings"
	"sync"
	"testing"
)

// Two minters never collide, one minter never repeats, and the id names its
// publish path. These are the three properties every publisher relies on.
func TestDeliveryIDsAreUniquePerMinterAndMonotonic(t *testing.T) {
	first, second := NewDeliveryIDs("patch"), NewDeliveryIDs("patch")
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		for _, minter := range []*DeliveryIDs{first, second} {
			id := minter.Next()
			if !strings.HasPrefix(id, "patch:") {
				t.Fatalf("id %q does not name its publish path", id)
			}
			if seen[id] {
				t.Fatalf("id %q was minted twice", id)
			}
			seen[id] = true
		}
	}
	if NewDeliveryIDs("").Next() == "" || !strings.HasPrefix(NewDeliveryIDs("").Next(), "sync:") {
		t.Fatal("an unnamed minter must still mint under a default kind")
	}
}

func TestDeliveryIDsAreSafeForConcurrentUse(t *testing.T) {
	minter := NewDeliveryIDs("stream")
	var (
		wait sync.WaitGroup
		mu   sync.Mutex
		seen = map[string]bool{}
	)
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for i := 0; i < 500; i++ {
				id := minter.Next()
				mu.Lock()
				if seen[id] {
					t.Errorf("id %q was minted twice", id)
				}
				seen[id] = true
				mu.Unlock()
			}
		}()
	}
	wait.Wait()
	if len(seen) != 4000 {
		t.Fatalf("minted %d distinct ids, want 4000", len(seen))
	}
}
