// 快照复制接收：payload 身份绑定信封、读出口只回源一次、权威回填准入、删除按版本留水位、checksum 编码。

package remoteentity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus/mirror"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// payload 里的身份（wire.Key、wire.Update.Key、Update.StateVersion）必须与外层信封交叉校验后才准写入，否则路由 / 顺序用的身份和
// 实际被改的视图不是同一个。这是协议完整性，不是授权：授权仍由传输与服务身份边界负责。
// 历史：RR-20260913-03。
func TestApplyReplicaRefusesPayloadIdentityThatContradictsTheEnvelope(t *testing.T) {
	const kind entity.EntityKind = 243
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, err := entity.BuildEntityID(9331, kind)
	if err != nil {
		t.Fatal(err)
	}
	declared := entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
	foreign := entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 2}

	manager := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	store := SnapshotReplicaStore{client: manager.snapshots}
	record := entity.RemoteSnapshotRecord{
		Key: declared, StateVersion: 4, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Codec: 1,
		Full: true, Data: []byte("payload"),
	}
	record.Checksum = entity.RemoteSnapshotChecksum(record.Data)

	envelopeFor := func(w remoteSnapshotWire, key entity.RemoteSnapshotKey, version uint64) mirror.Envelope {
		raw, err := json.Marshal(w)
		if err != nil {
			t.Fatal(err)
		}
		return mirror.Envelope{Key: remoteSnapshotReplicaKey(key), Version: int64(version), Op: mirror.OpUpsert, Payload: raw}
	}

	// The honest message applies.
	good := remoteSnapshotWire{Key: declared, Update: record, PublishedAt: time.Now().UnixNano()}
	if err := store.ApplyReplica(context.Background(), envelopeFor(good, declared, record.StateVersion)); err != nil {
		t.Fatalf("a well-formed message must apply: %v", err)
	}

	for _, tc := range []struct {
		name  string
		twist func(remoteSnapshotWire) remoteSnapshotWire
	}{
		{"payload key disagrees with the wire key", func(w remoteSnapshotWire) remoteSnapshotWire {
			w.Update.Key = foreign
			return w
		}},
		{"wire key disagrees with the envelope key", func(w remoteSnapshotWire) remoteSnapshotWire {
			w.Key = foreign
			w.Update.Key = foreign
			return w
		}},
		{"payload version disagrees with the envelope version", func(w remoteSnapshotWire) remoteSnapshotWire {
			w.Update.StateVersion = record.StateVersion + 7
			return w
		}},
		{"delete names a different key than the envelope", func(w remoteSnapshotWire) remoteSnapshotWire {
			return remoteSnapshotWire{Delete: true, Key: foreign}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, hadBefore, err := manager.snapshots.cache.Get(context.Background(), foreign, entity.RemoteReadCached, 0)
			if err != nil {
				t.Fatal(err)
			}
			env := envelopeFor(tc.twist(remoteSnapshotWire{Key: declared, Update: record, PublishedAt: time.Now().UnixNano()}), declared, record.StateVersion)
			if err := store.ApplyReplica(context.Background(), env); err == nil {
				t.Fatal("a message whose payload identity contradicts its envelope was applied")
			}
			after, hasAfter, err := manager.snapshots.cache.Get(context.Background(), foreign, entity.RemoteReadCached, 0)
			if err != nil {
				t.Fatal(err)
			}
			if hasAfter != hadBefore || after.StateVersion != before.StateVersion {
				t.Fatalf("the refused message still changed the foreign scope: before=(%d,%v) after=(%d,%v)",
					before.StateVersion, hadBefore, after.StateVersion, hasAfter)
			}
		})
	}
}

// 一个读出口、一次回源：快照缓存的 Get 已在陈旧上限内确认、Monotonic 不足时合并回源，Manager.ReadRemoteSnapshot 不再保留
// 外层回退（结果不满足就再直接加载一次，不经合并、不受等待名额约束）。Monotonic 未命中只回源一次；并发读者合并成一次。
// kind 253 与 B2 用例共用（同一注册定义），EntityID 用 96xx 段，与 B2 的 95xx 不重叠。

type countingSnapshotBackend struct {
	plainStorageBackend
	nopLoader
	loads   atomic.Int32
	release chan struct{} // 非 nil 时每次加载等它关闭
	answer  func(entity.RemoteSnapshotKey) (entity.RemoteSnapshotEnvelope, bool, error)
}

