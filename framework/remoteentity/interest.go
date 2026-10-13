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

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus/mirror"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

const SyncTopicInterest = "remote_entity_interest"

// ErrInterestQuotaExceeded 表示这个 consumer 在兴趣表里的租约已到配额（
// remote_entity.snapshot_interest_per_consumer）。consumer 本机的兴趣表收到自己的全部续租、按同一配额
// 判定，所以 consumer 在本地就得到这个错误：这个 key 不再有推送，读取在陈旧上限后回源（按需读取）。
// 它包裹 entity.ErrRemoteOverloaded。
var ErrInterestQuotaExceeded = fmt.Errorf("remote_entity: snapshot interest quota for this consumer is exhausted: %w", entity.ErrRemoteOverloaded)

// ErrInterestRegistryFull 表示兴趣表达到每节点的内存上限（remote_entity.snapshot_interest_subs）。
// 配额按 consumer 数选好时不该出现；出现说明 consumer 数 × 配额超过了上限。包裹 entity.ErrRemoteOverloaded。
var ErrInterestRegistryFull = fmt.Errorf("remote_entity: snapshot interest registry is full: %w", entity.ErrRemoteOverloaded)

// interestLease 是兴趣表为每个 consumer 保存的内容：租约何时结束、由哪次续约建立。释放只取消代际不比它新的租约，
// 所以在更新的续约之后到达的释放（线上重排或重放）不会影响那次续约。
//
// released 标记撤销水位：consumer 在 generation 撤销了兴趣，不比它新的续约（在释放之前发出、之后送达）不能让租约
// 复活。水位存活到 expiresAt（释放时间 + 兴趣 TTL）：比释放更旧的续约到那时也已过期，同样会被拒绝。
type interestLease struct {
	expiresAt  int64
	generation uint64
	released   bool
}

// remoteInterestLimits 是兴趣表的容量：PerConsumer 是每个 consumer 的租约配额，Total 是每节点全部
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
// 容量按 consumer 计：一个 consumer 用满自己的配额不会让别的 consumer 被拒；判定只取决于这个 consumer 自己发出的
// 消息，所以 consumer 能在本地预知（按全集群合计时，被拒的 consumer 不会知道，推送会静默停止）。
type remoteInterestRegistry struct {
	mu          sync.Mutex
	entries     map[entity.RemoteSnapshotKey]map[int32]interestLease
	perConsumer map[int32]int // 每个 consumer 的有效租约数（不含撤销水位）
	total       int           // 全部条目（租约与撤销水位）
	quota       int
	maxSubs     int
	fence       time.Duration
	lastLogAt   int64
	// overflow 是表满放不下撤销水位时的退路，每个 consumer 至多一个，不占表容量。
	overflow map[int32]*interestOverflowFence
	// now 是兴趣表读的系统时钟（纳秒）。租约与水位的到期是系统时间（D-L3），这里只为测试留一个注入缝：
	// 生产只经 newRemoteInterestRegistry 取 time.Now，测试替换它来走到期，不加睡眠。
	now func() int64
}

func systemNowNanos() int64 { return time.Now().UnixNano() }

// interestOverflowFence 记一个 consumer 在表满时发出、没能留下撤销水位的 release：代际上限、涉及 key 的
// 指纹位图与到期时刻（最后一次 release + 兴趣 TTL，与撤销水位同一个上界）。到期前，这个 consumer 代际不新于
// generation、key 落在位图里的新租约一律拒绝。位图有碰撞：同一窗口里别的 key 的迟到旧续租也可能被拒，
// 后果与被拒的续租相同（这个 key 暂无推送、读取按陈旧上限回源），下一次续租（代际更新）照常建立。
// 撤销之后发出的续租代际一定更新，不受影响。
type interestOverflowFence struct {
	generation uint64
	expiresAt  int64
	keys       [4]uint64
}

func interestOverflowBit(key entity.RemoteSnapshotKey, sid int32) (word int, mask uint64) {
	bit := uint64(remoteInterestReplicaKey(entity.RemoteSnapshotInterest{ConsumerSID: sid, Key: key})) % 256
	return int(bit / 64), uint64(1) << (bit % 64)
}

