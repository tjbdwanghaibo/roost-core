package runtimeid

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// Two processes must never mint the same id. This is the whole reason the
// package exists: a local counter is correct for exactly one process, and the
// symptom of being wrong is two different entities sharing a subscription, a
// route and a state slot.
func TestTwoShardsNeverCollide(t *testing.T) {
	first, err := New(1000)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(1001)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[int64]struct{}, 2048)
	for index := 0; index < 1024; index++ {
		for _, allocator := range []*Allocator{first, second} {
			id, err := allocator.Next()
			if err != nil {
				t.Fatal(err)
			}
			if _, duplicate := seen[id]; duplicate {
				t.Fatalf("two processes minted the same id %d", id)
			}
			seen[id] = struct{}{}
		}
	}
}

// And a restart of the SAME process re-mints from the start of its sequence,
// which is safe only because nothing that used those ids survived the restart
// — runtime entities are ephemeral by construction. The test states it so the
// assumption is visible if somebody ever makes them persistent.
func TestARestartReusesTheSameShardsSequence(t *testing.T) {
	before, _ := New(1000)
	first, _ := before.Next()
	after, _ := New(1000)
	again, _ := after.Next()
	if first != again {
		t.Fatalf("a restarted process minted %d then %d; the sequence is meant to restart", first, again)
	}
}

// Runtime ids live in the half of the unique range the framework reserves for
// explicitly assigned ids, so they cannot collide with anything the account
// service's block allocator hands out.
func TestRuntimeIdsAreInTheStaticHalf(t *testing.T) {
	allocator, _ := New(1000)
	id, err := allocator.Next()
	if err != nil {
		t.Fatal(err)
	}
	if uint64(id) < entity.StaticUniqueIDBase {
		t.Fatalf("runtime id %d is below the static base %d, where the allocator hands out player ids", id, entity.StaticUniqueIDBase)
	}
	if uint64(id) > entity.UniqueIDMask {
		t.Fatalf("runtime id %d does not fit the 52-bit unique range", id)
	}
	if shard := Shard(id); shard != 1000 {
		t.Fatalf("Shard(%d) = %d, want the minting sid", id, shard)
	}
}

// A sid that cannot be encoded fails at startup, not at the first spawn.
func TestAnUnencodableSidIsRefusedAtStartup(t *testing.T) {
	for _, sid := range []int32{0, -1, int32(MaxShard) + 1} {
		if _, err := New(sid); err == nil {
			t.Errorf("sid %d was accepted; it does not fit %d bits", sid, ShardBits)
		}
	}
}

// Exhaustion is a refusal rather than a wrap: a wrapped sequence re-mints ids
// that are still in use.
func TestSequenceExhaustionIsRefused(t *testing.T) {
	allocator, _ := New(1000)
	allocator.sequence.Store(MaxSequence)
	if _, err := allocator.Next(); err == nil {
		t.Fatal("the sequence wrapped instead of refusing")
	}
}
