package remoteentity

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	"github.com/tjbdwanghaibo/roost-core/sync/syncbus/mirror"
)

const SyncTopicInterest = "remote_entity_interest"

// ErrInterestQuotaExceeded 表示这个 consumer 在兴趣表里的租约已到配额（O4，
// remote_entity.snapshot_interest_per_consumer）。consumer 本机的兴趣表收到自己的全部续租、按同一配额
// 判定，所以 consumer 在本地就得到这个错误：这个 key 不再有推送，读取在陈旧上限后回源（按需读取）。
// 它包裹 entity.ErrRemoteOverloaded。
var ErrInterestQuotaExceeded = fmt.Errorf("remote_entity: snapshot interest quota for this consumer is exhausted: %w", entity.ErrRemoteOverloaded)

// ErrInterestRegistryFull 表示兴趣表达到每节点的内存上限（remote_entity.snapshot_interest_subs）。
// 配额按 consumer 数选好时不该出现；出现说明 consumer 数 × 配额超过了上限。包裹 entity.ErrRemoteOverloaded。
var ErrInterestRegistryFull = fmt.Errorf("remote_entity: snapshot interest registry is full: %w", entity.ErrRemoteOverloaded)

// interestLease is what the registry keeps per consumer: when the lease ends
// and which renewal established it. A release only cancels a lease whose
// generation it is not older than, so a release that arrives after a newer
// renewal — reordered on the wire, or replayed — leaves that renewal alone.
//
// released marks a release watermark (Mirror 第 4 步): the consumer withdrew
// its interest at generation, and a renewal not newer than that — issued
// before the release, delivered after it — must not bring the lease back.
// The watermark lives until expiresAt (release time + interest TTL): any
// renewal older than the release has expired by then and is refused anyway.
type interestLease struct {
	expiresAt  int64
	generation uint64
	released   bool
}

// remoteInterestLimits 是兴趣表的容量（O4）：PerConsumer 是每个 consumer 的租约配额，Total 是每节点全部
// 条目（租约与撤销水位）的内存上限，ReleaseFence 是撤销水位的存活时间（兴趣 TTL）。零值取缺省。
type remoteInterestLimits struct {
	PerConsumer  int
	Total        int
	ReleaseFence time.Duration
}

const (
	defaultInterestTotal        = 262144
	defaultInterestReleaseFence = 30 * time.Second
	// interestQuotaShare：配额缺省为总上限的 1/16，即 16 个 consumer 节点各自用满也放得下。
	interestQuotaShare = 16
)

// remoteInterestRegistry 是全集群兴趣表的本节点副本：兴趣主题是广播，每个节点都收到所有 consumer 的
// 续租与撤销。owner 用它决定发布哪些 key；consumer 本机也用它，对自己的续租按同一配额先判定一次。
//
// 容量按 consumer 计（O4，docs/feature/MIRROR-STEP-4-AND-O4-2026-10-06.md）：一个 consumer 用满自己的
// 配额不会让别的 consumer 被拒；判定只取决于这个 consumer 自己发出的消息，所以 consumer 能在本地预知。
// 之前按全集群合计，被拒的 consumer 不知道，推送静默停止。
type remoteInterestRegistry struct {
	mu          sync.Mutex
	entries     map[entity.RemoteSnapshotKey]map[int32]interestLease
	perConsumer map[int32]int // 每个 consumer 的有效租约数（不含撤销水位）
	total       int           // 全部条目（租约与撤销水位）
	quota       int
	maxSubs     int
	fence       time.Duration
	lastLogAt   int64
}