// overflowFencedLocked 报告一条新租约是否落在表满时没能留下水位的 release 之后（见 interestOverflowFence）。
func (r *remoteInterestRegistry) overflowFencedLocked(now int64, interest entity.RemoteSnapshotInterest) bool {
	fence := r.overflow[interest.ConsumerSID]
	if fence == nil {
		return false
	}
	if fence.expiresAt <= now {
		delete(r.overflow, interest.ConsumerSID)
		return false
	}
	word, mask := interestOverflowBit(interest.Key, interest.ConsumerSID)
	return interest.Generation <= fence.generation && fence.keys[word]&mask != 0
}

// fenceOverflowLocked 在表满、放不下撤销水位时记下这次 release（见 interestOverflowFence）。
func (r *remoteInterestRegistry) fenceOverflowLocked(now int64, key entity.RemoteSnapshotKey, sid int32, generation uint64) {
	fence := r.overflow[sid]
	if fence == nil || fence.expiresAt <= now {
		fence = &interestOverflowFence{}
		if r.overflow == nil {
			r.overflow = make(map[int32]*interestOverflowFence)
		}
		r.overflow[sid] = fence
	}
	word, mask := interestOverflowBit(key, sid)
	fence.keys[word] |= mask
	fence.generation = max(fence.generation, generation)
	fence.expiresAt = now + r.fence.Nanoseconds()
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
		now:         systemNowNanos,
	}
}

func (r *remoteInterestRegistry) renew(interest entity.RemoteSnapshotInterest) error {
	_, err := r.renewIfNeeded(interest, 0)
	return err
}

// renewIfNeeded 返回调用方是否应发布这次续约。本地读路径传入阈值，缓存命中不会每次读都发一条网络消息；复制应用传 0，
// 保持幂等。
func (r *remoteInterestRegistry) renewIfNeeded(interest entity.RemoteSnapshotInterest, remainingThreshold time.Duration) (bool, error) {
	if r == nil || interest.ConsumerSID == 0 || !interest.Key.Valid() {
		return false, entity.ErrRemoteRejected
	}
	now := r.now()
	if interest.ExpiresAt <= now {
		return false, entity.ErrRemoteRejected
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	current, exists := r.entries[interest.Key][interest.ConsumerSID]
	if exists && current.released && current.expiresAt <= now {
		r.removeLocked(interest.Key, interest.ConsumerSID)
		exists = false
	}
	if exists && !current.released {
		if interest.Generation < current.generation {
			// 比已记录租约更旧的续约（重排或重放）：不能把租约往任何方向移动。
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
	if r.overflowFencedLocked(now, interest) {
		// 同上，只是那次撤销赶上表满、水位记在 overflow 里。
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

// release 撤销一个 consumer 的租约，前提是这次释放不比建立租约的续约旧：陈旧的释放若照删，线上超车的续约会被
// 静默取消，发布方的兴趣过滤随之停止发送 consumer 仍在等的更新。
//
// 带代际的释放会留下撤销水位：否则线上超车了更旧续约的释放找不到可取消的租约，迟到的续约随后会建立一个 consumer
// 已经撤销的租约。表已满且条目不存在时，水位记到该 consumer 的溢出屏障而不是表里。
func (r *remoteInterestRegistry) release(key entity.RemoteSnapshotKey, consumerSID int32, generation uint64) {
	if r == nil {
		return
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	current, exists := r.entries[key][consumerSID]
	if exists && generation < current.generation {
		return
	}
	if !exists && r.total >= r.maxSubs {
		r.pruneExpiredLocked(now)
	}
	if !exists && r.total >= r.maxSubs {
		// 表已满放不下水位。若只撤销不留水位，之后表里空出一格，迟到的旧续租就会复活 consumer 已撤销的租约。
		// 所以记到 overflow，不占表容量。
		r.fenceOverflowLocked(now, key, consumerSID, generation)
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
	now := r.now()
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
	for sid, fence := range r.overflow {
		if fence.expiresAt <= now {
			delete(r.overflow, sid)
		}
	}
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
		return entity.ErrRemoteRejected
	}
	var wire remoteInterestWire
	if err := json.Unmarshal(env.Payload, &wire); err != nil {
		return err
	}
	// renew / release 都是携带完整身份的 Upsert 消息。
	// 在改注册表前绑定 payload，不能用一个订阅的信封操作另一个订阅。
	if env.Op != mirror.OpUpsert || !wire.Interest.Key.Valid() || wire.Interest.ConsumerSID == 0 || wire.Interest.Generation == 0 {
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
