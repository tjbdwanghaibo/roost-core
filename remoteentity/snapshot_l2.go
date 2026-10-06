package remoteentity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/cache"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	rediscore "github.com/tjbdwanghaibo/roost-core/redis"
)

// remoteSnapshotL2Lua holds the helpers every L2 script shares.
const remoteSnapshotL2Lua = `
-- Versions, marker epochs and route epochs are uint64 on the Go side. Lua
-- numbers are float64, so tonumber collapses adjacent values above 2^53 and
-- the ordering this CAS exists to enforce silently breaks (RR-20260913-10).
-- Everything ordered is therefore compared as an exact decimal string and
-- stored verbatim; only the TTL, which is small and ordered against nothing,
-- still goes through tonumber.
local function norm(v)
  if not v or v == "" then return "0" end
  local trimmed = string.gsub(v, "^0+", "")
  if trimmed == "" then return "0" end
  return trimmed
end
local function cmp(a, b)
  a, b = norm(a), norm(b)
  if #a ~= #b then if #a < #b then return -1 else return 1 end end
  if a == b then return 0 end
  if a < b then return -1 else return 1 end
end
`

const remoteSnapshotL2CAS = remoteSnapshotL2Lua + `-- A versioned delete leaves a tombstone (deleted_version) instead of nothing:
-- a snapshot not newer than it is the past, whoever writes it
-- (RR-20260913-01 复核, 2026-10-05).
local deleted = redis.call("HGET", KEYS[1], "deleted_version")
if deleted and cmp(ARGV[3], deleted) <= 0 then return 0 end
local oldMarker = redis.call("HGET", KEYS[1], "marker") or "0"
local oldRoute = redis.call("HGET", KEYS[1], "route") or "0"
local oldVersion = redis.call("HGET", KEYS[1], "version") or "0"
local oldChecksum = redis.call("HGET", KEYS[1], "checksum") or ""
local oldSchema = redis.call("HGET", KEYS[1], "schema") or ""
local oldCodec = redis.call("HGET", KEYS[1], "codec") or ""
local markerCmp = cmp(ARGV[1], oldMarker)
local routeCmp = cmp(ARGV[2], oldRoute)
if markerCmp < 0 or routeCmp < 0 then return 0 end
local sameEpoch = markerCmp == 0 and routeCmp == 0
local versionCmp = cmp(ARGV[3], oldVersion)
if sameEpoch and versionCmp < 0 then return 0 end
-- Same version must mean the same VALUE, and the value includes how its bytes
-- are read: checksum covers the payload only, so schema and codec have to be
-- compared too or one version's bytes can change interpretation
-- (RR-20260913-07). The local cache has always compared all three.
if sameEpoch and versionCmp == 0 and oldChecksum ~= "" and
   (oldChecksum ~= ARGV[4] or oldSchema ~= ARGV[7] or oldCodec ~= ARGV[8]) then return -1 end
redis.call("HSET", KEYS[1], "marker", ARGV[1], "route", ARGV[2], "version", ARGV[3],
  "checksum", ARGV[4], "schema", ARGV[7], "codec", ARGV[8], "data", ARGV[5])
if deleted then redis.call("HDEL", KEYS[1], "deleted_version") end
local ttl = tonumber(ARGV[6])
if ttl and ttl > 0 then redis.call("PEXPIRE", KEYS[1], ttl) end
return 1
`

// remoteSnapshotL2DeleteAtVersion deletes the key unless it holds a
// snapshot newer than ARGV[1]. Compared as an exact decimal string like the
// CAS above; a delete is ordered against the versions of one key only, so
// epochs are not part of it (U-0187 复核补修, RR-20260913-01).
//
// The key is not simply removed: it becomes a tombstone holding only
// deleted_version, with the snapshot TTL (ARGV[2], milliseconds). The local
// tombstone of the node that applied the delete fences only that node; another
// node that has not seen the delete yet — its authoritative load read the old
// value first, or a late replica arrived before the delete message — used to
// write the deleted snapshot back into the empty key, and every node with a
// cold L1 then read the deleted entity until the L2 TTL (RR-20260913-01 复核,
// 2026-10-05). An older delete never lowers a newer tombstone. Without a TTL
// the key is dropped as before rather than leaving a tombstone forever.
const remoteSnapshotL2DeleteAtVersion = remoteSnapshotL2Lua + `
local stored = redis.call("HGET", KEYS[1], "version")
if stored and cmp(stored, ARGV[1]) > 0 then return 0 end
local tomb = ARGV[1]
local deleted = redis.call("HGET", KEYS[1], "deleted_version")
if deleted and cmp(deleted, tomb) > 0 then tomb = deleted end
redis.call("DEL", KEYS[1])
local ttl = tonumber(ARGV[2])
if ttl and ttl > 0 then
  redis.call("HSET", KEYS[1], "deleted_version", tomb)
  redis.call("PEXPIRE", KEYS[1], ttl)
end
return 1
`