func newRemoteInterestRegistry(limits remoteInterestLimits) *remoteInterestRegistry {
	if limits.Total <= 0 {
		limits.Total = defaultInterestTotal
	}
	if limits.PerConsumer <= 0 {
		limits.PerConsumer = max(1, limits.Total/interestQuotaShare)
	}
	if limits.ReleaseFence <= 0 {
		limits.ReleaseFence = defaultInterestReleaseFence
	}
	return &remoteInterestRegistry{
		entries:     make(map[entity.RemoteSnapshotKey]map[int32]interestLease),
		perConsumer: make(map[int32]int),
		quota:       limits.PerConsumer,
		maxSubs:     limits.Total,
		fence:       limits.ReleaseFence,
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
	current, exists := r.entries[interest.Key][interest.ConsumerSID]
	if exists && current.released && current.expiresAt <= now {
		r.removeLocked(interest.Key, interest.ConsumerSID)
		exists = false
	}
	if exists && !current.released {
		if interest.Generation < current.generation {
			// A renewal older than the lease on record: reordered or
			// replayed. It must not move the lease in either direction.
			return false, nil
		}
		if remainingThreshold > 0 && current.expiresAt-now > remainingThreshold.Nanoseconds() {
			return false, nil
		}
		if interest.ExpiresAt > current.expiresAt || interest.Generation > current.generation {
			r.entries[interest.Key][interest.ConsumerSID] = interestLease{
				expiresAt:  max(interest.ExpiresAt, current.expiresAt),
				generation: interest.Generation,
			}
		}
		return true, nil
	}
	if exists && interest.Generation <= current.generation {
		// 撤销水位之前发出的续租迟到了：consumer 已经撤销，不能把租约复活。
		return false, nil
	}
	// 新租约（或撤销水位之后的续租）：先按这个 consumer 的配额，再按每节点的内存上限。
	if r.perConsumer[interest.ConsumerSID] >= r.quota {
		r.pruneExpiredLocked(now)
	}
	if r.perConsumer[interest.ConsumerSID] >= r.quota {
		return false, r.rejectLocked(now, interest, "consumer_quota", ErrInterestQuotaExceeded)
	}
	if !exists && r.total >= r.maxSubs {
		r.pruneExpiredLocked(now)
	}
	if !exists && r.total >= r.maxSubs {
		return false, r.rejectLocked(now, interest, "registry_full", ErrInterestRegistryFull)
	}
	r.setLocked(interest.Key, interest.ConsumerSID, interestLease{expiresAt: interest.ExpiresAt, generation: interest.Generation})
	return true, nil
}

// rejectLocked 计数并限频记日志（每个兴趣表每 10 秒至多一条），返回可识别的错误。
func (r *remoteInterestRegistry) rejectLocked(now int64, interest entity.RemoteSnapshotInterest, reason string, err error) error {
	metrics.IncCounter("remote_entity.remote.interest_rejected_total", metrics.Labels{"reason": reason}, 1)
	if now-r.lastLogAt >= (10 * time.Second).Nanoseconds() {
		r.lastLogAt = now
		slog.Warn("remote_entity: snapshot interest refused; the consumer gets no push for this key and reads it on demand",
			"reason", reason, "consumer_sid", interest.ConsumerSID, "consumer_leases", r.perConsumer[interest.ConsumerSID],
			"quota", r.quota, "registry_entries", r.total, "registry_limit", r.maxSubs)
	}
	return err
}

// setLocked 写一个条目并维护计数；调用方持有 mu。
func (r *remoteInterestRegistry) setLocked(key entity.RemoteSnapshotKey, sid int32, lease interestLease) {
	consumers := r.entries[key]
	if consumers == nil {
		consumers = make(map[int32]interestLease)
		r.entries[key] = consumers
	}
	old, exists := consumers[sid]
	if !exists {
		r.total++
	} else if !old.released {
		r.perConsumer[sid]--
	}
	if !lease.released {
		r.perConsumer[sid]++
	}
	consumers[sid] = lease
	if r.perConsumer[sid] == 0 {
		delete(r.perConsumer, sid)
	}
}

// removeLocked 删一个条目并维护计数；调用方持有 mu。
func (r *remoteInterestRegistry) removeLocked(key entity.RemoteSnapshotKey, sid int32) {
	consumers := r.entries[key]
	old, exists := consumers[sid]
	if !exists {
		return
	}
	delete(consumers, sid)
	r.total--
	if !old.released {
		if r.perConsumer[sid]--; r.perConsumer[sid] <= 0 {
			delete(r.perConsumer, sid)
		}
	}
	if len(consumers) == 0 {
		delete(r.entries, key)
	}
}

// release withdraws one consumer's lease, but only if the release is not
// older than the renewal that established it. A stale release used to delete
// whatever was there, so a renewal that overtook it on the wire was silently
// cancelled and the publisher's interest filter stopped sending updates the
// consumer still expected (RR-20260913-02).
//
// A release with a generation leaves a watermark (Mirror 第 4 步): before it,
// a release that overtook an older renewal on the wire found nothing to
// cancel, and the late renewal then created a lease the consumer had already
// withdrawn. Generation 0 is the legacy publisher and leaves none.
func (r *remoteInterestRegistry) release(key entity.RemoteSnapshotKey, consumerSID int32, generation uint64) {
	if r == nil {
		return
	}
	now := time.Now().UnixNano()
	r.mu.Lock()
	defer r.mu.Unlock()
	current, exists := r.entries[key][consumerSID]
	if exists && generation < current.generation {
		return
	}
	if generation == 0 || (!exists && r.total >= r.maxSubs) {
		// 旧发布者没有代际，或表已满放不下水位：只撤销（之前的行为）。
		r.removeLocked(key, consumerSID)
		return
	}
	r.setLocked(key, consumerSID, interestLease{expiresAt: now + r.fence.Nanoseconds(), generation: generation, released: true})
}

// drop 撤销本节点自己的租约而不留水位（本地回滚、清理过期）：没有消息被重排，不需要水位。
func (r *remoteInterestRegistry) drop(key entity.RemoteSnapshotKey, consumerSID int32, generation uint64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if current, exists := r.entries[key][consumerSID]; exists && generation >= current.generation {
		r.removeLocked(key, consumerSID)
	}
}

func (r *remoteInterestRegistry) interested(key entity.RemoteSnapshotKey) bool {
	if r == nil {
		return false
	}
	now := time.Now().UnixNano()
	r.mu.Lock()
	defer r.mu.Unlock()
	for sid, lease := range r.entries[key] {
		if lease.expiresAt <= now {
			r.removeLocked(key, sid)
			continue
		}
		if !lease.released {
			return true
		}
	}
	return false
}

// consumerLeases 返回一个 consumer 当前的有效租约数（测试与诊断用）。
func (r *remoteInterestRegistry) consumerLeases(sid int32) int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.perConsumer[sid]
}

