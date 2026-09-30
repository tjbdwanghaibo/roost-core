package remoteentity

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/entity"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

const (
	ownershipClaimScript = `
local current = redis.call("HGET", KEYS[1], ARGV[1])
if current then
  local _, owner = string.match(current, "^(%a+):(-?%d+):(%d+):(%d+)$")
  if owner and tonumber(owner) == tonumber(ARGV[2]) then return current end
  return ""
end
local value = "local:" .. ARGV[2] .. ":1:1"
redis.call("HSET", KEYS[1], ARGV[1], value)
return value
`
	// ownershipDecimalInc adds one to a decimal string without ever turning it
	// into a Lua number.
	//
	// The scripts used to write `.. ((tonumber(x) or 0) + 1)`, which
	// concatenates a Lua NUMBER, and Lua 5.1 renders numbers with "%.14g":
	// 10^14 came out as "1e+14". parseMarkerLease only accepts decimal uint64,
	// so the read failed — AFTER Redis had already stored the unreadable
	// record, which is the opposite of refusing bad input (RR-20260913-11).
	// Returning nil past uint64 means the caller refuses before persisting.
	ownershipDecimalInc = `
local function incdec(v)
  if not v or v == "" then v = "0" end
  v = string.gsub(v, "^0+", "")
  if v == "" then v = "0" end
  local out = {}
  local carry = 1
  for i = #v, 1, -1 do
    local d = string.byte(v, i) - 48 + carry
    if d >= 10 then d = d - 10 carry = 1 else carry = 0 end
    out[i] = string.char(48 + d)
  end
  local s = table.concat(out)
  if carry == 1 then s = "1" .. s end
  if #s > 20 or (#s == 20 and s > "18446744073709551615") then return nil end
  return s
end
`
	ownershipEnterSharedScript = ownershipDecimalInc + `
local current = redis.call("HGET", KEYS[1], ARGV[1])
local expected = ARGV[2]
if not current or current ~= expected then return "" end
local mode, owner, currentFence, currentRoute = string.match(current, "^(%a+):(-?%d+):(%d+):(%d+)$")
if mode ~= "local" then return "" end
local nextFence = incdec(currentFence)
if not nextFence then return "" end
local value = "shared:" .. owner .. ":" .. nextFence .. ":" .. currentRoute
redis.call("HSET", KEYS[1], ARGV[1], value)
return value
`
	ownershipLeaveSharedScript = ownershipDecimalInc + `
local current = redis.call("HGET", KEYS[1], ARGV[1])
if current == ARGV[2] then
  local mode, owner, currentFence, currentRoute = string.match(current, "^(%a+):(-?%d+):(%d+):(%d+)$")
  if mode ~= "shared" then return "" end
  local nextFence = incdec(currentFence)
  if not nextFence then return "" end
  local value = "local:" .. owner .. ":" .. nextFence .. ":" .. currentRoute
  redis.call("HSET", KEYS[1], ARGV[1], value)
  return value
end
return ""
`
	ownershipGetScript = `
local current = redis.call("HGET", KEYS[1], ARGV[1])
if not current then return "" end
return current
`
	ownershipTransferScript = ownershipDecimalInc + `
local current = redis.call("HGET", KEYS[1], ARGV[1])
if current ~= ARGV[2] then return "" end
local mode, _, currentMarker, currentRoute = string.match(current, "^(%a+):(-?%d+):(%d+):(%d+)$")
if not mode then return "" end
local marker = incdec(currentMarker)
local route = incdec(currentRoute)
if not marker or not route then return "" end
local value = mode .. ":" .. ARGV[3] .. ":" .. marker .. ":" .. route
redis.call("HSET", KEYS[1], ARGV[1], value)
return value
`
)

// markerRedisKey 是缺省的所有权标记 hash 键：一把 hash，field 是实体 ID。正式 Assemble 不写它
// （所有权存储是 Mongo 的 WriteAuthority）；只有自行 SetOwnershipStore(NewRedisMarker…) 的非 authority
// 兼容装配会用到。缺省不带部署前缀，共用一个 Redis db 的多个部署应各自加前缀（RR-20260930-19）。
const markerRedisKey = "remote_entity:marks"

type markerEval interface {
	Eval(ctx context.Context, script string, keys []string, args ...any) (any, error)
}

// redisMarker implements entity.IRemoteEntityOwnershipStore using one Redis
// hash and Lua CAS transitions. Ownership absence is never interpreted as a
// local lease.
type redisMarker struct {
	redis markerEval
	key   string
}

var _ entity.IRemoteEntityOwnershipStore = (*redisMarker)(nil)