// remoteSnapshotL2Store is the shared cache layer. Its comparison and write
// happen in one Redis script, so a delayed publisher cannot overwrite a newer
// ownership epoch or state version.
type remoteSnapshotL2Store struct {
	redis remoteSnapshotRedis
	ttl   time.Duration
	// keyPrefix 为空时键与旧版本逐字相同（remote_entity:snapshot:…）；非空时键为 "<keyPrefix>:remote_entity:snapshot:…"，
	// 让共用一个 Redis db 的多个部署不共享 L2 快照（RR-20260927-17）。
	keyPrefix string
	// tombstoneWait 是写墓碑之后的 WAIT（O-M6-3）；零值不等。
	tombstoneWait tombstoneWait
}

// tombstoneWait 是 DeleteAtVersion 写墓碑之后的 WAIT 设置与计数（O-M6-3，
// docs/feature/MIRROR-M6-OBSERVATIONS-2026-10-06.md §3）。
//
// 为什么只对墓碑：Redis 异步复制，切主时还没复制到副本的写会丢。快照丢了只是回到更旧的版本，L2 的 CAS 与
// 陈旧上限照样保证不回退；墓碑丢了，L1 空的新读者会在陈旧上限内把已删除的实体读成存在（O-M6-3）。删除很少，
// 每次多等一次副本确认的代价小。WAIT 只缩小窗口：副本在确认前断开或被暂停时仍会丢，结果计为 short /
// no_replicas，可观测。
type tombstoneWait struct {
	replicas int
	timeout  time.Duration

	confirmed, short, noReplicas, failed, skipped atomic.Uint64
	lastWarnAt                                    atomic.Int64
	noReplicaOnce                                 sync.Once
}

// SnapshotL2TombstoneWaitStats 是墓碑 WAIT 的累计结果（诊断与测试用）。
type SnapshotL2TombstoneWaitStats struct {
	// Confirmed：副本确认数达到配置。Short：WAIT 超时、确认不足。NoReplicas：主节点没有连着的副本，没等。
	// Failed：ROLE / WAIT 出错（脚本已执行、复制未知）。Skipped：客户端不支持同连接 WAIT 或 Cluster 拓扑刚变，没等。
	Confirmed, Short, NoReplicas, Failed, Skipped uint64
}

type remoteSnapshotL2Value struct {
	Key          entity.RemoteSnapshotKey `json:"key"`
	StateVersion uint64                   `json:"state_version"`
	BaseVersion  uint64                   `json:"base_version"`
	MarkerEpoch  uint64                   `json:"marker_epoch"`
	RouteEpoch   uint64                   `json:"route_epoch"`
	Schema       uint32                   `json:"schema"`
	Codec        uint16                   `json:"codec"`
	Checksum     entity.RemoteChecksum    `json:"checksum"`
	Full         bool                     `json:"full"`
	PublishedAt  int64                    `json:"published_at"`
	ExpiresAt    int64                    `json:"expires_at"`
	Data         []byte                   `json:"data"`
}

type remoteSnapshotRedis interface {
	HGet(context.Context, string, string) ([]byte, error)
	Eval(context.Context, string, []string, ...any) (any, error)
	Del(context.Context, ...string) (int64, error)
}

func NewSnapshotL2Store(redis remoteSnapshotRedis, ttl time.Duration) *remoteSnapshotL2Store {
	return &remoteSnapshotL2Store{redis: redis, ttl: ttl}
}

// NewSnapshotL2StoreWithKeyPrefix 与 NewSnapshotL2Store 相同，另给全部 L2 快照键加部署前缀（Config.SnapshotL2KeyPrefix）。
// 前缀为空时等同 NewSnapshotL2Store，键不变。前缀先经 ValidateSnapshotL2KeyPrefix 校验。
func NewSnapshotL2StoreWithKeyPrefix(redis remoteSnapshotRedis, ttl time.Duration, prefix string) (*remoteSnapshotL2Store, error) {
	if err := ValidateSnapshotL2KeyPrefix(prefix); err != nil {
		return nil, err
	}
	return &remoteSnapshotL2Store{redis: redis, ttl: ttl, keyPrefix: prefix}, nil
}

