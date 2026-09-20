package runtimeid

import (
	"fmt"
	"sync/atomic"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// Package runtimeid mints ids for entities the process creates at run time —
// a spawned monster, a dropped item — and nobody stores.
//
// Why it is not a counter: an entity id is a GLOBAL identity. The room keys
// subjects by it, the AOI keys subjects by it, and Nest addresses entities by
// it. Two game processes each counting from the same base mint the same id,
// and then two different monsters share a subscription, a route and a state
// slot (RR-20260918-09). A local counter is correct exactly as long as there
// is one process, which is the assumption this demo is otherwise built to
// stop making.
//
// The layout is the framework's own split, used as intended:
//
//	unique id (52 bits) = [static half (1)][shard (16)][sequence (34)]
//
// `entity.StaticUniqueIDBase` marks the upper half of the unique range, which
// the framework reserves for explicitly assigned ids — as opposed to the
// lower half, handed out in blocks by the account service's allocator. A
// runtime id is explicitly assigned, so it belongs there and can never
// collide with a player id however many players the allocator hands out.
//
// Above the base, the shard is the process's sid: stable, unique per
// deployment, and already required for everything else in the demo. What is
// left is the per-process sequence.
const (
	// ShardBits is how many distinct sids this scheme admits.
	ShardBits = 16
	// SequenceBits is how many ids one process may mint before it must be
	// restarted with a fresh... nothing. It cannot be refreshed: 2^34 is
	// seventeen billion, which is the point — overflow is a refusal, not a
	// wrap, and nobody will reach it.
	SequenceBits = 34

	MaxShard    = int64(1)<<ShardBits - 1
	MaxSequence = int64(1)<<SequenceBits - 1
)

// Allocator mints ids for one process.
type Allocator struct {
	base     int64
	sequence atomic.Int64
}

// New validates the sid at startup rather than at the first spawn: a process
// whose sid cannot be encoded must fail to start, not fail to spawn its first
// monster an hour later.
func New(sid int32) (*Allocator, error) {
	if sid <= 0 || int64(sid) > MaxShard {
		return nil, fmt.Errorf("runtimeid: sid %d is outside 1..%d, so it cannot be encoded into a runtime entity id", sid, MaxShard)
	}
	base := int64(entity.StaticUniqueIDBase) | int64(sid)<<SequenceBits
	return &Allocator{base: base}, nil
}

// Next returns the next unique id for this process, or an error once the
// sequence is exhausted. It returns an error rather than wrapping because a
// wrapped sequence re-mints ids that are still in use — the same collision
// this package exists to prevent, arriving from the other direction.
func (a *Allocator) Next() (int64, error) {
	next := a.sequence.Add(1)
	if next > MaxSequence {
		return 0, fmt.Errorf("runtimeid: this process has minted %d runtime ids, the most the %d-bit sequence holds", MaxSequence, SequenceBits)
	}
	return a.base | next, nil
}

// Shard reports which process minted a unique id. It is for logs and GM
// commands: "which process owns this monster" is the first question when one
// shows up somewhere it should not.
func Shard(uniqueID int64) int32 {
	if uniqueID&int64(entity.StaticUniqueIDBase) == 0 {
		return 0
	}
	return int32((uniqueID >> SequenceBits) & MaxShard)
}
