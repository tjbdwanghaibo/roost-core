package entity

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/cache"
)

// l2Fake is a Store[RemoteSnapshotKey, RemoteSnapshotEnvelope] with the same
// same-version rule the real L2 enforces, plus an optional barrier on Get so a
// read can be paused between capturing a value and returning it.
type l2Fake struct {
	mu     sync.Mutex
	values map[RemoteSnapshotKey]RemoteSnapshotEnvelope
	// getEntered / getResume, when set, pin the next Get after it captured
	// its value and before it returns.
	getEntered chan struct{}
	getResume  chan struct{}
	setErr     error
}

func newL2Fake() *l2Fake { return &l2Fake{values: map[RemoteSnapshotKey]RemoteSnapshotEnvelope{}} }

func (f *l2Fake) Get(_ context.Context, key RemoteSnapshotKey) (RemoteSnapshotEnvelope, bool, error) {
	f.mu.Lock()
	value, ok := f.values[key]
	entered, resume := f.getEntered, f.getResume
	f.getEntered, f.getResume = nil, nil
	f.mu.Unlock()
	if entered != nil {
		close(entered)
		<-resume
	}
	return value, ok, nil
}

func (f *l2Fake) Set(_ context.Context, value RemoteSnapshotEnvelope) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return f.setErr
	}
	if current, ok := f.values[value.Key]; ok &&
		current.MarkerEpoch == value.MarkerEpoch && current.RouteEpoch == value.RouteEpoch && current.StateVersion == value.StateVersion {
		if current.Checksum != value.Checksum || current.Schema != value.Schema || current.Codec != value.Codec {
			return ErrRemoteVersionConflict
		}
	}
	f.values[value.Key] = value
	return nil
}

func (f *l2Fake) Delete(_ context.Context, key RemoteSnapshotKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.values, key)
	return nil
}

var _ cache.Store[RemoteSnapshotKey, RemoteSnapshotEnvelope] = (*l2Fake)(nil)

func l2ConflictKey(t *testing.T, kind EntityKind, unique int64) RemoteSnapshotKey {
	t.Helper()
	MustRegisterEntityKindDefs(EntityKindDef{Kind: kind, Category: 1, RemotePolicy: RemotePolicyManaged})
	id, err := BuildEntityID(unique, kind)
	if err != nil {
		t.Fatal(err)
	}
	return RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
}

func snapshotAt(key RemoteSnapshotKey, version uint64, body string) RemoteSnapshotEnvelope {
	return RemoteSnapshotEnvelope{
		Key: key, StateVersion: version, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Codec: 1, Full: true,
		Payload: CopyFrozenRemoteSnapshotPayload([]byte(body)),
	}
}

// U-0180 · C8 · RR-20260913-05:L2 的版本冲突是一致性错误,不能被当成可降级的可用性故障吞掉。
//
// 缓存对 L2 固定 IgnoreRemoteError:true,ReadThrough 对 remote.Set 的一切错误统一降级再写 L1。
// L2 已存同 key/epoch/version 的 A,一个 L1 为空的进程 Publish 同版本的 B:L2 正确返回
// ErrRemoteVersionConflict,却被吞掉,Publish 返回 nil,L1=B、L2=A —— 同一个版本在不同进程返回
// 不同内容,而发布方收到成功。网络故障下保留 L1 可用是对的;语义冲突不是网络故障。
func TestPublishSurfacesAnL2VersionConflictAndKeepsItOutOfL1(t *testing.T) {
	key := l2ConflictKey(t, 234, 9314)
	l2 := newL2Fake()
	c := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{Shards: 1, MaxEntries: 4, MaxBytes: 4096, TTL: time.Minute, MaxWaiters: 4}, l2, nil)

	a := snapshotAt(key, 5, "A")
	a.Checksum = RemoteSnapshotChecksum(a.Payload.data)
	if err := l2.Set(context.Background(), a); err != nil {
		t.Fatal(err)
	}

	b := snapshotAt(key, 5, "B")
	err := c.Publish(context.Background(), b)
	if !errors.Is(err, ErrRemoteVersionConflict) {
		t.Fatalf("publishing a conflicting same-version value = %v, want ErrRemoteVersionConflict", err)
	}
	if got, ok, _ := c.local.Get(context.Background(), key); ok {
		t.Fatalf("the conflicting value reached L1: %q", got.Payload.BytesCopy())
	}

	// A plain L2 outage is still degradable: L1 takes the value, Publish succeeds.
	outage := newL2Fake()
	outage.setErr = errors.New("connection refused")
	degraded := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{Shards: 1, MaxEntries: 4, MaxBytes: 4096, TTL: time.Minute, MaxWaiters: 4}, outage, nil)
	if err := degraded.Publish(context.Background(), snapshotAt(key, 5, "B")); err != nil {
		t.Fatalf("an L2 outage must still degrade to L1: %v", err)
	}
	if _, ok, _ := degraded.local.Get(context.Background(), key); !ok {
		t.Fatal("degraded publish did not populate L1")
	}
}

