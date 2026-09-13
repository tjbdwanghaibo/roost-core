package entity

import (
	"context"
	"errors"
	"testing"
	"time"
)

// U-0187 复核补修 · C8 · RR-20260913-01 残余:删除水位必须是**所有** L1 写入路径共用的准入,
// L2 的删除也要带版本。第一版只在 Publish 里查墓碑、DeleteAtVersion 只比较 L1:
//   1. Get 已从 L2 捕获 v1 尚未回填,DeleteAtVersion(2) 走完,回填经 ReadThrough.loadOne 直接写 L1,
//      墓碑有效期内旧值复活;
//   2. L1 为空、L2 已有 v3,收到旧删除 v2:L2 被无条件 Del,较新的共享缓存被旧删除清掉。
//
// U-0180 复核补修 · C8 · RR-20260913-05 残余:Publish 的 L2 预检查与 L2.Set 不是同一原子边界。
//   3. B 预检查读到 L2 miss 后暂停,另一发布者写入同版本 A;放行 B,L2.Set 正确拒绝,
//      但 ReadThrough.Set 在 IgnoreRemoteError 下吞掉了这个语义冲突,B 落进 L1、返回 nil。

func TestDeleteFenceCoversInflightL2Refill(t *testing.T) {
	ctx := context.Background()
	key := l2ConflictKey(t, 240, 9801)
	l2 := newL2Fake()
	c := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{TTL: time.Minute, TombstoneTTL: time.Minute}, l2, nil)
	a := snapshotAt(key, 1, "A")
	a.Checksum = RemoteSnapshotChecksum(a.Payload.data)
	if err := l2.Set(ctx, a); err != nil {
		t.Fatal(err)
	}
	entered, resume := make(chan struct{}), make(chan struct{})
	l2.getEntered, l2.getResume = entered, resume
	done := make(chan bool, 1)
	go func() {
		_, ok, _ := c.Get(ctx, key, RemoteReadCached, 0)
		done <- ok
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no L2 read")
	}
	err := c.DeleteAtVersion(ctx, key, 2)
	close(resume)
	if err != nil {
		t.Fatal(err)
	}
	ok := <-done
	_, cached, _ := c.local.Get(ctx, key)
	if ok || cached {
		t.Fatalf("deleted v1 refilled through live tombstone: returned=%v cached=%v", ok, cached)
	}
}

func TestColdL1DeletePreservesNewerL2(t *testing.T) {
	ctx := context.Background()
	key := l2ConflictKey(t, 241, 9802)
	l2 := newL2Fake()
	c := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{TTL: time.Minute}, l2, nil)
	a := snapshotAt(key, 3, "new")
	a.Checksum = RemoteSnapshotChecksum(a.Payload.data)
	if err := l2.Set(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteAtVersion(ctx, key, 2); err != nil {
		t.Fatal(err)
	}
	got, ok, _ := l2.Get(ctx, key)
	if !ok || got.StateVersion != 3 {
		t.Fatalf("old delete removed newer L2: found=%v version=%d", ok, got.StateVersion)
	}
	// 顺序对照:不比 L2 旧的删除照常清掉 L2。
	if err := c.DeleteAtVersion(ctx, key, 3); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := l2.Get(ctx, key); ok {
		t.Fatal("delete at the stored version must remove the L2 copy")
	}
}

func TestPublishConflictAfterPreflight(t *testing.T) {
	ctx := context.Background()
	key := l2ConflictKey(t, 242, 9803)
	l2 := newL2Fake()
	c := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{TTL: time.Minute}, l2, nil)
	entered, resume := make(chan struct{}), make(chan struct{})
	l2.getEntered, l2.getResume = entered, resume
	done := make(chan error, 1)
	go func() { done <- c.Publish(ctx, snapshotAt(key, 5, "B")) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no preflight")
	}
	a := snapshotAt(key, 5, "A")
	a.Checksum = RemoteSnapshotChecksum(a.Payload.data)
	err := l2.Set(ctx, a)
	close(resume)
	if err != nil {
		t.Fatal(err)
	}
	err = <-done
	local, ok, _ := c.local.Get(ctx, key)
	remote, _, _ := l2.Get(ctx, key)
	if !errors.Is(err, ErrRemoteVersionConflict) || ok {
		t.Fatalf("conflict swallowed after preflight: err=%v L1=%q L2=%q", err, local.Payload.BytesCopy(), remote.Payload.BytesCopy())
	}
	// 降级契约保留:L2 纯故障时 Publish 仍成功并写 L1。
	outage := newL2Fake()
	outage.setErr = errors.New("dial tcp: connection refused")
	degraded := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{TTL: time.Minute}, outage, nil)
	if err := degraded.Publish(ctx, snapshotAt(key, 6, "C")); err != nil {
		t.Fatalf("an L2 outage must still degrade to L1: %v", err)
	}
	if _, ok, _ := degraded.local.Get(ctx, key); !ok {
		t.Fatal("degraded publish did not reach L1")
	}
}
