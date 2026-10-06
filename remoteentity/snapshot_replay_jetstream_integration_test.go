//go:build integration

package remoteentity

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
	gojs "github.com/nats-io/nats.go/jetstream"

	"github.com/tjbdwanghaibo/roost-core/entity"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	natsdriver "github.com/tjbdwanghaibo/roost-core/nats/driver"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/sync/syncbus"
	syncdriver "github.com/tjbdwanghaibo/roost-core/sync/syncbus/driver"
	"github.com/tjbdwanghaibo/roost-core/sync/syncbus/mirror"
)

// B2 · N05 O5 在真实 JetStream + 真实 Redis 上的复现：同步总线给每个 sid 一个 DeliverAll 的 durable，
// 新 sid（扩容、换 sid 重启）启动时重放保留期内的全部快照消息。owner 发布 v1（有兴趣，进流），之后提交
// v2（兴趣已过期，不进流），键在 snapshot_l2_ttl 内没有写入、L2 过期。新节点加入时重放 v1：修前 CAS
// 面对空键接受了它，v1 同时写进共享 L2 与新节点 L1，所有冷节点的 Cached 读都看到被取代的 v1。
//
// 隔离：NATS 流名 / 前缀与 L2 部署前缀都带 b2l2.<pid>.<ns>，内存存储，用例结束删流（durable 随流删除）
// 并逐键删 Redis。以 ROOST_DATAENGINE_IT 与 ROOST_DATAENGINE_IT_NATS_URL 准入。单独运行：
//
//	go test -tags integration -run '^TestRealJetStreamReplayAfterL2ExpiryDoesNotResurrect$' ./remoteentity
func TestRealJetStreamReplayAfterL2ExpiryDoesNotResurrect(t *testing.T) {
	r := realRemoteRedis(t)
	natsURL := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if natsURL == "" {
		t.Skip("ROOST_DATAENGINE_IT_NATS_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	unique := fmt.Sprintf("%d%d", os.Getpid(), time.Now().UnixNano()%1_000_000_000)
	syncPrefix := "b2l2.p" + unique + ".sync"
	l2Prefix := "b2l2:" + unique
	const l2TTL = 2 * time.Second
	key := staleBackfillKey(t, b2WatermarkMatrixKind, 9601)
	redisKey := l2Prefix + ":" + remoteSnapshotL2Key(key)
	b27DeleteKeys(t, r, redisKey)

	busFor := func(sid int32) fsyncbus.ISyncBus {
		client, err := natsdriver.NewClient(fnats.DefaultConfig(natsURL), natsdriver.ClientOptions{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(client.Close)
		js, err := natsdriver.NewJetStreamClient(client)
		if err != nil {
			t.Fatal(err)
		}
		bus, err := syncdriver.NewJetStreamSyncBus(ctx, js, syncdriver.JetStreamSyncConfig{
			LocalSid: sid, Prefix: syncPrefix, Storage: fnats.JetStreamStorageMemory, StreamMaxAge: 10 * time.Minute,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer stopCancel()
			_ = bus.StopWithContext(stopCtx)
		})
		return bus
	}
	t.Cleanup(func() {
		nc, err := gonats.Connect(natsURL, gonats.Timeout(2*time.Second))
		if err != nil {
			t.Logf("cleanup connect: %v", err)
			return
		}
		defer nc.Close()
		js, err := gojs.New(nc)
		if err != nil {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		t.Logf("delete stream %s: %v", syncdriver.JetStreamSyncStream(syncPrefix), js.DeleteStream(cleanupCtx, syncdriver.JetStreamSyncStream(syncPrefix)))
	})

	cfg := DefaultConfig()
	cfg.SnapshotL2TTL = l2TTL
	ownerL2, err := NewSnapshotL2StoreWithKeyPrefix(r, l2TTL, l2Prefix)
	if err != nil {
		t.Fatal(err)
	}
	owner := NewManager(newMockVersionedLockFactory(), cfg, 2101, ownerL2)
	ownerSnapshots, ownerInterests := owner.BindSync(busFor(2101))
	defer ownerSnapshots.Stop()
	defer ownerInterests.Stop()

	// 一个消费者对这个 key 有兴趣：owner 的提交进流。
	if err := owner.snapshots.interests.renew(entity.RemoteSnapshotInterest{ConsumerSID: 2102, Key: key, ExpiresAt: time.Now().Add(time.Minute).UnixNano(), Generation: 1}); err != nil {
		t.Fatal(err)
	}
	v1 := staleBackfillEnvelope(key, 1, 1, "v1")
	if err := owner.snapshots.cache.Publish(ctx, v1); err != nil {
		t.Fatal(err)
	}
	publisher, ok := owner.snapshots.transport.(entity.IRemoteSnapshotPublisher)
	if !ok {
		t.Fatal("owner has no snapshot publisher")
	}
	if err := publisher.PublishRemoteSnapshot(ctx, entity.RemoteSnapshotRecord{
		Key: key, StateVersion: 1, BaseVersion: 0, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Codec: 1, Full: true,
		Data: []byte("v1"), Checksum: entity.RemoteSnapshotChecksum([]byte("v1")),
	}); err != nil {
		t.Fatal(err)
	}
	// 兴趣过期后 owner 提交 v2：只写 L2，不进流。
	owner.snapshots.interests.release(key, 2102, 2)
	if err := owner.snapshots.cache.Publish(ctx, staleBackfillEnvelope(key, 2, 1, "v2")); err != nil {
		t.Fatal(err)
	}
	// 键在 L2 TTL 内没有新的写入：等它过期。
	deadline := time.Now().Add(10 * time.Second)
	for {
		n, err := r.Exists(ctx, redisKey)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("L2 key did not expire")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 新 sid 加入：它的 durable 从头重放。
	lateL2, err := NewSnapshotL2StoreWithKeyPrefix(r, l2TTL, l2Prefix)
	if err != nil {
		t.Fatal(err)
	}
	late := NewManager(newMockVersionedLockFactory(), cfg, 2103, lateL2)
	var applied atomic.Int32
	lateReplicator := mirror.New(busFor(2103), SyncTopicSnapshot, countingReplicaStore{inner: SnapshotReplicaStore{client: late.snapshots}, applied: &applied})
	if err := lateReplicator.Start(); err != nil {
		t.Fatal(err)
	}
	defer lateReplicator.Stop()
	deadline = time.Now().Add(10 * time.Second)
	for applied.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the late joiner never received the replayed history")
		}
		time.Sleep(50 * time.Millisecond)
	}

	got, found, err := late.ReadRemoteSnapshot(ctx, key, entity.RemoteReadCached, 0)
	stored, held, l2Err := lateL2.Get(ctx, key)
	t.Logf("replayed messages applied=%d; late joiner Cached read found=%v payload=%q err=%v; L2 held=%v version=%d err=%v",
		applied.Load(), found, got.Payload.BytesCopy(), err, held, stored.StateVersion, l2Err)
	if found && got.StateVersion == 1 {
		t.Fatalf("the late joiner serves the replayed v1 (authority is at v2)")
	}
	if held && stored.StateVersion == 1 {
		t.Fatalf("the replayed v1 was written back into the shared L2; every cold node now reads it")
	}
}

type countingReplicaStore struct {
	inner   SnapshotReplicaStore
	applied *atomic.Int32
}

func (s countingReplicaStore) ApplyReplica(ctx context.Context, env mirror.Envelope) error {
	err := s.inner.ApplyReplica(ctx, env)
	s.applied.Add(1)
	return err
}
