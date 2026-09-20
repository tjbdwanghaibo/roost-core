package remoteentity

import (
	"fmt"
	"math"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// BSON has no unsigned 64-bit integer, so every uint64 this package persists
// needs an answer to "what if the top bit is set". The answer depends on what
// the field is FOR (RR-20260920-01):
//
//	checksum            a full 64-bit hash; nothing compares or ranges over
//	                    it, so entity.RemoteChecksum stores it as 8 bytes and
//	                    keeps its whole domain.
//	version/epoch/fence monotonic counters that Mongo compares ($gte on a
//	                    snapshot read, CAS on a version). They must stay
//	                    numeric to keep that ordering, so their domain ends at
//	                    MaxInt64 and a commit beyond it is refused here.
//
// Refusing HERE is the half that matters most: a record that cannot be
// encoded must never become durable, because startup recovery replays it and
// fails the same deterministic way every time — the process stops coming up
// and the only way out is deleting the WAL.
func validateRemoteCommitEncodable(commit entity.RemoteCommit) error {
	for _, counter := range []struct {
		name  string
		value uint64
	}{
		{"base version", commit.BaseVersion},
		{"next version", commit.NextVersion},
		{"marker epoch", commit.MarkerEpoch},
		{"route epoch", commit.RouteEpoch},
		{"lock fence", commit.LockFence},
	} {
		if counter.value > math.MaxInt64 {
			return fmt.Errorf("%w: %s %d exceeds what BSON can hold (max %d)", entity.ErrRemoteRejected, counter.name, counter.value, uint64(math.MaxInt64))
		}
	}
	for _, mutation := range commit.Mutations {
		if mutation.Version > math.MaxInt64 {
			return fmt.Errorf("%w: mutation %d version %d exceeds what BSON can hold", entity.ErrRemoteRejected, mutation.ID, mutation.Version)
		}
	}
	for _, snapshot := range commit.Snapshots {
		// The checksum is deliberately absent: it is stored as bytes, so every
		// value is writable, and refusing it would reject half of a valid hash
		// space.
		for _, counter := range []struct {
			name  string
			value uint64
		}{
			{"state version", snapshot.StateVersion},
			{"base version", snapshot.BaseVersion},
			{"marker epoch", snapshot.MarkerEpoch},
			{"route epoch", snapshot.RouteEpoch},
		} {
			if counter.value > math.MaxInt64 {
				return fmt.Errorf("%w: snapshot %s %d exceeds what BSON can hold", entity.ErrRemoteRejected, counter.name, counter.value)
			}
		}
	}
	return nil
}
