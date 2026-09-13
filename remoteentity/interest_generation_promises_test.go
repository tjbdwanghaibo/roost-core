package remoteentity

import (
	"context"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

func interestKeyFor(t *testing.T, kind entity.EntityKind, unique int64) entity.RemoteSnapshotKey {
	t.Helper()
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, err := entity.BuildEntityID(unique, kind)
	if err != nil {
		t.Fatal(err)
	}
	return entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
}

// U-0184 · C8 · RR-20260913-02:迟到的旧 release 不能取消更新的 renewal。
//
// 接收端按 key/SID 无条件删除,没有订阅代际比较:renew → release → renew 三条消息,接收侧先收到
// 新 renewal,再收到迟到的旧 release,当前兴趣被删掉;syncer 的兴趣过滤随后静默跳过本该发送的
// 快照更新,而本地仍认为兴趣有效。ExpiresAt 不能当版本用:合法 release 的发布时刻通常早于它
// 撤销的 lease 到期时刻。所以 renew / release 各带一个可比较、不可复用的 Generation,
// 只释放代际不落后的订阅。
func TestStaleInterestReleaseDoesNotCancelANewerRenewal(t *testing.T) {
	key := interestKeyFor(t, 244, 9341)
	registry := newRemoteInterestRegistry()
	now := time.Now().UnixNano()
	const sid int32 = 7

	first := entity.RemoteSnapshotInterest{ConsumerSID: sid, Key: key, ExpiresAt: now + int64(time.Minute), Generation: 10}
	release := entity.RemoteSnapshotInterest{ConsumerSID: sid, Key: key, ExpiresAt: now, Generation: 11}
	second := entity.RemoteSnapshotInterest{ConsumerSID: sid, Key: key, ExpiresAt: now + int64(2*time.Minute), Generation: 12}

	if err := registry.renew(first); err != nil {
		t.Fatal(err)
	}
	// The newer renewal arrives before the release it was issued after.
	if err := registry.renew(second); err != nil {
		t.Fatal(err)
	}
	registry.release(release.Key, release.ConsumerSID, release.Generation)
	if !registry.interested(key) {
		t.Fatal("an older release canceled the newer renewal")
	}

	// A release issued AFTER the current renewal does cancel it.
	registry.release(key, sid, 13)
	if registry.interested(key) {
		t.Fatal("a release newer than the renewal must cancel it")
	}

	// A renewal older than what is recorded is ignored rather than moving
	// the lease backwards.
	if err := registry.renew(second); err != nil {
		t.Fatal(err)
	}
	stale := first
	stale.ExpiresAt = now + int64(10*time.Minute)
	if err := registry.renew(stale); err != nil {
		t.Fatal(err)
	}
	registry.mu.Lock()
	lease := registry.entries[key][sid]
	registry.mu.Unlock()
	if lease.generation != second.Generation || lease.expiresAt != second.ExpiresAt {
		t.Fatalf("a stale renewal overwrote the current lease: %+v", lease)
	}
}

// The manager stamps every renewal and release with a generation that only
// moves forward, and survives a restart without going backwards.
func TestManagerStampsInterestMessagesWithAdvancingGenerations(t *testing.T) {
	key := interestKeyFor(t, 245, 9342)
	captured := make([]entity.RemoteSnapshotInterest, 0, 3)
	publisher := &capturingInterestPublisher{onPublish: func(i entity.RemoteSnapshotInterest, _ bool) {
		captured = append(captured, i)
	}}
	manager := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	manager.syncer = publisher

	ctx := context.Background()
	if err := manager.RenewRemoteSnapshotInterest(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := manager.ReleaseRemoteSnapshotInterest(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := manager.RenewRemoteSnapshotInterest(ctx, key); err != nil {
		t.Fatal(err)
	}
	if len(captured) != 3 {
		t.Fatalf("captured %d interest messages, want 3", len(captured))
	}
	for i := 1; i < len(captured); i++ {
		if captured[i].Generation <= captured[i-1].Generation {
			t.Fatalf("generations do not advance: %d then %d", captured[i-1].Generation, captured[i].Generation)
		}
	}
	// A fresh manager (a restart reusing the SID) starts above where any
	// earlier process could plausibly have been, because the seed is a clock.
	restarted := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	restarted.syncer = publisher
	if err := restarted.RenewRemoteSnapshotInterest(ctx, key); err != nil {
		t.Fatal(err)
	}
	if captured[3].Generation <= captured[2].Generation {
		t.Fatalf("a restarted manager issued generation %d, not above the previous %d", captured[3].Generation, captured[2].Generation)
	}
}

// capturingInterestPublisher is a remoteSyncTransport that records interest
// messages and drops snapshot traffic.
type capturingInterestPublisher struct {
	onPublish func(entity.RemoteSnapshotInterest, bool)
}

func (*capturingInterestPublisher) PublishRemoteSnapshot(context.Context, entity.RemoteSnapshotRecord) error {
	return nil
}

func (*capturingInterestPublisher) DeleteRemoteSnapshot(context.Context, entity.RemoteSnapshotKey, uint64) error {
	return nil
}

func (p *capturingInterestPublisher) PublishRemoteInterest(_ context.Context, interest entity.RemoteSnapshotInterest, release bool) error {
	if p.onPublish != nil {
		p.onPublish(interest, release)
	}
	return nil
}