func (r *remoteInterestRegistry) pruneExpiredLocked(now int64) {
	for key, consumers := range r.entries {
		for sid, lease := range consumers {
			if lease.expiresAt <= now {
				r.removeLocked(key, sid)
			}
		}
	}
}

type remoteInterestWire struct {
	Release  bool                          `json:"release,omitempty"`
	Interest entity.RemoteSnapshotInterest `json:"interest"`
}

// InterestReplicaStore 把兴趣消息落到 SnapshotClient 的全集群兴趣表。
type InterestReplicaStore struct{ client *SnapshotClient }

func (s InterestReplicaStore) ApplyReplica(_ context.Context, env mirror.Envelope) error {
	if s.client == nil || len(env.Payload) == 0 {
		return nil
	}
	var wire remoteInterestWire
	if err := json.Unmarshal(env.Payload, &wire); err != nil {
		return err
	}
	// RR-20261005-NC-34：renew/release 都是携带完整身份的 Upsert 消息。
	// 在改注册表前绑定 payload，不能用一个订阅的信封操作另一个订阅。
	if env.Op != mirror.OpUpsert || !wire.Interest.Key.Valid() || wire.Interest.ConsumerSID == 0 {
		return fmt.Errorf("remote_entity: interest message has invalid operation or identity")
	}
	if env.Key != remoteInterestReplicaKey(wire.Interest) || env.Version != wire.Interest.ExpiresAt {
		return fmt.Errorf("remote_entity: interest message identity does not match its payload")
	}
	if wire.Release {
		s.client.interests.release(wire.Interest.Key, wire.Interest.ConsumerSID, wire.Interest.Generation)
	} else {
		if err := s.client.interests.renew(wire.Interest); err != nil {
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
