package remoteentity

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// RR-20261006-68：兴趣续租的广播不能拖住同条带其他 key 的读。
//
// 修前 renewInterest 在条带锁 localInterestLocks[EntityID%64] 里同步调 publishInterest；总线慢时（JetStream
// 卡住最长 syncbus.publish_timeout，缺省 5s）同条带的其他 key——包括不需要续租的 L1 命中——都排队等这次
// 发布。承诺：条带锁只保护代际分配与本机表 / 兴趣表的更新，广播在锁外；同条带另一个 key 的 L1 命中不等
// 别人的广播。续租失败的回滚（rollbackLocalInterest）不变。
type slowInterestPublisher struct {
	capturingInterestPublisher
	delay   atomic.Int64
	entered chan entity.RemoteSnapshotKey
	fail    atomic.Bool
}

func (p *slowInterestPublisher) PublishRemoteInterest(_ context.Context, interest entity.RemoteSnapshotInterest, _ bool) error {
	if d := time.Duration(p.delay.Load()); d > 0 {
		select {
		case p.entered <- interest.Key:
		default:
		}
		time.Sleep(d)
	}
	if p.fail.Load() {
		return entity.ErrRemoteOverloaded
	}
	return nil
}

func sameStripeKeys(t *testing.T, base int64) (entity.RemoteSnapshotKey, entity.RemoteSnapshotKey) {
	t.Helper()
	first := staleBackfillKey(t, b2WatermarkKind, base)
	stripes := uint64(len((&SnapshotClient{}).localInterestLocks))
	for unique := base + 1; unique < base+10_000; unique++ {
		other := staleBackfillKey(t, b2WatermarkKind, unique)
		if uint64(other.EntityID)%stripes == uint64(first.EntityID)%stripes {
			return first, other
		}
	}
	t.Fatal("no second key on the same stripe")
	return first, first
}

func TestInterestPublishDoesNotBlockSameStripeCacheHits(t *testing.T) {
	step4RegisterDelta(t)
	ctx := context.Background()
	slowKey, hitKey := sameStripeKeys(t, 96_700)
	client, err := NewSnapshotClient(nil, SnapshotClientDeps{ConsumerSID: 2671,
		Loader: func(_ context.Context, key entity.RemoteSnapshotKey, _ entity.RemoteReadConsistency, _ uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
			return step4Full(key, 3, "v3"), true, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	publisher := &slowInterestPublisher{entered: make(chan entity.RemoteSnapshotKey, 1)}
	client.transport = publisher

	// hitKey 先装进 L1、续上一整段租约：之后的读既是 L1 命中，也不需要续租。
	if _, found, err := client.ReadSnapshot(ctx, entity.RemoteSnapshotRead{Key: hitKey, Consistency: entity.RemoteReadMonotonic}); err != nil || !found {
		t.Fatalf("warm read: found=%v err=%v", found, err)
	}

	publisher.delay.Store(int64(500 * time.Millisecond))
	slowDone := make(chan error, 1)
	go func() {
		_, _, err := client.ReadSnapshot(ctx, entity.RemoteSnapshotRead{Key: slowKey, Consistency: entity.RemoteReadMonotonic})
		slowDone <- err
	}()
	select {
	case <-publisher.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the renewal of slowKey never reached the publisher")
	}

	started := time.Now()
	got, found, err := client.ReadSnapshot(ctx, entity.RemoteSnapshotRead{Key: hitKey, Consistency: entity.RemoteReadCached})
	elapsed := time.Since(started)
	if err != nil || !found || got.StateVersion != 3 {
		t.Fatalf("cache hit: v%d found=%v err=%v", got.StateVersion, found, err)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("an L1 hit on the same stripe waited %v for another key's interest publish", elapsed)
	}
	if err := <-slowDone; err != nil {
		t.Fatal(err)
	}
}

// 回滚语义不变：广播失败后本机表撤掉这次续租，下一次读同一个 key 再续一次。
func TestInterestPublishFailureStillRollsBackOutsideTheStripeLock(t *testing.T) {
	step4RegisterDelta(t)
	ctx := context.Background()
	key, _ := sameStripeKeys(t, 96_900)
	client, err := NewSnapshotClient(nil, SnapshotClientDeps{ConsumerSID: 2672})
	if err != nil {
		t.Fatal(err)
	}
	publisher := &slowInterestPublisher{entered: make(chan entity.RemoteSnapshotKey, 1)}
	client.transport = publisher
	publisher.fail.Store(true)
	if err := client.RenewInterest(ctx, key); err == nil {
		t.Fatal("a failed publish must surface as a renewal error")
	}
	if n := client.Stats().LocalInterests; n != 0 {
		t.Fatalf("a failed publish left %d local interests", n)
	}
	if client.interests.interested(key) {
		t.Fatal("a failed publish left the lease in the local interest registry")
	}
	publisher.fail.Store(false)
	if err := client.RenewInterest(ctx, key); err != nil {
		t.Fatal(err)
	}
	if n := client.Stats().LocalInterests; n != 1 || !client.interests.interested(key) {
		t.Fatalf("the retry after a failed publish did not renew: local=%d", n)
	}
}
