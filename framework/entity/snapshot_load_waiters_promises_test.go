package entity

import (
	"context"
	"errors"
	"testing"
	"time"
)

// U-0174 · C2 · RR-20260913-04：合并加载的等待名额在跟随者取消后归还。
//
// loadMonotonic 的跟随者 `call.waiters++` 之后,ctx 取消就直接 return,计数不减。于是
// MaxWaiters 计的是"本次 load 期间累计进过门的次数"而不是"正在等的人数":慢权威后端叠加
// 超时重试,健康请求会在首个 load 结束前一直收到 ErrRemoteOverloaded。
//
// 这与 cache.ReadThroughStore 的 RR-20260908-02 / U-0155 是同一个错误,但那是另一个包;
// entity 这一层自建的合并逻辑各写了一份,所以要各修一次。
func TestSnapshotLoadReturnsACanceledWaitersSlot(t *testing.T) {
	const kind EntityKind = 231
	MustRegisterEntityKindDefs(EntityKindDef{Kind: kind, Category: 1, RemotePolicy: RemotePolicyManaged})
	id, err := BuildEntityID(9311, kind)
	if err != nil {
		t.Fatal(err)
	}
	key := RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}

	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	loader := func(ctx context.Context, k RemoteSnapshotKey, _ RemoteReadConsistency, _ uint64) (RemoteSnapshotEnvelope, bool, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return RemoteSnapshotEnvelope{
			Key: k, StateVersion: 5, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Full: true,
			Payload: CopyFrozenRemoteSnapshotPayload([]byte("authority")),
		}, true, nil
	}
	cache := NewRemoteSnapshotCache(
		RemoteSnapshotCacheConfig{Shards: 1, MaxEntries: 4, MaxBytes: 1024, TTL: time.Minute, MaxWaiters: 1},
		nil, loader)

	leader := make(chan error, 1)
	go func() {
		_, _, err := cache.Get(context.Background(), key, RemoteReadMonotonic, 5)
		leader <- err
	}()
	awaitSignal(t, entered, "the authoritative load to start")
	waitForLoadWaiters(t, cache, 0)

	// One follower joins and takes the only slot.
	followerCtx, cancelFollower := context.WithCancel(context.Background())
	follower := make(chan error, 1)
	go func() {
		_, _, err := cache.Get(followerCtx, key, RemoteReadMonotonic, 5)
		follower <- err
	}()
	waitForLoadWaiters(t, cache, 1)

	// While it is really waiting, the slot is taken: a second follower is refused.
	if _, _, err := cache.Get(context.Background(), key, RemoteReadMonotonic, 5); !errors.Is(err, ErrRemoteOverloaded) {
		t.Fatalf("a live follower must hold the only slot, got %v", err)
	}

	// It leaves. The slot it was holding has to come back.
	cancelFollower()
	if err := awaitChan(t, follower, "the canceled follower to return"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled follower = %v", err)
	}
	waitForLoadWaiters(t, cache, 0)

	replacement := make(chan error, 1)
	go func() {
		_, _, err := cache.Get(context.Background(), key, RemoteReadMonotonic, 5)
		replacement <- err
	}()
	waitForLoadWaiters(t, cache, 1)

	close(release)
	if err := awaitChan(t, leader, "the leader to finish"); err != nil {
		t.Fatalf("leader = %v", err)
	}
	if err := awaitChan(t, replacement, "the replacement follower to finish"); err != nil {
		t.Fatalf("replacement follower = %v; it was admitted, so it must get the loaded value", err)
	}
}

func waitForLoadWaiters(t *testing.T, c *RemoteSnapshotCache, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.loadMu.Lock()
		got := 0
		for _, call := range c.loads {
			got += call.waiters
		}
		c.loadMu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("load waiters = %d, want %d", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func awaitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}