// NewRedisMarker 用显式键名构造标记存储；key 为空时用缺省 remote_entity:marks。
func NewRedisMarker(redis fredis.IRedis, key string) *redisMarker {
	return newRedisMarkerForEval(redis, key)
}

// NewRedisMarkerWithKeyPrefix 与 NewRedisMarker(redis, "") 相同，另给标记键加部署前缀：
// prefix 为空时键仍是 remote_entity:marks（逐字不变），非空时是 "<prefix>:remote_entity:marks"，
// 与 L2 快照键前缀（NewSnapshotL2StoreWithKeyPrefix）同形，同一个部署前缀值可同时用于两者。
// 前缀先经 ValidateMarkerKeyPrefix 校验（RR-20260930-19）。
func NewRedisMarkerWithKeyPrefix(redis fredis.IRedis, prefix string) (*redisMarker, error) {
	return newRedisMarkerForEvalWithKeyPrefix(redis, prefix)
}

// ValidateMarkerKeyPrefix 校验标记键前缀：可以为空（键不变）；非空时不能含首尾或内部空白。
// 标记只有一把 hash 键，Redis Cluster hash tag 无害、允许（与 L2 前缀不同，L2 是逐实体多键，带 tag 会钉在一个槽）；
// 但一旦写了花括号就要求首个 tag 非空且闭合——Redis 对 "e{" / "e{}" 不做 tag 哈希，配置者会以为钉住了槽而没有。
// 这与 kit 对 remote_entity.lock_key 的 Cluster 判断是同一条规则（RR-20260924-25）。
func ValidateMarkerKeyPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if strings.TrimSpace(prefix) != prefix || strings.ContainsAny(prefix, " \t\r\n") {
		return fmt.Errorf("remote_entity: marker key prefix %q must not contain whitespace", prefix)
	}
	if start := strings.IndexByte(prefix, '{'); start >= 0 {
		if end := strings.IndexByte(prefix[start+1:], '}'); end <= 0 {
			return fmt.Errorf("remote_entity: marker key prefix %q has an empty or unclosed first Redis Cluster hash tag; use e.g. {roost:remote} or drop the braces", prefix)
		}
	}
	return nil
}

func markerKeyForPrefix(prefix string) string {
	if prefix == "" {
		return markerRedisKey
	}
	return prefix + ":" + markerRedisKey
}

func newRedisMarkerForEvalWithKeyPrefix(redis markerEval, prefix string) (*redisMarker, error) {
	if err := ValidateMarkerKeyPrefix(prefix); err != nil {
		return nil, err
	}
	return newRedisMarkerForEval(redis, markerKeyForPrefix(prefix)), nil
}

func newRedisMarkerForEval(redis markerEval, key string) *redisMarker {
	if key == "" {
		key = markerRedisKey
	}
	return &redisMarker{redis: redis, key: key}
}

func (m *redisMarker) GetOwnership(ctx context.Context, id int64) (entity.RemoteEntityMarkerLease, bool, error) {
	field := strconv.FormatInt(id, 10)
	raw, err := m.redis.Eval(ctx, ownershipGetScript, []string{m.key}, field)
	if err != nil {
		return entity.RemoteEntityMarkerLease{}, false, err
	}
	value := fmt.Sprint(raw)
	if value == "" {
		return entity.RemoteEntityMarkerLease{}, false, nil
	}
	_, lease, err := parseMarkerLease(value)
	if err != nil {
		return entity.RemoteEntityMarkerLease{}, false, err
	}
	return lease, true, nil
}

func (m *redisMarker) ClaimOwnership(ctx context.Context, id int64, ownerSid int32) (entity.RemoteEntityMarkerLease, error) {
	if id == 0 || ownerSid == 0 {
		return entity.RemoteEntityMarkerLease{}, fmt.Errorf("remote_entity: invalid ownership claim for %d", id)
	}
	field := strconv.FormatInt(id, 10)
	raw, err := m.redis.Eval(ctx, ownershipClaimScript, []string{m.key}, field, strconv.FormatInt(int64(ownerSid), 10))
	if err != nil {
		return entity.RemoteEntityMarkerLease{}, err
	}
	if fmt.Sprint(raw) == "" {
		return entity.RemoteEntityMarkerLease{}, fmt.Errorf("remote_entity: ownership claim conflict for %d", id)
	}
	_, lease, err := parseMarkerLease(fmt.Sprint(raw))
	return lease, err
}

