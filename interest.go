package remoteentity

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/fnv"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/cube-core/entity"
	"github.com/tjbdwanghaibo/cube-core/metrics"
	"github.com/tjbdwanghaibo/cube-core/mirror"
)

const syncTopicRemoteInterest = "remote_entity_interest"

type remoteInterestRegistry struct {
	mu      sync.Mutex
	entries map[entity.RemoteSnapshotKey]map[int32]int64
	total   int
	maxKeys int
	maxSubs int
}

func newRemoteInterestRegistry(limits ...int) *remoteInterestRegistry {
	maxKeys, maxSubs := 65536, 262144
	if len(limits) > 0 && limits[0] > 0 {
		maxKeys = limits[0]
	}
	if len(limits) > 1 && limits[1] > 0 {
		maxSubs = limits[1]
	}
	return &remoteInterestRegistry{
		entries: make(map[entity.RemoteSnapshotKey]map[int32]int64),
		maxKeys: maxKeys,
		maxSubs: maxSubs,
	}
}

func (r *remoteInterestRegistry) renew(interest entity.RemoteSnapshotInterest) error {
	_, err := r.renewIfNeeded(interest, 0)
	return err
}

// renewIfNeeded returns whether the caller should publish the renewal. Local
// read paths pass a threshold so cache hits do not cause one network message
// per read; replica apply passes zero and remains idempotent.
func (r *remoteInterestRegistry) renewIfNeeded(interest entity.RemoteSnapshotInterest, remainingThreshold time.Duration) (bool, error) {
	if r == nil || interest.ConsumerSID == 0 || !interest.Key.Valid() || interest.ExpiresAt <= time.Now().UnixNano() {
		return false, entity.ErrRemoteRejected
	}
	now := time.Now().UnixNano()
	r.mu.Lock()
	defer r.mu.Unlock()
	consumers := r.entries[interest.Key]
	if consumers != nil {
		if current, exists := consumers[interest.ConsumerSID]; exists {
			if remainingThreshold > 0 && current-now > remainingThreshold.Nanoseconds() {
				return false, nil
			}
			if interest.ExpiresAt > current {
				consumers[interest.ConsumerSID] = interest.ExpiresAt
			}
			return true, nil
		}
	}
	if len(r.entries) >= r.maxKeys || r.total >= r.maxSubs {
		r.pruneExpiredLocked(now)
		consumers = r.entries[interest.Key]
	}
	if consumers == nil && len(r.entries) >= r.maxKeys || r.total >= r.maxSubs {
		metrics.IncCounter("remote_entity.remote.interest_rejected_total", nil, 1)
		return false, entity.ErrRemoteOverloaded
	}
	if consumers == nil {
		consumers = make(map[int32]int64)
		r.entries[interest.Key] = consumers
	}
	consumers[interest.ConsumerSID] = interest.ExpiresAt
	r.total++
	return true, nil
}

func (r *remoteInterestRegistry) release(key entity.RemoteSnapshotKey, consumerSID int32) {
	if r == nil {
		return
	}
	r.mu.Lock()
	consumers := r.entries[key]
	if _, exists := consumers[consumerSID]; exists {
		delete(consumers, consumerSID)
		r.total--
	}
	if len(consumers) == 0 {
		delete(r.entries, key)
	}
	r.mu.Unlock()
}

func (r *remoteInterestRegistry) interested(key entity.RemoteSnapshotKey) bool {
	if r == nil {
		return false
	}
	now := time.Now().UnixNano()
	r.mu.Lock()
	consumers := r.entries[key]
	for sid, expiresAt := range consumers {
		if expiresAt <= now {
			delete(consumers, sid)
			r.total--
			continue
		}
		r.mu.Unlock()
		return true
	}
	if len(consumers) == 0 {
		delete(r.entries, key)
	}
	r.mu.Unlock()
	return false
}

func (r *remoteInterestRegistry) pruneExpiredLocked(now int64) {
	for key, consumers := range r.entries {
		for sid, expiresAt := range consumers {
			if expiresAt <= now {
				delete(consumers, sid)
				r.total--
			}
		}
		if len(consumers) == 0 {
			delete(r.entries, key)
		}
	}
}

type remoteInterestWire struct {
	Release  bool                          `json:"release,omitempty"`
	Interest entity.RemoteSnapshotInterest `json:"interest"`
}

type remoteInterestReplicaStore struct{ mgr *remoteEntityManager }

func (s remoteInterestReplicaStore) ApplyReplica(_ context.Context, env mirror.Envelope) error {
	if s.mgr == nil || s.mgr.remote == nil || len(env.Payload) == 0 {
		return nil
	}
	var wire remoteInterestWire
	if err := json.Unmarshal(env.Payload, &wire); err != nil {
		return err
	}
	if wire.Release {
		s.mgr.remote.interests.release(wire.Interest.Key, wire.Interest.ConsumerSID)
	} else {
		if err := s.mgr.remote.interests.renew(wire.Interest); err != nil {
			return err
		}
	}
	return nil
}

func remoteInterestReplicaKey(interest entity.RemoteSnapshotInterest) int64 {
	h := fnv.New64a()
	var raw [34]byte
	binary.BigEndian.PutUint32(raw[0:4], uint32(interest.ConsumerSID))
	binary.BigEndian.PutUint32(raw[4:8], interest.Key.Tenant)
	binary.BigEndian.PutUint64(raw[8:16], uint64(interest.Key.EntityID))
	binary.BigEndian.PutUint16(raw[16:18], uint16(interest.Key.Kind))
	binary.BigEndian.PutUint32(raw[18:22], interest.Key.Scope)
	binary.BigEndian.PutUint32(raw[22:26], interest.Key.Policy)
	_, _ = h.Write(raw[:26])
	result := int64(h.Sum64() & ((1 << 63) - 1))
	if result == 0 {
		return 1
	}
	return result
}
