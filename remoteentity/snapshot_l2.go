package remoteentity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
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

const remoteSnapshotL2CAS = remoteSnapshotL2Lua + `local oldMarker = redis.call("HGET", KEYS[1], "marker") or "0"
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
local ttl = tonumber(ARGV[6])
if ttl and ttl > 0 then redis.call("PEXPIRE", KEYS[1], ttl) end
return 1
`

// remoteSnapshotL2DeleteAtVersion deletes the key unless it holds a
// snapshot newer than ARGV[1]. Compared as an exact decimal string like the
// CAS above; a delete is ordered against the versions of one key only, so
// epochs are not part of it (U-0187 复核补修, RR-20260913-01).
const remoteSnapshotL2DeleteAtVersion = remoteSnapshotL2Lua + `
local stored = redis.call("HGET", KEYS[1], "version")
if stored and cmp(stored, ARGV[1]) > 0 then return 0 end
redis.call("DEL", KEYS[1])
return 1
`

// remoteSnapshotL2Store is the shared cache layer. Its comparison and write
// happen in one Redis script, so a delayed publisher cannot overwrite a newer
// ownership epoch or state version.
type remoteSnapshotL2Store struct {
	redis remoteSnapshotRedis
	ttl   time.Duration
}

type remoteSnapshotL2Value struct {
	Key          entity.RemoteSnapshotKey `json:"key"`
	StateVersion uint64                   `json:"state_version"`
	BaseVersion  uint64                   `json:"base_version"`
	MarkerEpoch  uint64                   `json:"marker_epoch"`
	RouteEpoch   uint64                   `json:"route_epoch"`
	Schema       uint32                   `json:"schema"`
	Codec        uint16                   `json:"codec"`
	Checksum     uint64                   `json:"checksum"`
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

func (s *remoteSnapshotL2Store) Get(ctx context.Context, key entity.RemoteSnapshotKey) (entity.RemoteSnapshotEnvelope, bool, error) {
	if s == nil || s.redis == nil || !key.Valid() {
		return entity.RemoteSnapshotEnvelope{}, false, nil
	}
	raw, err := s.redis.HGet(ctx, remoteSnapshotL2Key(key), "data")
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
	result, err := s.redis.Eval(ctx, remoteSnapshotL2CAS, []string{remoteSnapshotL2Key(value.Key)},
		value.MarkerEpoch, value.RouteEpoch, value.StateVersion, value.Checksum, raw, ttlMillis,
		value.Schema, value.Codec)
	if err != nil {
		return err
	}
	accepted, parseErr := strconv.ParseInt(fmt.Sprint(result), 10, 64)
	if parseErr != nil {
		return fmt.Errorf("remote_entity: invalid L2 CAS response %q: %w", fmt.Sprint(result), parseErr)
	}
	if accepted == 0 {
		return nil
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
	_, err := s.redis.Del(ctx, remoteSnapshotL2Key(key))
	return err
}

// DeleteAtVersion implements entity.RemoteSnapshotVersionedDeleter: the
// comparison and the delete run in one script, so a newer snapshot that lands
// between "read version" and "DEL" cannot be lost.
func (s *remoteSnapshotL2Store) DeleteAtVersion(ctx context.Context, key entity.RemoteSnapshotKey, version uint64) error {
	if s == nil || s.redis == nil || !key.Valid() {
		return nil
	}
	_, err := s.redis.Eval(ctx, remoteSnapshotL2DeleteAtVersion, []string{remoteSnapshotL2Key(key)}, strconv.FormatUint(version, 10))
	return err
}

var _ entity.RemoteSnapshotVersionedDeleter = (*remoteSnapshotL2Store)(nil)

func remoteSnapshotL2Key(key entity.RemoteSnapshotKey) string {
	return fmt.Sprintf("remote_entity:snapshot:%d:%d:%d:%d:%d", key.Tenant, key.Kind, key.EntityID, key.Scope, key.Policy)
}
