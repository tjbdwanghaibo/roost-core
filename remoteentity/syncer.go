package remoteentity

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	"github.com/tjbdwanghaibo/roost-core/sync/syncbus/mirror"
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
	// PublishedAt 是发布方发出这条消息的时刻（Unix 纳秒，B2 新增，omitempty）。接收方据此丢弃比共享 L2
	// 能担保的窗口更老的快照更新（ApplyReplica）。旧发布者不带它，旧接收方忽略它。
	PublishedAt int64 `json:"published_at,omitempty"`
}

func (s *remoteSyncer) PublishRemoteSnapshot(ctx context.Context, update entity.RemoteSnapshotRecord) error {
	if s == nil || s.snapshotRep == nil {
		return nil
	}
	if s.mgr != nil && s.mgr.remote != nil && !s.mgr.remote.interests.interested(update.Key) {
		return nil
	}
	raw, err := json.Marshal(remoteSnapshotWire{Key: update.Key, Update: update.Clone(), PublishedAt: time.Now().UnixNano()})
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
	raw, err := json.Marshal(remoteSnapshotWire{Delete: true, Key: key, PublishedAt: time.Now().UnixNano()})
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
	if s.historic(wire.PublishedAt, time.Now()) {
		// N05 O5：同步总线的 durable 用 DeliverAll，新 sid 或落后的游标会重放保留期内的历史。共享 L2 对
		// 一次写入（更新的版本或删除墓碑）的记忆只有 snapshot_l2_ttl 那么长；比这更老的快照，L2 的 CAS 已经
		// 无法替它担保——键过期后会接受它，旧版本同时写进 L2 与 L1、所有冷节点都读到。丢掉它只意味着之后
		// 按需读取。窗口取 L2 TTL 的一半，另一半留给跨节点时钟偏差与投递延迟。删除不过滤：带版本删除的
		// 重放无害。
		metrics.IncCounter("remote_entity.snapshot_replica_historic_dropped_total", nil, 1)
		return nil
	}
	err := s.mgr.remote.cache.ApplyUpdate(ctx, wire.Update)
	if errors.Is(err, entity.ErrRemoteSnapshotGap) || errors.Is(err, entity.ErrRemoteSnapshotEpochMismatch) || errors.Is(err, entity.ErrRemoteSnapshotSchemaMismatch) {
		_, _, loadErr := s.mgr.remote.cache.LoadAuthoritative(ctx, wire.Update.Key, entity.RemoteReadMonotonic, wire.Update.StateVersion)
		return loadErr
	}
	return err
}

// historic 报告一条带发布时刻的快照更新是否已老到共享 L2 无法担保（见 ApplyReplica）。没有发布时刻
// （旧发布者）或没有配置 L2 TTL 时不过滤。
func (s SnapshotReplicaStore) historic(publishedAt int64, now time.Time) bool {
	if publishedAt <= 0 || s.mgr == nil || s.mgr.cfg == nil || s.mgr.cfg.SnapshotL2TTL <= 0 {
		return false
	}
	return now.UnixNano()-publishedAt > (s.mgr.cfg.SnapshotL2TTL / 2).Nanoseconds()
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
