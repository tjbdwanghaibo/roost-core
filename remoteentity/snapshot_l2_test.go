package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	// Mirrors the script exactly: ordered fields are compared as decimal
	// strings and stored verbatim, and the same-version check covers schema
	// and codec as well as the payload checksum.
	norm := func(v string) string {
		trimmed := strings.TrimLeft(v, "0")
		if trimmed == "" {
			return "0"
		}
		return trimmed
	}
	cmp := func(a, b string) int {
		a, b = norm(a), norm(b)
		if len(a) != len(b) {
			if len(a) < len(b) {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	}
	arg := func(value any) string { return fmt.Sprint(value) }
	oldMarker, oldRoute, oldVersion := string(fields["marker"]), string(fields["route"]), string(fields["version"])
	marker, route, version := arg(args[0]), arg(args[1]), arg(args[2])
	markerCmp, routeCmp := cmp(marker, oldMarker), cmp(route, oldRoute)
	if markerCmp < 0 || routeCmp < 0 {
		return int64(0), nil
	}
	sameEpoch := markerCmp == 0 && routeCmp == 0
	versionCmp := cmp(version, oldVersion)
	if sameEpoch && versionCmp < 0 {
		return int64(0), nil
	}
	checksum, schema, codec := arg(args[3]), arg(args[6]), arg(args[7])
	if sameEpoch && versionCmp == 0 && len(fields["checksum"]) > 0 &&
		(string(fields["checksum"]) != checksum || string(fields["schema"]) != schema || string(fields["codec"]) != codec) {
		return int64(-1), nil
	}
	fields["marker"] = []byte(marker)
	fields["route"] = []byte(route)
	fields["version"] = []byte(version)
	fields["checksum"] = []byte(checksum)
	fields["schema"] = []byte(schema)
	fields["codec"] = []byte(codec)
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
	store := NewSnapshotL2Store(newSnapshotRedisFake(), time.Minute)
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
	store := NewSnapshotL2Store(newSnapshotRedisFake(), time.Minute)
	value := entity.RemoteSnapshotEnvelope{Key: key, StateVersion: 1, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Full: true, Payload: entity.CopyFrozenRemoteSnapshotPayload([]byte("a"))}
	if err := store.Set(context.Background(), value); err != nil {
		t.Fatal(err)
	}
	value.Payload = entity.CopyFrozenRemoteSnapshotPayload([]byte("b"))
	if err := store.Set(context.Background(), value); !errors.Is(err, entity.ErrRemoteVersionConflict) {
		t.Fatalf("conflict error=%v", err)
	}
}
