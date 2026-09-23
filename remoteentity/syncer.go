package remoteentity

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/syncbus/mirror"
	"hash/fnv"
)

const SyncTopicSnapshot = "remote_entity_snapshot"

// remoteSyncer publishes immutable snapshots and renewable interests.
type remoteSyncer struct {
	snapshotRep *mirror.Replicator
	interestRep *mirror.Replicator
	mgr         *Manager
}

func NewSyncer(snapshot *mirror.Replicator) *remoteSyncer {
	return &remoteSyncer{snapshotRep: snapshot}
}

type remoteSnapshotWire struct {
	Delete bool                        `json:"delete,omitempty"`
	Key    entity.RemoteSnapshotKey    `json:"key"`
	Update entity.RemoteSnapshotRecord `json:"update,omitempty"`
}

func (s *remoteSyncer) PublishRemoteSnapshot(ctx context.Context, update entity.RemoteSnapshotRecord) error {
	if s == nil || s.snapshotRep == nil {
		return nil
	}
	if s.mgr != nil && s.mgr.remote != nil && !s.mgr.remote.interests.interested(update.Key) {
		return nil
	}
	raw, err := json.Marshal(remoteSnapshotWire{Key: update.Key, Update: update.Clone()})
	if err != nil {
		return err
	}
	return s.snapshotRep.Publish(ctx, mirror.Envelope{Key: remoteSnapshotReplicaKey(update.Key), Version: int64(update.StateVersion), Op: mirror.OpUpsert, Payload: raw})
}

func (s *remoteSyncer) PublishRemoteInterest(ctx context.Context, interest entity.RemoteSnapshotInterest, release bool) error {
	if s == nil || s.interestRep == nil {
		return nil
	}
	raw, err := json.Marshal(remoteInterestWire{Release: release, Interest: interest})
	if err != nil {
		return err
	}
	return s.interestRep.Publish(ctx, mirror.Envelope{
		Key: remoteInterestReplicaKey(interest), Version: interest.ExpiresAt,
		Op: mirror.OpUpsert, Payload: raw,
	})
}

func (s *remoteSyncer) DeleteRemoteSnapshot(ctx context.Context, key entity.RemoteSnapshotKey, version uint64) error {
	if s == nil || s.snapshotRep == nil {
		return nil
	}
	raw, err := json.Marshal(remoteSnapshotWire{Delete: true, Key: key})
	if err != nil {
		return err
	}
	return s.snapshotRep.Publish(ctx, mirror.Envelope{Key: remoteSnapshotReplicaKey(key), Version: int64(version), Op: mirror.OpUpsert, Payload: raw})
}

// remoteSnapshotReplicaKey defines the ordering/deduplication domain. Every
// independently versioned tenant/entity/kind/scope/policy snapshot needs its
// own key; using EntityID alone drops sibling snapshots at the same version.
func remoteSnapshotReplicaKey(key entity.RemoteSnapshotKey) int64 {
	h := fnv.New64a()
	var raw [22]byte
	binary.BigEndian.PutUint32(raw[0:4], key.Tenant)
	binary.BigEndian.PutUint64(raw[4:12], uint64(key.EntityID))
	binary.BigEndian.PutUint16(raw[12:14], uint16(key.Kind))
	binary.BigEndian.PutUint32(raw[14:18], key.Scope)
	binary.BigEndian.PutUint32(raw[18:22], key.Policy)
	_, _ = h.Write(raw[:])
	result := int64(h.Sum64() & ((1 << 63) - 1))
	if result == 0 {
		return 1
	}
	return result
}

type SnapshotReplicaStore struct{ mgr *Manager }

func (s SnapshotReplicaStore) ApplyReplica(ctx context.Context, env mirror.Envelope) error {
	if s.mgr == nil || s.mgr.remote == nil || len(env.Payload) == 0 {
		return nil
	}
	var wire remoteSnapshotWire
	if err := json.Unmarshal(env.Payload, &wire); err != nil {
		return err
	}
	if err := validateSnapshotWireIdentity(env, wire); err != nil {
		return err
	}
	if wire.Delete {
		// The delete carries the version of the commit that removed the
		// snapshot (DeleteRemoteSnapshot publishes it as env.Version). Apply
		// it at that version so it neither clears a newer snapshot delivered
		// first nor lets an older one delivered later resurrect the key
		// (U-0187, RR-20260913-01). A version-less delete stays a plain
		// invalidation for compatibility with older publishers.
		if env.Version <= 0 {
			return s.mgr.remote.cache.Delete(ctx, wire.Key)
		}
		return s.mgr.remote.cache.DeleteAtVersion(ctx, wire.Key, uint64(env.Version))
	}
	err := s.mgr.remote.cache.ApplyUpdate(ctx, wire.Update)
	if errors.Is(err, entity.ErrRemoteSnapshotGap) || errors.Is(err, entity.ErrRemoteSnapshotEpochMismatch) || errors.Is(err, entity.ErrRemoteSnapshotSchemaMismatch) {
		_, _, loadErr := s.mgr.remote.cache.LoadAuthoritative(ctx, wire.Update.Key, entity.RemoteReadMonotonic, wire.Update.StateVersion)
		return loadErr
	}
	return err
}

// validateSnapshotWireIdentity binds the payload's own identity to the
// envelope that routed it.
//
// The generic Replicator checks the outer SyncMsg against mirror.Envelope, but
// this third layer — wire.Key, wire.Update.Key and Update.StateVersion — was
// never cross-checked against either. A message could therefore declare one
// key for routing and ordering and write a different view: changing only
// Update.Key.Scope in the payload applied to the other scope's cache and
// returned nil (RR-20260913-03). Ordering, deduplication and the write must
// all name the same thing or the message is malformed.
//
// This is protocol integrity, not authorization: who may publish on the
// internal topic stays a transport and service-identity question.
func validateSnapshotWireIdentity(env mirror.Envelope, wire remoteSnapshotWire) error {
	if !wire.Key.Valid() {
		return fmt.Errorf("remote_entity: snapshot message has an invalid key")
	}
	if env.Key != remoteSnapshotReplicaKey(wire.Key) {
		return fmt.Errorf("remote_entity: snapshot message key %d does not match its payload", env.Key)
	}
	if wire.Delete {
		return nil
	}
	if wire.Update.Key != wire.Key {
		return fmt.Errorf("remote_entity: snapshot payload key does not match the message key")
	}
	if env.Version < 0 || uint64(env.Version) != wire.Update.StateVersion {
		return fmt.Errorf("remote_entity: snapshot message version %d does not match payload version %d",
			env.Version, wire.Update.StateVersion)
	}
	return nil
}

var _ entity.IRemoteSnapshotPublisher = (*remoteSyncer)(nil)