// U-0181 · C8 · RR-20260913-06:从 L2 回填 L1 必须走和 Publish 一样的同版本内容规则。
//
// ReadThrough 的 L2 命中直接 setLocal(value)。确定性交错:L1 为空,一次读在 L2 取到 A 后暂停;
// 另一路 Publish 同版本的 B 成功写进 L1;放行之前那次读 —— 回填把 L1 改回 A,没有任何错误。
// 所有写入 L1 的入口都要共用一次原子的版本/内容校验,不能只在 Publish 外层加锁。
func TestL2BackfillCannotOverwriteAPublishedSameVersionValue(t *testing.T) {
	key := l2ConflictKey(t, 235, 9315)
	l2 := newL2Fake()
	c := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{Shards: 1, MaxEntries: 4, MaxBytes: 4096, TTL: time.Minute, MaxWaiters: 4}, l2, nil)

	a := snapshotAt(key, 5, "A")
	a.Checksum = RemoteSnapshotChecksum(a.Payload.data)
	if err := l2.Set(context.Background(), a); err != nil {
		t.Fatal(err)
	}

	// Pin a read after it captured A from L2 and before it backfills L1.
	entered, resume := make(chan struct{}), make(chan struct{})
	l2.mu.Lock()
	l2.getEntered, l2.getResume = entered, resume
	l2.mu.Unlock()
	type result struct {
		value RemoteSnapshotEnvelope
		ok    bool
		err   error
	}
	read := make(chan result, 1)
	go func() {
		v, ok, err := c.Get(context.Background(), key, RemoteReadCached, 0)
		read <- result{v, ok, err}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the read never reached L2")
	}

	// Meanwhile B is published for the same version. L2 refuses it (A is
	// there), which the publish surfaces; but that is not this test's point —
	// what matters is that L1 now holds B and the paused backfill must not
	// undo it. Use a fresh L2-less publish path to seed L1 with B.
	l2.mu.Lock()
	l2.values[key] = snapshotAt(key, 5, "B")
	l2.values[key] = func(v RemoteSnapshotEnvelope) RemoteSnapshotEnvelope {
		v.Checksum = RemoteSnapshotChecksum(v.Payload.data)
		return v
	}(l2.values[key])
	l2.mu.Unlock()
	if err := c.Publish(context.Background(), snapshotAt(key, 5, "B")); err != nil {
		t.Fatalf("publishing B: %v", err)
	}
	if got, ok, _ := c.local.Get(context.Background(), key); !ok || string(got.Payload.BytesCopy()) != "B" {
		t.Fatalf("control: L1 does not hold B after publish (ok=%v)", ok)
	}

	close(resume)
	r := <-read
	if r.err != nil {
		t.Fatalf("the paused read failed: %v", r.err)
	}
	if got, ok, _ := c.local.Get(context.Background(), key); !ok || string(got.Payload.BytesCopy()) != "B" {
		t.Fatalf("the L2 backfill overwrote the published same-version value: L1=%q", got.Payload.BytesCopy())
	}
}
