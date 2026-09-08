package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	rediscore "github.com/tjbdwanghaibo/roost-core/redis"
)

type snapshotRedisFake struct {
	mu     sync.Mutex
	values map[string]map[string][]byte
}

func newSnapshotRedisFake() *snapshotRedisFake {
	return &snapshotRedisFake{values: make(map[string]map[string][]byte)}
}

func (f *snapshotRedisFake) HGet(_ context.Context, key, field string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	value := f.values[key][field]
	if value == nil {
		return nil, rediscore.ErrNil
	}
	return append([]byte(nil), value...), nil
}

func (f *snapshotRedisFake) Eval(_ context.Context, _ string, keys []string, args ...any) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fields := f.values[keys[0]]
	if fields == nil {
		fields = make(map[string][]byte)
		f.values[keys[0]] = fields
	}
	parse := func(value any) uint64 {
		parsed, _ := strconv.ParseUint(fmt.Sprint(value), 10, 64)
		return parsed
	}
	oldMarker, oldRoute, oldVersion := parse(string(fields["marker"])), parse(string(fields["route"])), parse(string(fields["version"]))
	marker, route, version := parse(args[0]), parse(args[1]), parse(args[2])
	if marker < oldMarker || route < oldRoute || (marker == oldMarker && route == oldRoute && version < oldVersion) {
		return int64(0), nil
	}
	checksum := fmt.Sprint(args[3])
	if marker == oldMarker && route == oldRoute && version == oldVersion && len(fields["checksum"]) > 0 && string(fields["checksum"]) != checksum {
		return int64(-1), nil
	}
	fields["marker"] = []byte(strconv.FormatUint(marker, 10))
	fields["route"] = []byte(strconv.FormatUint(route, 10))
	fields["version"] = []byte(strconv.FormatUint(version, 10))
	fields["checksum"] = []byte(checksum)
	fields["data"] = append([]byte(nil), args[4].([]byte)...)
	return int64(1), nil
}

func (f *snapshotRedisFake) Del(_ context.Context, keys ...string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, key := range keys {
		delete(f.values, key)
	}
	return int64(len(keys)), nil
}

func TestRemoteSnapshotL2RejectsDelayedPublisher(t *testing.T) {
	const kind entity.EntityKind = 127
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, err := entity.BuildEntityID(1901, kind)
	if err != nil {
		t.Fatal(err)
	}
	key := entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
	store := newRemoteSnapshotL2Store(newSnapshotRedisFake(), time.Minute)
	newer := entity.RemoteSnapshotEnvelope{Key: key, StateVersion: 8, BaseVersion: 7, MarkerEpoch: 4, RouteEpoch: 2, Schema: 1, Full: true, Payload: entity.CopyFrozenRemoteSnapshotPayload([]byte("new"))}
	if err := store.Set(context.Background(), newer); err != nil {
		t.Fatal(err)
	}
	stale := newer
	stale.StateVersion = 7
	stale.Payload = entity.CopyFrozenRemoteSnapshotPayload([]byte("stale"))
	if err := store.Set(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Get(context.Background(), key)
	if err != nil || !ok || got.StateVersion != 8 || string(got.Payload.BytesCopy()) != "new" {
		t.Fatalf("got=%+v ok=%v err=%v", got, ok, err)
	}
}

func TestRemoteSnapshotL2RejectsSameVersionDifferentContent(t *testing.T) {
	const kind entity.EntityKind = 129
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, err := entity.BuildEntityID(1903, kind)
	if err != nil {
		t.Fatal(err)
	}
	key := entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
	store := newRemoteSnapshotL2Store(newSnapshotRedisFake(), time.Minute)
	value := entity.RemoteSnapshotEnvelope{Key: key, StateVersion: 1, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Full: true, Payload: entity.CopyFrozenRemoteSnapshotPayload([]byte("a"))}
	if err := store.Set(context.Background(), value); err != nil {
		t.Fatal(err)
	}
	value.Payload = entity.CopyFrozenRemoteSnapshotPayload([]byte("b"))
	if err := store.Set(context.Background(), value); !errors.Is(err, entity.ErrRemoteVersionConflict) {
		t.Fatalf("conflict error=%v", err)
	}
}
