package remoteentity

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"github.com/tjbdwanghaibo/cube-core/entity"
	"github.com/tjbdwanghaibo/cube-core/mirror"
	"hash/fnv"
)

const syncTopicRemoteSnapshot = "remote_entity_snapshot"

// remoteSyncer publishes immutable snapshots and renewable interests.
type remoteSyncer struct {
	snapshotRep *mirror.Replicator
	interestRep *mirror.Replicator
	mgr         *remoteEntityManager
}

func newRemoteSyncer(snapshot *mirror.Replicator) *remoteSyncer {
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

type remoteSnapshotReplicaStore struct{ mgr *remoteEntityManager }

func (s remoteSnapshotReplicaStore) ApplyReplica(ctx context.Context, env mirror.Envelope) error {
	if s.mgr == nil || s.mgr.remote == nil || len(env.Payload) == 0 {
		return nil
	}
	var wire remoteSnapshotWire
	if err := json.Unmarshal(env.Payload, &wire); err != nil {
		return err
	}
	if wire.Delete {
		return s.mgr.remote.cache.Delete(ctx, wire.Key)
	}
	err := s.mgr.remote.cache.ApplyUpdate(ctx, wire.Update)
	if errors.Is(err, entity.ErrRemoteSnapshotGap) || errors.Is(err, entity.ErrRemoteSnapshotEpochMismatch) || errors.Is(err, entity.ErrRemoteSnapshotSchemaMismatch) {
		_, _, loadErr := s.mgr.remote.cache.LoadAuthoritative(ctx, wire.Update.Key, entity.RemoteReadMonotonic, wire.Update.StateVersion)
		return loadErr
	}
	return err
}

var _ entity.IRemoteSnapshotPublisher = (*remoteSyncer)(nil)