// NewSnapshotL2StoreFromConfig 按 Config 建 L2 store：SnapshotL2TTL、SnapshotL2KeyPrefix 与墓碑 WAIT
// （SnapshotL2TombstoneWaitReplicas / Timeout，O-M6-3）。Assemble 与 kit 的只读 RemoteMirrorMod 都用它，
// 规则只在这里检查一次。
func NewSnapshotL2StoreFromConfig(redis remoteSnapshotRedis, cfg *Config) (*remoteSnapshotL2Store, error) {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	if err := ValidateSnapshotL2TombstoneWait(cfg.SnapshotL2TombstoneWaitReplicas, cfg.SnapshotL2TombstoneWaitTimeout); err != nil {
		return nil, err
	}
	store, err := NewSnapshotL2StoreWithKeyPrefix(redis, cfg.SnapshotL2TTL, cfg.SnapshotL2KeyPrefix)
	if err != nil {
		return nil, err
	}
	store.tombstoneWait.replicas = cfg.SnapshotL2TombstoneWaitReplicas
	store.tombstoneWait.timeout = cfg.SnapshotL2TombstoneWaitTimeout
	return store, nil
}

// MaxSnapshotL2TombstoneWaitTimeout 是墓碑 WAIT 超时的上限：调用方（owner 的提交后发布、只读方 apply
// 删除消息）最多多等这么久；它也必须低于 Redis 客户端的读超时（缺省 3s），否则客户端先超时，WAIT 记为出错。
const MaxSnapshotL2TombstoneWaitTimeout = time.Second

// ValidateSnapshotL2TombstoneWait 校验墓碑 WAIT 的设置：副本数不能为负；要等副本时超时必须为正（Redis 的
// WAIT … 0 是永久阻塞，删除会卡住），且不超过 MaxSnapshotL2TombstoneWaitTimeout。
func ValidateSnapshotL2TombstoneWait(replicas int, timeout time.Duration) error {
	if replicas < 0 {
		return fmt.Errorf("remote_entity: snapshot L2 tombstone wait replicas must not be negative, got %d", replicas)
	}
	if replicas > 0 && (timeout <= 0 || timeout > MaxSnapshotL2TombstoneWaitTimeout) {
		return fmt.Errorf("remote_entity: snapshot L2 tombstone wait timeout must be in (0, %v] when waiting for %d replica(s), got %v", MaxSnapshotL2TombstoneWaitTimeout, replicas, timeout)
	}
	return nil
}

// TombstoneWaitStats 返回墓碑 WAIT 的累计结果。
func (s *remoteSnapshotL2Store) TombstoneWaitStats() SnapshotL2TombstoneWaitStats {
	if s == nil {
		return SnapshotL2TombstoneWaitStats{}
	}
	w := &s.tombstoneWait
	return SnapshotL2TombstoneWaitStats{Confirmed: w.confirmed.Load(), Short: w.short.Load(), NoReplicas: w.noReplicas.Load(), Failed: w.failed.Load(), Skipped: w.skipped.Load()}
}

// ValidateSnapshotL2KeyPrefix 校验 L2 快照键前缀：可以为空（键不变）；非空时不能有首尾空白或内部空白，
// 也不能含 Redis Cluster hash tag 的花括号——L2 脚本只操作单键，不需要同槽，带 hash tag 会把全部快照键钉在同一个槽上。
func ValidateSnapshotL2KeyPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if strings.TrimSpace(prefix) != prefix || strings.ContainsAny(prefix, " \t\r\n") {
		return fmt.Errorf("remote_entity: snapshot L2 key prefix %q must not contain whitespace", prefix)
	}
	if strings.ContainsAny(prefix, "{}") {
		return fmt.Errorf("remote_entity: snapshot L2 key prefix %q must not contain a Redis Cluster hash tag; L2 scripts touch one key and a tag would pin every snapshot key to one slot", prefix)
	}
	return nil
}

