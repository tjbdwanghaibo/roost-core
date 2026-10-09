package misc

import "testing"

// Hash64 shards worker queues and nest dispatch, so an identical input must
// always land on the same shard: the mapping has to be a pure function of the
// key, not of process state.
func TestHash64IsDeterministicAndSpreads(t *testing.T) {
	if Hash64(42) != Hash64(42) {
		t.Fatal("Hash64 is not deterministic")
	}
	// Consecutive keys must not collapse onto one shard, which is the failure
	// mode that silently serialises a whole worker pool.
	const shards = 8
	seen := make(map[uint64]int, shards)
	for key := uint64(0); key < 512; key++ {
		seen[Hash64(key)%shards]++
	}
	if len(seen) != shards {
		t.Fatalf("consecutive keys reached only %d/%d shards", len(seen), shards)
	}
	for shard, count := range seen {
		if count == 0 || count > 512/shards*3 {
			t.Fatalf("shard %d took %d of 512 keys: distribution is too skewed", shard, count)
		}
	}
}