func (b *countingSnapshotBackend) LoadRemoteSnapshot(ctx context.Context, key entity.RemoteSnapshotKey, _ entity.RemoteReadConsistency, _ uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
	b.loads.Add(1)
	if b.release != nil {
		select {
		case <-b.release:
		case <-ctx.Done():
			return entity.RemoteSnapshotEnvelope{}, false, ctx.Err()
		}
	}
	if b.answer != nil {
		return b.answer(key)
	}
	return entity.RemoteSnapshotEnvelope{}, false, nil
}

func newReadExitManager(t *testing.T, backend *countingSnapshotBackend) *Manager {
	t.Helper()
	m := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1301)
	m.SetBackend(backend)
	return m
}

// Monotonic 回源未找到时只回源一次，ReadRemoteSnapshot 不再直接 LoadAuthoritative 第二次。
func TestReadRemoteSnapshotMonotonicMissLoadsAuthorityOnce(t *testing.T) {
	ctx := context.Background()
	backend := &countingSnapshotBackend{}
	m := newReadExitManager(t, backend)
	key := staleBackfillKey(t, b2WatermarkKind, 9601)
	_, found, err := m.ReadRemoteSnapshot(ctx, key, entity.RemoteReadMonotonic, 0)
	if err != nil || found {
		t.Fatalf("missing key: found=%v err=%v", found, err)
	}
	if got := backend.loads.Load(); got != 1 {
		t.Fatalf("one Monotonic miss loaded the authority %d times, want 1 (the reader's fallback reloaded outside the coalesced load)", got)
	}
}

// 权威低于最低版本时只回源一次，不在外层再等待后直接加载第二次。
func TestReadRemoteSnapshotMonotonicBelowMinimumLoadsAuthorityOnce(t *testing.T) {
	ctx := context.Background()
	key := staleBackfillKey(t, b2WatermarkKind, 9602)
	backend := &countingSnapshotBackend{answer: func(k entity.RemoteSnapshotKey) (entity.RemoteSnapshotEnvelope, bool, error) {
		return staleBackfillEnvelope(k, 3, 1, "v3"), true, nil
	}}
	m := newReadExitManager(t, backend)
	_, found, err := m.ReadRemoteSnapshot(ctx, key, entity.RemoteReadMonotonic, 5)
	if !errors.Is(err, entity.ErrRemoteSnapshotStale) || found {
		t.Fatalf("authority below the minimum: found=%v err=%v, want ErrRemoteSnapshotStale", found, err)
	}
	if got := backend.loads.Load(); got != 1 {
		t.Fatalf("one Monotonic read below the minimum loaded the authority %d times, want 1", got)
	}
}