func (s *remoteSnapshotL2Store) Get(ctx context.Context, key entity.RemoteSnapshotKey) (entity.RemoteSnapshotEnvelope, bool, error) {
	if s == nil || s.redis == nil || !key.Valid() {
		return entity.RemoteSnapshotEnvelope{}, false, nil
	}
	raw, err := s.redis.HGet(ctx, s.key(key), "data")
	if err != nil {
		if errors.Is(err, rediscore.ErrNil) {
			return entity.RemoteSnapshotEnvelope{}, false, nil
		}
		return entity.RemoteSnapshotEnvelope{}, false, err
	}
	var wire remoteSnapshotL2Value
	if err := json.Unmarshal(raw, &wire); err != nil {
		return entity.RemoteSnapshotEnvelope{}, false, err
	}
	value := entity.RemoteSnapshotEnvelope{
		Key: wire.Key, StateVersion: wire.StateVersion, BaseVersion: wire.BaseVersion,
		MarkerEpoch: wire.MarkerEpoch, RouteEpoch: wire.RouteEpoch, Schema: wire.Schema,
		Codec: wire.Codec, Checksum: wire.Checksum, Full: wire.Full,
		PublishedAt: wire.PublishedAt, ExpiresAt: wire.ExpiresAt,
		Payload: entity.TakeFrozenRemoteSnapshotPayload(wire.Data),
	}
	if value.Key != key {
		return entity.RemoteSnapshotEnvelope{}, false, fmt.Errorf("remote_entity: L2 snapshot key mismatch")
	}
	if err := value.Valid(); err != nil {
		return entity.RemoteSnapshotEnvelope{}, false, fmt.Errorf("remote_entity: invalid L2 snapshot: %w", err)
	}
	return value.Clone(), true, nil
}

func (s *remoteSnapshotL2Store) Set(ctx context.Context, value entity.RemoteSnapshotEnvelope) error {
	if s == nil || s.redis == nil {
		return nil
	}
	data := value.Payload.BytesCopy()
	value.Checksum = entity.RemoteSnapshotChecksum(data)
	if err := value.Valid(); err != nil {
		return err
	}
	raw, err := json.Marshal(remoteSnapshotL2Value{
		Key: value.Key, StateVersion: value.StateVersion, BaseVersion: value.BaseVersion,
		MarkerEpoch: value.MarkerEpoch, RouteEpoch: value.RouteEpoch, Schema: value.Schema,
		Codec: value.Codec, Checksum: value.Checksum, Full: value.Full,
		PublishedAt: value.PublishedAt, ExpiresAt: value.ExpiresAt, Data: data,
	})
	if err != nil {
		return err
	}
	ttlMillis := s.ttl.Milliseconds()
	// RemoteChecksum 是 BSON 专用命名类型，Redis 不接受它作为脚本参数。
	// 转为精确十进制串，沿用既有 checksum 格式并保留完整 uint64 范围。
	result, err := s.redis.Eval(ctx, remoteSnapshotL2CAS, []string{s.key(value.Key)},
		value.MarkerEpoch, value.RouteEpoch, value.StateVersion, strconv.FormatUint(uint64(value.Checksum), 10), raw, ttlMillis,
		value.Schema, value.Codec)
	if err != nil {
		return err
	}
	accepted, parseErr := strconv.ParseInt(fmt.Sprint(result), 10, 64)
	if parseErr != nil {
		return fmt.Errorf("remote_entity: invalid L2 CAS response %q: %w", fmt.Sprint(result), parseErr)
	}
	if accepted == 0 {
		// RR-20261005-NC-130：CAS 落败（L2 已有更新的版本或更新的 epoch）就是被判 stale 的写，
		// 按 cache.Store 约定返回 ErrStaleWrite。之前返回 nil，ReadThroughStore.Set 把它当成写入成功
		// 照常写 L1，L1 冷的节点于是停在比 L2 更旧的快照上。
		return fmt.Errorf("%w: L2 holds a newer snapshot", cache.ErrStaleWrite)
	}
	if accepted < 0 {
		return fmt.Errorf("%w: L2 same version has different content", entity.ErrRemoteVersionConflict)
	}
	return nil
}

func (s *remoteSnapshotL2Store) Delete(ctx context.Context, key entity.RemoteSnapshotKey) error {
	if s == nil || s.redis == nil || !key.Valid() {
		return nil
	}
	_, err := s.redis.Del(ctx, s.key(key))
	return err
}

