package remoteentity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/syncbus/mirror"
)

// N05 / RR-20261005-NC-35、36：经 BindSync 和正式 Replicator 收到 delta 后，
// gap/epoch/schema 回填须使用权威全量；加载失败或最终版本不够，不能向发布调用方报告成功。
// 此夹具证明同步传输中的错误传播与缓存状态，不代表真实 broker 的 ACK/retry。
func TestSnapshotReplicaAuthoritativeRepairAdmission(t *testing.T) {
	key := interestKeyFor(t, 244, 9539)
	for _, scenario := range []string{"gap", "epoch", "schema", "loader-error", "foreign-key", "retained-epoch"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			m := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
			current := entity.RemoteSnapshotEnvelope{Key: key, StateVersion: 1, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Codec: 1, Full: true, Payload: entity.CopyFrozenRemoteSnapshotPayload([]byte("before"))}
			update := entity.RemoteSnapshotRecord{Key: key, BaseVersion: 8, StateVersion: 9, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Codec: 1, Data: []byte("unused-delta")}
			if scenario == "epoch" {
				update.BaseVersion, update.MarkerEpoch, update.RouteEpoch = 1, 2, 2
			}
			if scenario == "schema" {
				update.BaseVersion, update.Schema = 1, 2
			}
			if scenario == "retained-epoch" {
				current.MarkerEpoch, current.RouteEpoch = 2, 2
			}
			answer := entity.RemoteSnapshotEnvelope{Key: key, StateVersion: 9, MarkerEpoch: update.MarkerEpoch, RouteEpoch: update.RouteEpoch, Schema: update.Schema, Codec: 1, Full: true, Payload: entity.CopyFrozenRemoteSnapshotPayload([]byte("authority-full"))}
			if scenario == "foreign-key" {
				answer.Key.Scope++
			}
			loadFailure := errors.New("authority unavailable")
			bad := true
			calls := 0
			m.remote.cache = entity.NewRemoteSnapshotCache(entity.RemoteSnapshotCacheConfig{Shards: 1, MaxEntries: 4, MaxBytes: 4096, TTL: time.Minute}, nil,
				func(_ context.Context, requested entity.RemoteSnapshotKey, _ entity.RemoteReadConsistency, minimum uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
					calls++
					if requested != key || minimum != 9 {
						t.Errorf("loader arguments: key=%+v minimum=%d", requested, minimum)
					}
					if bad && scenario == "loader-error" {
						return entity.RemoteSnapshotEnvelope{}, false, loadFailure
					}
					return answer, true, nil
				})
			if err := m.remote.cache.Publish(ctx, current); err != nil {
				t.Fatal(err)
			}
			bus := &interestIdentityBus{}
			receiver, _ := m.BindSync(bus)
			if err := receiver.Start(); err != nil {
				t.Fatal(err)
			}
			defer receiver.Stop()
			publisher := NewSyncer(mirror.New(bus, SyncTopicSnapshot, nil))
			err := publisher.PublishRemoteSnapshot(ctx, update)
			wantFailure := scenario == "loader-error" || scenario == "foreign-key" || scenario == "retained-epoch"
			if wantFailure && err == nil {
				t.Fatal("failed authoritative repair returned success to publisher")
			}
			if !wantFailure && err != nil {
				t.Fatal(err)
			}
			if scenario == "loader-error" && !errors.Is(err, loadFailure) {
				t.Fatalf("loader error lost: %v", err)
			}
			if scenario == "retained-epoch" && !errors.Is(err, entity.ErrRemoteSnapshotStale) {
				t.Fatalf("minimum-version error lost: %v", err)
			}
			got, found, readErr := m.remote.cache.Get(ctx, key, entity.RemoteReadCached, 0)
			if readErr != nil || !found || calls != 1 {
				t.Fatalf("repair state: found=%v calls=%d err=%v", found, calls, readErr)
			}
			if wantFailure {
				if got.StateVersion != current.StateVersion || string(got.Payload.BytesCopy()) != "before" || got.MarkerEpoch != current.MarkerEpoch {
					t.Fatal("failed repair changed requested cache")
				}
				if scenario == "foreign-key" {
					if _, found, _ := m.remote.cache.Get(ctx, answer.Key, entity.RemoteReadCached, 0); found {
						t.Fatal("foreign repair poisoned another key")
					}
				}
				bad = false
				answer.Key, answer.MarkerEpoch, answer.RouteEpoch = key, current.MarkerEpoch, current.RouteEpoch
				if err := publisher.PublishRemoteSnapshot(ctx, update); err != nil {
					t.Fatalf("repair retry: %v", err)
				}
				got, found, readErr = m.remote.cache.Get(ctx, key, entity.RemoteReadCached, 0)
				if calls != 2 {
					t.Fatalf("repair retry calls=%d", calls)
				}
			}
			if readErr != nil || !found || got.StateVersion != 9 || !got.Full || string(got.Payload.BytesCopy()) != "authority-full" || got.Schema != answer.Schema {
				t.Fatalf("full repair missing: found=%v snapshot=%+v err=%v", found, got, readErr)
			}
		})
	}
}