func (m *redisMarker) EnterSharedExpected(ctx context.Context, id int64, expected entity.RemoteEntityMarkerLease) (entity.RemoteEntityMarkerLease, error) {
	if expected.Shared {
		return entity.RemoteEntityMarkerLease{}, fmt.Errorf("remote_entity: expected local lease for %d", id)
	}
	field := strconv.FormatInt(id, 10)
	raw, err := m.redis.Eval(ctx, ownershipEnterSharedScript, []string{m.key}, field, formatMarkerLease(expected))
	if err != nil {
		return entity.RemoteEntityMarkerLease{}, err
	}
	if fmt.Sprint(raw) == "" {
		return entity.RemoteEntityMarkerLease{}, fmt.Errorf("remote_entity: enter shared compare-and-swap failed for %d", id)
	}
	_, lease, err := parseMarkerLease(fmt.Sprint(raw))
	if err != nil {
		return entity.RemoteEntityMarkerLease{}, err
	}
	return lease, nil
}

func (m *redisMarker) LeaveSharedExpected(ctx context.Context, id int64, lease entity.RemoteEntityMarkerLease) (entity.RemoteEntityMarkerLease, error) {
	if !lease.Shared || lease.MarkerEpoch == 0 {
		return entity.RemoteEntityMarkerLease{}, fmt.Errorf("remote_entity: invalid shared lease for %d", id)
	}
	field := strconv.FormatInt(id, 10)
	expected := formatMarkerLease(lease)
	raw, err := m.redis.Eval(ctx, ownershipLeaveSharedScript, []string{m.key}, field, expected)
	if err != nil {
		return entity.RemoteEntityMarkerLease{}, err
	}
	shared, next, err := parseMarkerLease(fmt.Sprint(raw))
	if err != nil {
		return entity.RemoteEntityMarkerLease{}, err
	}
	if shared || next.MarkerEpoch <= lease.MarkerEpoch {
		return entity.RemoteEntityMarkerLease{}, fmt.Errorf("remote_entity: marker fence mismatch for %d", id)
	}
	return next, nil
}

func (m *redisMarker) TransferExpected(ctx context.Context, id int64, expected entity.RemoteEntityMarkerLease, newOwnerSid int32) (entity.RemoteEntityMarkerLease, error) {
	if expected.MarkerEpoch == 0 || expected.RouteEpoch == 0 || newOwnerSid == 0 || newOwnerSid == expected.OwnerSid {
		return entity.RemoteEntityMarkerLease{}, fmt.Errorf("remote_entity: invalid ownership transfer for %d", id)
	}
	field := strconv.FormatInt(id, 10)
	raw, err := m.redis.Eval(ctx, ownershipTransferScript, []string{m.key}, field, formatMarkerLease(expected), strconv.FormatInt(int64(newOwnerSid), 10))
	if err != nil {
		return entity.RemoteEntityMarkerLease{}, err
	}
	if fmt.Sprint(raw) == "" {
		return entity.RemoteEntityMarkerLease{}, fmt.Errorf("remote_entity: ownership compare-and-swap failed for %d", id)
	}
	_, next, err := parseMarkerLease(fmt.Sprint(raw))
	if err != nil {
		return entity.RemoteEntityMarkerLease{}, err
	}
	if next.OwnerSid != newOwnerSid || next.MarkerEpoch <= expected.MarkerEpoch || next.RouteEpoch <= expected.RouteEpoch {
		return entity.RemoteEntityMarkerLease{}, fmt.Errorf("remote_entity: invalid ownership epoch for %d", id)
	}
	return next, nil
}

func parseMarkerLease(raw string) (bool, entity.RemoteEntityMarkerLease, error) {
	parts := strings.Split(raw, ":")
	if len(parts) != 4 || (parts[0] != "shared" && parts[0] != "local") {
		return false, entity.RemoteEntityMarkerLease{}, fmt.Errorf("remote_entity: invalid marker lease %q", raw)
	}
	owner, ownerErr := strconv.ParseInt(parts[1], 10, 32)
	fence, fenceErr := strconv.ParseUint(parts[2], 10, 64)
	route, routeErr := strconv.ParseUint(parts[3], 10, 64)
	if ownerErr != nil || fenceErr != nil || routeErr != nil || fence == 0 || route == 0 {
		return false, entity.RemoteEntityMarkerLease{}, fmt.Errorf("remote_entity: invalid marker lease %q", raw)
	}
	shared := parts[0] == "shared"
	return shared, entity.RemoteEntityMarkerLease{OwnerSid: int32(owner), MarkerEpoch: fence, RouteEpoch: route, Shared: shared}, nil
}

func formatMarkerLease(lease entity.RemoteEntityMarkerLease) string {
	mode := "local"
	if lease.Shared {
		mode = "shared"
	}
	return fmt.Sprintf("%s:%d:%d:%d", mode, lease.OwnerSid, lease.MarkerEpoch, lease.RouteEpoch)
}
