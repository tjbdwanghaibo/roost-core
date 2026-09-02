package remoteentity

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/tjbdwanghaibo/cube-core/entity"
	fredis "github.com/tjbdwanghaibo/cube-core/redis"
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
	ownershipEnterSharedScript = `
local current = redis.call("HGET", KEYS[1], ARGV[1])
local expected = ARGV[2]
if not current or current ~= expected then return "" end
local mode, owner, currentFence, currentRoute = string.match(current, "^(%a+):(-?%d+):(%d+):(%d+)$")
if mode ~= "local" then return "" end
local value = "shared:" .. owner .. ":" .. ((tonumber(currentFence) or 0) + 1) .. ":" .. currentRoute
redis.call("HSET", KEYS[1], ARGV[1], value)
return value
`
	ownershipLeaveSharedScript = `
local current = redis.call("HGET", KEYS[1], ARGV[1])
if current == ARGV[2] then
  local mode, owner, currentFence, currentRoute = string.match(current, "^(%a+):(-?%d+):(%d+):(%d+)$")
  if mode ~= "shared" then return "" end
  local value = "local:" .. owner .. ":" .. ((tonumber(currentFence) or 0) + 1) .. ":" .. currentRoute
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
	ownershipTransferScript = `
local current = redis.call("HGET", KEYS[1], ARGV[1])
if current ~= ARGV[2] then return "" end
local mode, _, currentMarker, currentRoute = string.match(current, "^(%a+):(-?%d+):(%d+):(%d+)$")
if not mode then return "" end
local marker = (tonumber(currentMarker) or 0) + 1
local route = (tonumber(currentRoute) or 0) + 1
local value = mode .. ":" .. ARGV[3] .. ":" .. marker .. ":" .. route
redis.call("HSET", KEYS[1], ARGV[1], value)
return value
`
)

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

func newRedisMarker(redis fredis.IRedis, key string) *redisMarker {
	return newRedisMarkerForEval(redis, key)
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
