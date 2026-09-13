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

func NewRedisMarker(redis fredis.IRedis, key string) *redisMarker {
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