// 经 BindSync 和正式 Replicator 收到 delta 后，gap / epoch / schema 回填使用权威全量；加载失败或最终版本不够，不向发布调用方
// 报告成功。此夹具证明同步传输中的错误传播与缓存状态，不代表真实 broker 的 ACK / retry。
// 历史：RR-20261005-NC-35、RR-20261005-NC-36。
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
			m.snapshots.cache = entity.NewRemoteSnapshotCache(entity.RemoteSnapshotCacheConfig{Shards: 1, MaxEntries: 4, MaxBytes: 4096, TTL: time.Minute}, nil,
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
			if err := m.snapshots.cache.Publish(ctx, current); err != nil {
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
			got, found, readErr := m.snapshots.cache.Get(ctx, key, entity.RemoteReadCached, 0)
			if readErr != nil || !found || calls != 1 {
				t.Fatalf("repair state: found=%v calls=%d err=%v", found, calls, readErr)
			}
			if wantFailure {
				if got.StateVersion != current.StateVersion || string(got.Payload.BytesCopy()) != "before" || got.MarkerEpoch != current.MarkerEpoch {
					t.Fatal("failed repair changed requested cache")
				}
				if scenario == "foreign-key" {
					if _, found, _ := m.snapshots.cache.Get(ctx, answer.Key, entity.RemoteReadCached, 0); found {
						t.Fatal("foreign repair poisoned another key")
					}
				}
				bad = false
				answer.Key, answer.MarkerEpoch, answer.RouteEpoch = key, current.MarkerEpoch, current.RouteEpoch
				if err := publisher.PublishRemoteSnapshot(ctx, update); err != nil {
					t.Fatalf("repair retry: %v", err)
				}
				got, found, readErr = m.snapshots.cache.Get(ctx, key, entity.RemoteReadCached, 0)
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

// Remote 复制侧的删除按信封版本应用并留水位：同一 key 的 delete 与 upsert 乱序投递时，迟到的 delete v2 不清掉已到的 full v3，
// 先到的 delete v2 之后迟到的 full v1 也不复活已删键。用真实 remoteSyncer 生成 delete 信封、真实 SnapshotReplicaStore 应用，只重排投递顺序。
// 历史：RR-20260913-01。

func deleteOrderSetup(t *testing.T, kind entity.EntityKind, seq int64) (*Manager, SnapshotReplicaStore, entity.RemoteSnapshotKey) {
	t.Helper()
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, err := entity.BuildEntityID(seq, kind)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	return manager, SnapshotReplicaStore{client: manager.snapshots}, entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
}

func deleteOrderUpsert(t *testing.T, key entity.RemoteSnapshotKey, version uint64) mirror.Envelope {
	t.Helper()
	record := entity.RemoteSnapshotRecord{
		Key: key, StateVersion: version, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Codec: 1,
		Full: true, Data: []byte("state"),
	}
	record.Checksum = entity.RemoteSnapshotChecksum(record.Data)
	raw, err := json.Marshal(remoteSnapshotWire{Key: key, Update: record, PublishedAt: time.Now().UnixNano()})
	if err != nil {
		t.Fatal(err)
	}
	return mirror.Envelope{Key: remoteSnapshotReplicaKey(key), Version: int64(version), Op: mirror.OpUpsert, Payload: raw}
}

func deleteOrderDelete(t *testing.T, key entity.RemoteSnapshotKey, version uint64) mirror.Envelope {
	t.Helper()
	raw, err := json.Marshal(remoteSnapshotWire{Delete: true, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	// 与 remoteSyncer.DeleteRemoteSnapshot 发出的信封一致：版本在 env.Version。
	return mirror.Envelope{Key: remoteSnapshotReplicaKey(key), Version: int64(version), Op: mirror.OpUpsert, Payload: raw}
}

// 删除必须带版本（DeleteRemoteSnapshot 总以提交版本发布）。不带版本的删除是坏消息：拒绝、不失效缓存。
func TestApplyReplicaRefusesUnversionedDelete(t *testing.T) {
	manager, store, key := deleteOrderSetup(t, 246, 9343)
	ctx := context.Background()
	if err := store.ApplyReplica(ctx, deleteOrderUpsert(t, key, 3)); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyReplica(ctx, deleteOrderDelete(t, key, 0)); err == nil {
		t.Fatal("an unversioned delete was applied")
	}
	got, ok, err := manager.snapshots.cache.Get(ctx, key, entity.RemoteReadCached, 0)
	if err != nil || !ok || got.StateVersion != 3 {
		t.Fatalf("refused unversioned delete still invalidated v3: found=%v version=%d err=%v", ok, got.StateVersion, err)
	}
}

func TestApplyReplicaPromiseDelayedDeleteKeepsNewerSnapshot(t *testing.T) {
	manager, store, key := deleteOrderSetup(t, 244, 9341)
	ctx := context.Background()
	if err := store.ApplyReplica(ctx, deleteOrderUpsert(t, key, 3)); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyReplica(ctx, deleteOrderDelete(t, key, 2)); err != nil {
		t.Fatal(err)
	}
	got, ok, err := manager.snapshots.cache.Get(ctx, key, entity.RemoteReadCached, 0)
	if err != nil || !ok || got.StateVersion != 3 {
		t.Fatalf("old delete removed v3: found=%v version=%d err=%v", ok, got.StateVersion, err)
	}
}

func TestApplyReplicaPromiseDeleteFencesDelayedOlderSnapshot(t *testing.T) {
	manager, store, key := deleteOrderSetup(t, 245, 9342)
	ctx := context.Background()
	if err := store.ApplyReplica(ctx, deleteOrderDelete(t, key, 2)); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyReplica(ctx, deleteOrderUpsert(t, key, 1)); err != nil {
		t.Fatal(err)
	}
	got, ok, err := manager.snapshots.cache.Get(ctx, key, entity.RemoteReadCached, 0)
	if err != nil || ok {
		t.Fatalf("deleted snapshot resurrected: found=%v version=%d err=%v", ok, got.StateVersion, err)
	}
	// 顺序对照：按序 upsert v1 → delete v2 → upsert v3 分别得到删除与 v3。
	if err := store.ApplyReplica(ctx, deleteOrderUpsert(t, key, 3)); err != nil {
		t.Fatal(err)
	}
	got, ok, err = manager.snapshots.cache.Get(ctx, key, entity.RemoteReadCached, 0)
	if err != nil || !ok || got.StateVersion != 3 {
		t.Fatalf("ordered newer snapshot lost: found=%v version=%d err=%v", ok, got.StateVersion, err)
	}
}

// payloadWithChecksumHighBit finds a snapshot payload whose CRC64 has the top
// bit set, and one whose CRC64 does not. Both are ordinary payloads — that is
// the point of the test below: which half of the hash space a snapshot lands
// in is not something the caller chooses.
func payloadsByChecksumSign(t *testing.T) (low, high []byte) {
	t.Helper()
	for i := 0; i < 10_000 && (low == nil || high == nil); i++ {
		candidate := []byte(fmt.Sprintf("guild-roster-%d", i))
		if entity.RemoteSnapshotChecksum(candidate) > math.MaxInt64 {
			if high == nil {
				high = candidate
			}
			continue
		}
		if low == nil {
			low = candidate
		}
	}
	if low == nil || high == nil {
		t.Fatal("could not find payloads on both sides of MaxInt64")
	}
	return low, high
}

// snapshot 的 checksum 是完整 64 位 CRC，高位为 1 是正常值（约一半载荷）：BSON 没有无符号 64 位整数，checksum 按 8 字节写入，
// 否则这些载荷在投影阶段写不进去、WAL 每次启动恢复都会再失败。用例走真正的提交路径（fake mongo 用真实 bson 编解码每个文档）。
// 历史：RR-20260920-01。
func TestASnapshotCommitsWhicheverHalfOfTheHashSpaceItLandsIn(t *testing.T) {
	low, high := payloadsByChecksumSign(t)
	for name, data := range map[string][]byte{
		"checksum below MaxInt64": low,
		"checksum above MaxInt64": high,
	} {
		t.Run(name, func(t *testing.T) {
			const kind entity.EntityKind = 197
			entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
			id, err := entity.BuildEntityID(991, kind)
			if err != nil {
				t.Fatal(err)
			}
			key := entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
			checksum := entity.RemoteSnapshotChecksum(data)
			var tx entity.RemoteTransactionID
			tx[15] = 1
			commit := entity.RemoteCommit{
				TransactionID: tx, EntityID: id, Kind: kind, BaseVersion: 0, NextVersion: 1,
				MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Codec: 1,
				Mutations: []entity.RemoteDataMutation{{Database: "game", Collection: "guilds", ID: id, Version: 1, Data: data}},
				Snapshots: []entity.RemoteSnapshotRecord{{
					Key: key, BaseVersion: 0, StateVersion: 1, MarkerEpoch: 1, RouteEpoch: 1,
					Schema: 1, Codec: 1, Full: true, Data: data, Checksum: checksum,
				}},
			}
			store := newBoundMongoCommitter(newRemoteMongoFake(), "control", 7, 0)
			if err = store.EnsureRemoteStorage(context.Background()); err != nil {
				t.Fatal(err)
			}
			commit.LockFence = testWriteGrant(t, store, id).Fence
			if _, err = store.CommitRemote(context.Background(), commit); err != nil {
				t.Fatalf("a snapshot whose checksum is %d could not be committed: %v", checksum, err)
			}
			snapshot, ok, err2 := store.LoadRemoteSnapshot(context.Background(), key, entity.RemoteReadLinearizable, 1)
			if err2 != nil || !ok {
				t.Fatalf("snapshot not readable back: ok=%v err=%v", ok, err2)
			}
			if snapshot.Checksum != checksum {
				t.Fatalf("checksum read back as %d, want %d", snapshot.Checksum, checksum)
			}
			if string(snapshot.Payload.BytesCopy()) != string(data) {
				t.Fatal("snapshot payload did not survive the round trip")
			}
		})
	}
}

// 版本 / epoch / fence 也是 uint64，但它们参与 Mongo 的数值比较（快照读用 `$gte`），
// 所以不能改成字节。业务上限就是 MaxInt64，超出必须在**写进任何地方之前**被拒绝，
// 而不是等到投影阶段才失败——那时记录已经持久化，恢复会一遍遍重放它。
func TestACommitWhoseCountersCannotBeEncodedIsRefusedUpFront(t *testing.T) {
	data := []byte("state")
	var tx entity.RemoteTransactionID
	tx[15] = 1
	const kind entity.EntityKind = 196
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, err := entity.BuildEntityID(992, kind)
	if err != nil {
		t.Fatal(err)
	}
	base := entity.RemoteCommit{
		TransactionID: tx, EntityID: id, Kind: kind, BaseVersion: 0, NextVersion: 1,
		MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Codec: 1,
		Mutations: []entity.RemoteDataMutation{{Database: "game", Collection: "guilds", ID: id, Version: 1, Data: data}},
	}
	for name, mutate := range map[string]func(*entity.RemoteCommit){
		// A legitimate increment that lands one past what BSON can hold.
		"next version": func(c *entity.RemoteCommit) {
			c.BaseVersion, c.NextVersion = math.MaxInt64, math.MaxInt64+1
			c.Mutations[0].Version = math.MaxInt64 + 1
		},
		"marker epoch": func(c *entity.RemoteCommit) { c.MarkerEpoch = math.MaxInt64 + 1 },
		"route epoch":  func(c *entity.RemoteCommit) { c.RouteEpoch = math.MaxInt64 + 1 },
		"lock fence":   func(c *entity.RemoteCommit) { c.LockFence = math.MaxInt64 + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			commit := base.Clone()
			mutate(&commit)
			store := newBoundMongoCommitter(newRemoteMongoFake(), "control", 7, 0)
			if err := store.EnsureRemoteStorage(context.Background()); err != nil {
				t.Fatal(err)
			}
			_, commitErr := store.CommitRemote(context.Background(), commit)
			if commitErr == nil {
				t.Fatalf("a %s beyond MaxInt64 was accepted", name)
			}
			// The promise is not which refusal it is — a CAS conflict is a
			// perfectly good one — but that the caller never gets a storage
			// overflow. That error means the record reached the encoder, and
			// on the durable path it means it reached the WAL first.
			if strings.Contains(commitErr.Error(), "overflows int64") {
				t.Fatalf("a %s beyond MaxInt64 failed at the encoder instead of up front: %v", name, commitErr)
			}
			if !errors.Is(commitErr, entity.ErrRemoteRejected) && !errors.Is(commitErr, fmongo.ErrVersionConflict) {
				t.Fatalf("a %s beyond MaxInt64 was refused with an unexpected error: %v", name, commitErr)
			}
		})
	}
}

// checksum 只有 8 字节一种存储形态（所有写入都经 MarshalBSONValue）。旧版本写下的 BSON 数字形态以及
// null / undefined 都不读取：明确报“unexpected BSON type”，不静默读成某个值。
// 历史：RR-20260920-01 之前 checksum 存为数字；未上线，不兼容旧数据（维护者 2026-10-13）。
func TestAChecksumNotStoredAsEightBytesIsRefused(t *testing.T) {
	for name, stored := range map[string]any{
		"int64":     int64(1234567890123),
		"int32":     int32(4242),
		"double":    float64(42),
		"null":      nil,
		"undefined": bson.Undefined{},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := bson.Marshal(bson.M{"checksum": stored})
			if err != nil {
				t.Fatal(err)
			}
			var back struct {
				Checksum entity.RemoteChecksum `bson:"checksum"`
			}
			err = bson.Unmarshal(raw, &back)
			if err == nil || !strings.Contains(err.Error(), "remote checksum has unexpected BSON type") {
				t.Fatalf("a %s checksum was read as %d (err=%v); want it refused", name, uint64(back.Checksum), err)
			}
		})
	}
}

// 新形态写出去的就是 8 字节，不是数字——否则"兼容旧的"会被误读成"还在写旧的"。
func TestAChecksumIsStoredAsEightBytes(t *testing.T) {
	raw, err := bson.Marshal(bson.M{"checksum": entity.RemoteChecksum(1)})
	if err != nil {
		t.Fatal(err)
	}
	var back bson.M
	if err := bson.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	binaryValue, ok := back["checksum"].(bson.Binary)
	if !ok {
		t.Fatalf("checksum stored as %T, want bson.Binary", back["checksum"])
	}
	if len(binaryValue.Data) != 8 {
		t.Fatalf("checksum stored as %d bytes, want 8", len(binaryValue.Data))
	}
}
