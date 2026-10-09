package remoteentity

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// Mirror 方案第 2 步（统一快照准入与所有读出口，docs/feature/MIRROR-STEPS-1-3-2026-10-06.md）：
// B2 之后快照缓存自己的 Get 已经在陈旧上限内确认、Monotonic 不足时合并回源；Manager.ReadRemoteSnapshot
// 却还保留着 B2 之前的外层回退——Get 回源之后，结果不满足（未找到 / 低于最低版本）就再等 2ms、再直接
// LoadAuthoritative 一次。第二次加载不经合并（coalesce），也不受等待名额（RR-20260913-04）约束：一次
// Monotonic 未命中回源两次，N 个并发读者各自多打一次权威。
//
// 承诺：一个读出口、一次回源。Monotonic 未命中只回源一次；并发读者合并成一次。
//
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

// 修前：Get 的 Monotonic 回源未找到之后，ReadRemoteSnapshot 又直接 LoadAuthoritative 一次（loads=2）。
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

// 修前：权威低于最低版本时 Get 回源得到 ErrRemoteSnapshotStale，外层等 2ms 再直接加载一次（loads=2）。
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