// DeleteAtVersion implements entity.RemoteSnapshotVersionedDeleter: the
// comparison and the delete run in one script, so a newer snapshot that lands
// between "read version" and "DEL" cannot be lost. A stored snapshot newer
// than version refuses the delete; that is reported as cache.ErrStaleWrite so
// the cache adopts the newer snapshot (B2). The script is idempotent — the
// tombstone only ever rises — so re-sending it after an unknown result is safe.
//
// O-M6-3：配置了墓碑 WAIT 且客户端支持同连接 WAIT（fredis.ReplicatedEvaler）时，脚本之后在同一连接上等副本
// 确认。WAIT 的结果不改变返回值——墓碑已经写在主节点上，报错只会把一次已提交的删除变成“结果未知”；
// 确认不足、出错都只计数与记日志（recordTombstoneWait）。调用方最多多等 timeout。
func (s *remoteSnapshotL2Store) DeleteAtVersion(ctx context.Context, key entity.RemoteSnapshotKey, version uint64) error {
	if s == nil || s.redis == nil || !key.Valid() {
		return nil
	}
	redisKey := s.key(key)
	args := []any{strconv.FormatUint(version, 10), s.ttl.Milliseconds()}
	var result any
	if replicated, ok := s.redis.(rediscore.ReplicatedEvaler); ok && s.tombstoneWait.replicas > 0 {
		outcome, err := replicated.EvalReplicated(ctx, remoteSnapshotL2DeleteAtVersion, []string{redisKey}, s.tombstoneWait.replicas, s.tombstoneWait.timeout, args...)
		if err != nil {
			return err
		}
		result = outcome.Result
		s.recordTombstoneWait(redisKey, outcome)
	} else {
		var err error
		if result, err = s.redis.Eval(ctx, remoteSnapshotL2DeleteAtVersion, []string{redisKey}, args...); err != nil {
			return err
		}
		if s.tombstoneWait.replicas > 0 {
			s.recordTombstoneWait(redisKey, rediscore.ReplicatedEvalResult{Result: result, Skipped: rediscore.ReplicatedSkipUnsupported})
		}
	}
	if fmt.Sprint(result) == "0" {
		return fmt.Errorf("%w: L2 holds a snapshot newer than the delete", cache.ErrStaleWrite)
	}
	return nil
}

var _ entity.RemoteSnapshotVersionedDeleter = (*remoteSnapshotL2Store)(nil)

// recordTombstoneWait 计墓碑 WAIT 的结果（指标 remote_entity.snapshot_l2_tombstone_wait_total{result}）：
// short / error 限频 Warn（每个 store 10s 一条）；no_replicas 首次一条 Info（单机开发环境属正常）。
func (s *remoteSnapshotL2Store) recordTombstoneWait(redisKey string, outcome rediscore.ReplicatedEvalResult) {
	w := &s.tombstoneWait
	var label string
	switch {
	case outcome.WaitErr != nil:
		label = "error"
		w.failed.Add(1)
	case outcome.Waited && outcome.Replicas >= int64(w.replicas):
		label = "confirmed"
		w.confirmed.Add(1)
	case outcome.Waited:
		label = "short"
		w.short.Add(1)
	case outcome.Skipped == rediscore.ReplicatedSkipNoReplicas:
		label = "no_replicas"
		w.noReplicas.Add(1)
		w.noReplicaOnce.Do(func() {
			slog.Info("remote_entity: the Redis primary has no connected replica; snapshot tombstones are not waited for (expected on a single-node development Redis)",
				"key", redisKey, "wait_replicas", w.replicas)
		})
	default:
		label = "skipped"
		w.skipped.Add(1)
	}
	metrics.IncCounter("remote_entity.snapshot_l2_tombstone_wait_total", metrics.Labels{"result": label}, 1)
	if label != "short" && label != "error" {
		return
	}
	now := time.Now().UnixNano()
	last := w.lastWarnAt.Load()
	if now-last < (10*time.Second).Nanoseconds() || !w.lastWarnAt.CompareAndSwap(last, now) {
		return
	}
	slog.Warn("remote_entity: snapshot tombstone written on the Redis primary but not confirmed by its replicas; a failover now may bring the deleted snapshot back until cached_max_staleness",
		"key", redisKey, "result", label, "acked_replicas", outcome.Replicas, "wait_replicas", w.replicas, "wait_timeout", w.timeout, "err", outcome.WaitErr)
}

// key 是本 store 的 Redis 键：无前缀时与 remoteSnapshotL2Key 逐字相同。
func (s *remoteSnapshotL2Store) key(key entity.RemoteSnapshotKey) string {
	if s.keyPrefix == "" {
		return remoteSnapshotL2Key(key)
	}
	return s.keyPrefix + ":" + remoteSnapshotL2Key(key)
}

func remoteSnapshotL2Key(key entity.RemoteSnapshotKey) string {
	return fmt.Sprintf("remote_entity:snapshot:%d:%d:%d:%d:%d", key.Tenant, key.Kind, key.EntityID, key.Scope, key.Policy)
}
