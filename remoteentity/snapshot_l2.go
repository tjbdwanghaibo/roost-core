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

const remoteSnapshotL2CAS = `
local oldMarker = tonumber(redis.call("HGET", KEYS[1], "marker") or "0")
local oldRoute = tonumber(redis.call("HGET", KEYS[1], "route") or "0")
local oldVersion = tonumber(redis.call("HGET", KEYS[1], "version") or "0")
local oldChecksum = redis.call("HGET", KEYS[1], "checksum") or ""
local marker = tonumber(ARGV[1])
local route = tonumber(ARGV[2])
local version = tonumber(ARGV[3])
if marker < oldMarker or route < oldRoute then return 0 end
if marker == oldMarker and route == oldRoute and version < oldVersion then return 0 end
if marker == oldMarker and route == oldRoute and version == oldVersion and oldChecksum ~= "" and oldChecksum ~= ARGV[4] then return -1 end
redis.call("HSET", KEYS[1], "marker", marker, "route", route, "version", version, "checksum", ARGV[4], "data", ARGV[5])
local ttl = tonumber(ARGV[6])
if ttl and ttl > 0 then redis.call("PEXPIRE", KEYS[1], ttl) end
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

func newRemoteSnapshotL2Store(redis remoteSnapshotRedis, ttl time.Duration) *remoteSnapshotL2Store {
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
		value.MarkerEpoch, value.RouteEpoch, value.StateVersion, value.Checksum, raw, ttlMillis)
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

func remoteSnapshotL2Key(key entity.RemoteSnapshotKey) string {
	return fmt.Sprintf("remote_entity:snapshot:%d:%d:%d:%d:%d", key.Tenant, key.Kind, key.EntityID, key.Scope, key.Policy)
}
