// 兴趣表：作用域与过期、按 consumer 的容量、代际与撤销水位、payload 身份校验、续租广播不阻塞读。

package remoteentity

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus/mirror"
	"github.com/tjbdwanghaibo/roost-core/internal/stopcontract"
)

func TestRemoteInterestRegistryIsScopedAndExpires(t *testing.T) {
	const kind entity.EntityKind = 128
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, err := entity.BuildEntityID(1902, kind)
	if err != nil {
		t.Fatal(err)
	}
	registry := newRemoteInterestRegistry(remoteInterestLimits{})
	key := entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 3, Policy: 2}
	if err := registry.renew(entity.RemoteSnapshotInterest{ConsumerSID: 1001, Key: key, ExpiresAt: time.Now().Add(time.Second).UnixNano(), Generation: 1}); err != nil {
		t.Fatal(err)
	}
	if !registry.interested(key) {
		t.Fatal("renewed interest was not visible")
	}
	otherPolicy := key
	otherPolicy.Policy++
	if registry.interested(otherPolicy) {
		t.Fatal("interest leaked into another LOD policy")
	}
	_ = registry.renew(entity.RemoteSnapshotInterest{ConsumerSID: 1001, Key: otherPolicy, ExpiresAt: time.Now().Add(-time.Second).UnixNano()})
	if registry.interested(otherPolicy) {
		t.Fatal("expired interest was retained")
	}
	registry.release(key, 1001, 2)
	if registry.interested(key) {
		t.Fatal("released interest was retained")
	}
}

func TestLocalInterestCapacityPrunesExpiredAndCoalescesConcurrentRenewal(t *testing.T) {
	const kind entity.EntityKind = 131
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	firstID, err := entity.BuildEntityID(1910, kind)
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := entity.BuildEntityID(1911, kind)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.SnapshotInterestKeys = 1
	cfg.SnapshotInterestTTL = time.Minute
	mgr := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	first := entity.RemoteSnapshotKey{EntityID: firstID, Kind: kind, Scope: 1}
	second := entity.RemoteSnapshotKey{EntityID: secondID, Kind: kind, Scope: 1}
	if err := mgr.RenewRemoteSnapshotInterest(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	mgr.snapshots.localInterestMu.Lock()
	mgr.snapshots.localInterests[first] = time.Now().Add(-time.Second).UnixNano()
	mgr.snapshots.localInterestMu.Unlock()
	if err := mgr.RenewRemoteSnapshotInterest(context.Background(), second); err != nil {
		t.Fatalf("expired interest did not release capacity: %v", err)
	}

	if err := mgr.ReleaseRemoteSnapshotInterest(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	const workers = 64
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- mgr.RenewRemoteSnapshotInterest(context.Background(), second)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent renewal failed: %v", err)
		}
	}
	mgr.snapshots.localInterestMu.Lock()
	count := len(mgr.snapshots.localInterests)
	mgr.snapshots.localInterestMu.Unlock()
	if count != 1 {
		t.Fatalf("local interest count = %d, want 1", count)
	}
}

// 容量按 consumer 计：每个 consumer 有自己的配额，Total 只是每节点的内存上限；两种拒绝都是可识别的错误（包裹 ErrRemoteOverloaded）。
func TestRemoteInterestRegistryHasHardCapacityLimits(t *testing.T) {
	const kind entity.EntityKind = 129
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, err := entity.BuildEntityID(1903, kind)
	if err != nil {
		t.Fatal(err)
	}
	registry := newRemoteInterestRegistry(remoteInterestLimits{PerConsumer: 1, Total: 3})
	key := entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
	other := key
	other.Scope++
	now := time.Now().UnixNano()
	registry.now = func() int64 { return now }
	expires := now + time.Hour.Nanoseconds()
	for _, sid := range []int32{1, 2, 3} {
		if err := registry.renew(entity.RemoteSnapshotInterest{ConsumerSID: sid, Key: key, ExpiresAt: expires, Generation: 1}); err != nil {
			t.Fatalf("consumer %d within its quota: %v", sid, err)
		}
	}
	if err := registry.renew(entity.RemoteSnapshotInterest{ConsumerSID: 1, Key: other, ExpiresAt: expires, Generation: 3}); !errors.Is(err, ErrInterestQuotaExceeded) || !errors.Is(err, entity.ErrRemoteOverloaded) {
		t.Fatalf("second lease of consumer 1 = %v, want ErrInterestQuotaExceeded (an ErrRemoteOverloaded)", err)
	}
	if err := registry.renew(entity.RemoteSnapshotInterest{ConsumerSID: 4, Key: other, ExpiresAt: expires}); !errors.Is(err, ErrInterestRegistryFull) || !errors.Is(err, entity.ErrRemoteOverloaded) {
		t.Fatalf("a fourth consumer over the node limit = %v, want ErrInterestRegistryFull", err)
	}
	registry.release(key, 1, 2)
	if got := registry.consumerLeases(1); got != 0 {
		t.Fatalf("released consumer quota=%d", got)
	}
	if err := registry.renew(entity.RemoteSnapshotInterest{ConsumerSID: 1, Key: other, ExpiresAt: expires, Generation: 3}); !errors.Is(err, ErrInterestRegistryFull) {
		t.Fatalf("release watermark must retain its slot: %v", err)
	}
	// 撤销水位到期后才能回收表槽；其他 consumer 的有效租约不能被驱逐。
	now += registry.fence.Nanoseconds() + 1
	if err := registry.renew(entity.RemoteSnapshotInterest{ConsumerSID: 1, Key: other, ExpiresAt: expires, Generation: 3}); err != nil {
		t.Fatalf("quota was not released: %v", err)
	}
	if got := registry.consumerLeases(1); got != 1 {
		t.Fatalf("consumer 1 holds %d leases, want 1", got)
	}
}

func TestRemoteSnapshotReplicaKeyIncludesFullScope(t *testing.T) {
	base := entity.RemoteSnapshotKey{Tenant: 7, EntityID: 99, Kind: 12, Scope: 3, Policy: 2}
	variants := []entity.RemoteSnapshotKey{base}
	for _, mutate := range []func(*entity.RemoteSnapshotKey){
		func(k *entity.RemoteSnapshotKey) { k.Tenant++ },
		func(k *entity.RemoteSnapshotKey) { k.EntityID++ },
		func(k *entity.RemoteSnapshotKey) { k.Kind++ },
		func(k *entity.RemoteSnapshotKey) { k.Scope++ },
		func(k *entity.RemoteSnapshotKey) { k.Policy++ },
	} {
		v := base
		mutate(&v)
		variants = append(variants, v)
	}
	seen := make(map[int64]struct{}, len(variants))
	for _, key := range variants {
		hash := remoteSnapshotReplicaKey(key)
		if _, exists := seen[hash]; exists {
			t.Fatalf("unexpected replica key collision for %+v", key)
		}
		seen[hash] = struct{}{}
	}
}

func interestKeyFor(t *testing.T, kind entity.EntityKind, unique int64) entity.RemoteSnapshotKey {
	t.Helper()
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, err := entity.BuildEntityID(unique, kind)
	if err != nil {
		t.Fatal(err)
	}
	return entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
}

// 迟到的旧 release 不能取消更新的 renewal：renew / release 各带一个可比较、不可复用的 Generation，接收端只释放代际不落后的
// 订阅（ExpiresAt 不能当版本用）；更旧的 renewal 也不能把租约往回改。
// 历史：RR-20260913-02。
func TestStaleInterestReleaseDoesNotCancelANewerRenewal(t *testing.T) {
	key := interestKeyFor(t, 244, 9341)
	registry := newRemoteInterestRegistry(remoteInterestLimits{})
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

	// Mirror 第 4 步更正：release(13) 留下撤销水位，在它之前发出的 renewal(12) 迟到后不能复活租约
	// （之前这里重新建起了租约，用例把它当作前提）。
	if err := registry.renew(second); err != nil {
		t.Fatal(err)
	}
	if registry.interested(key) {
		t.Fatal("a renewal issued before the release brought the withdrawn lease back")
	}
	// A renewal older than what is recorded is ignored rather than moving
	// the lease backwards.
	current := second
	current.Generation = 14
	if err := registry.renew(current); err != nil {
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
	if lease.generation != current.Generation || lease.expiresAt != current.ExpiresAt || lease.released {
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
	manager.snapshots.transport = publisher

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
	restarted.snapshots.transport = publisher
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

// 同一进程里相继创建的两个 Manager 落进同一个时钟刻度时，第二个的首个代际仍高于本进程已发出的任何代际（时钟冻住，
// 不依赖纳秒分辨率），owner 端不会按“落后代际忽略”丢掉新实例的 renewal。
// 历史：RR-20260913-02。
func TestInterestGenerationSeedAdvancesPastIssuedGenerationsUnderAFrozenClock(t *testing.T) {
	key := interestKeyFor(t, 250, 9343)
	previous := interestGenerationNow
	interestGenerationNow = func() uint64 { return 1_700_000_000_000_000_000 }
	t.Cleanup(func() { interestGenerationNow = previous })

	captured := make([]entity.RemoteSnapshotInterest, 0, 4)
	publisher := &capturingInterestPublisher{onPublish: func(i entity.RemoteSnapshotInterest, _ bool) {
		captured = append(captured, i)
	}}
	ctx := context.Background()
	first := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	first.snapshots.transport = publisher
	// renew → release → renew:三条消息、三个递增代际(同 key 的连续 renew 会被去重)。
	if err := first.RenewRemoteSnapshotInterest(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := first.ReleaseRemoteSnapshotInterest(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := first.RenewRemoteSnapshotInterest(ctx, key); err != nil {
		t.Fatal(err)
	}
	restarted := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	restarted.snapshots.transport = publisher
	if err := restarted.RenewRemoteSnapshotInterest(ctx, key); err != nil {
		t.Fatal(err)
	}
	if len(captured) != 4 {
		t.Fatalf("captured %d messages, want 4", len(captured))
	}
	for i := 1; i < len(captured); i++ {
		if captured[i].Generation <= captured[i-1].Generation {
			t.Fatalf("a restarted manager issued generation %d, not above the previous %d (same clock tick)", captured[i].Generation, captured[i-1].Generation)
		}
	}
}

// 表满时记下的溢出水位（interestOverflowFence）在最后一次 release + 兴趣 TTL 时失效（经 registry.now 注入可控时钟）：
//   - 到期前一刻，迟到的旧续租（代际不新于水位、key 在位图里）仍被挡住；
//   - 到期那一刻起水位清掉、不再挡：同一条旧续租照常建立租约；
//   - 到期时刻随同一 consumer 的后续 release 后移（最后一次 release + TTL）；
//   - 每个 consumer 至多一个水位：到期后由任何一次表满时的清理回收，不需要那个 consumer 再出现。

// overflowTestRegistry 建一个容量为 2、已被 filler 占满的兴趣表，时钟由返回的指针控制。
func overflowTestRegistry(t *testing.T, fence time.Duration) (*remoteInterestRegistry, *int64, entity.RemoteSnapshotKey) {
	t.Helper()
	registry := newRemoteInterestRegistry(remoteInterestLimits{PerConsumer: 4, Total: 2, ReleaseFence: fence})
	clock := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC).UnixNano()
	registry.now = func() int64 { return clock }
	const filler int32 = 1
	keyA := interestKeyFor(t, 245, 9701)
	keyB := interestKeyFor(t, 245, 9702)
	for _, key := range []entity.RemoteSnapshotKey{keyA, keyB} {
		if err := registry.renew(entity.RemoteSnapshotInterest{ConsumerSID: filler, Key: key, ExpiresAt: clock + time.Hour.Nanoseconds(), Generation: 1}); err != nil {
			t.Fatal(err)
		}
	}
	return registry, &clock, keyA
}

func TestInterestOverflowFenceExpiresOneTTLAfterTheLastRelease(t *testing.T) {
	const fence = 30 * time.Second
	registry, clock, fillerKey := overflowTestRegistry(t, fence)
	const consumer int32 = 7
	keyC := interestKeyFor(t, 245, 9703)
	keyD := interestKeyFor(t, 245, 9704)
	start := *clock

	// 表满时撤销 C（代际 21），10 秒后再撤销 D（代际 23）：同一个 consumer 的水位，到期时刻后移到第二次 + TTL。
	registry.release(keyC, consumer, 21)
	*clock = start + (10 * time.Second).Nanoseconds()
	registry.release(keyD, consumer, 23)
	if got := len(registry.overflow); got != 1 {
		t.Fatalf("overflow entries = %d after two releases by one consumer, want 1", got)
	}
	lastRelease := *clock
	registry.drop(fillerKey, 1, 1) // 表里空出一格：之后能否建立只由水位决定

	late := func() entity.RemoteSnapshotInterest {
		return entity.RemoteSnapshotInterest{ConsumerSID: consumer, Key: keyC, ExpiresAt: *clock + time.Minute.Nanoseconds(), Generation: 20}
	}

	// 第一次 release 的 TTL 已过、最后一次的还没过：仍然挡住。
	*clock = start + fence.Nanoseconds() + 1
	if err := registry.renew(late()); err != nil {
		t.Fatalf("late renewal: %v", err)
	}
	if registry.interested(keyC) {
		t.Fatal("the overflow fence expired one TTL after the first release; it must last until one TTL after the last")
	}
	// 到期前一刻：仍然挡住。
	*clock = lastRelease + fence.Nanoseconds() - 1
	if err := registry.renew(late()); err != nil {
		t.Fatalf("late renewal: %v", err)
	}
	if registry.interested(keyC) {
		t.Fatal("a renewal issued before the release revived the withdrawn lease before the overflow fence expired")
	}
	if got := len(registry.overflow); got != 1 {
		t.Fatalf("overflow entries = %d before expiry, want 1", got)
	}

	// 到期那一刻：水位清掉，同一条旧续租照常建立（此时任何撤销之前发出的真实续租都已过期，见 interestLease）。
	*clock = lastRelease + fence.Nanoseconds()
	if err := registry.renew(late()); err != nil {
		t.Fatalf("renewal after the fence expired: %v", err)
	}
	if !registry.interested(keyC) {
		t.Fatal("the overflow fence still refused a renewal after it expired")
	}
	if got := len(registry.overflow); got != 0 {
		t.Fatalf("overflow entries = %d after expiry, want 0", got)
	}
}

func TestInterestOverflowFencesAreOnePerConsumerAndReclaimedOnExpiry(t *testing.T) {
	const fence = 30 * time.Second
	registry, clock, _ := overflowTestRegistry(t, fence)
	start := *clock

	// 两个 consumer 在表满时各撤销大量 key：每个 consumer 只占一个溢出水位，代际取最大。
	for i := range 1000 {
		registry.release(interestKeyFor(t, 245, 10_000+int64(i)), 7, uint64(100+i))
	}
	for i := range 500 {
		registry.release(interestKeyFor(t, 245, 20_000+int64(i)), 8, uint64(50+i))
	}
	if got := len(registry.overflow); got != 2 {
		t.Fatalf("overflow entries = %d after 1500 releases by two consumers, want 2", got)
	}
	if got := registry.overflow[7].generation; got != 1099 {
		t.Fatalf("consumer 7 fence generation = %d, want the largest released (1099)", got)
	}
	if registry.total != 2 {
		t.Fatalf("registry entries = %d, want the two filler leases only (overflow fences take no table capacity)", registry.total)
	}

	// 两个水位都到期之后，另一个 consumer 在表满时的一次 release 触发清理：过期水位被回收，不需要
	// consumer 7 / 8 再出现；只剩新的那一个。
	*clock = start + fence.Nanoseconds()
	registry.release(interestKeyFor(t, 245, 30_000), 9, 1)
	if got := len(registry.overflow); got != 1 || registry.overflow[9] == nil {
		t.Fatalf("overflow after expiry = %d entries (consumer 9 present: %v), want only consumer 9's", got, registry.overflow[9] != nil)
	}
}

// 兴趣订阅的 payload 必须绑定信封 key / 版本 / 操作：不能用一个消费者的信封续租或撤销另一个 scope / SID 的 lease。
// 历史：RR-20261005-NC-34。
func TestInterestPayloadIdentityBeforeRegistryMutation(t *testing.T) {
	key := interestKeyFor(t, 244, 9350)
	base := entity.RemoteSnapshotInterest{Key: key, ConsumerSID: 7, ExpiresAt: time.Now().Add(time.Hour).UnixNano(), Generation: 11}
	for _, release := range []bool{false, true} {
		for _, field := range []string{"scope", "sid", "expires", "operation"} {
			name := "renew/" + field
			if release {
				name = "release/" + field
			}
			t.Run(name, func(t *testing.T) {
				m := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
				bus := &interestIdentityBus{}
				_, receiver := m.BindSync(bus)
				if err := receiver.Start(); err != nil {
					t.Fatal(err)
				}
				defer receiver.Stop()
				payload := base
				op := mirror.OpUpsert
				switch field {
				case "scope":
					payload.Key.Scope++
				case "sid":
					payload.ConsumerSID++
				case "expires":
					payload.ExpiresAt++
				case "operation":
					op = mirror.OpDelete
				}
				seed := payload
				seed.Generation = 10
				if err := m.snapshots.interests.renew(seed); err != nil {
					t.Fatal(err)
				}
				raw, err := mirror.MarshalPayload(remoteInterestWire{Interest: payload, Release: release})
				if err != nil {
					t.Fatal(err)
				}
				err = mirror.New(bus, SyncTopicInterest, nil).Publish(context.Background(), mirror.Envelope{Key: remoteInterestReplicaKey(base), Version: base.ExpiresAt, Op: op, Payload: raw})
				lease, exists := m.snapshots.interests.entries[seed.Key][seed.ConsumerSID]
				if !exists || lease.generation != seed.Generation || lease.expiresAt != seed.ExpiresAt || m.snapshots.interests.total != 1 {
					t.Errorf("refused message changed lease: %+v exists=%v total=%d", lease, exists, m.snapshots.interests.total)
				}
				if err == nil {
					t.Error("interest identity mismatch returned success")
				}
				// 拒绝之后仍能按发布端正式格式续租、撤销；不是关掉全部接收。
				if err := m.snapshots.transport.PublishRemoteInterest(context.Background(), payload, false); err != nil {
					t.Fatal(err)
				}
				if !m.snapshots.interests.interested(payload.Key) {
					t.Fatal("honest renewal missing")
				}
				if err := m.snapshots.transport.PublishRemoteInterest(context.Background(), payload, true); err != nil {
					t.Fatal(err)
				}
				if m.snapshots.interests.interested(payload.Key) {
					t.Fatal("honest release missing")
				}
			})
		}
	}
}

func TestInterestWireRejectsLegacyAndPreservesOrdering(t *testing.T) {
	m := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	bus := &interestIdentityBus{}
	_, rep := m.BindSync(bus)
	if err := rep.Start(); err != nil {
		t.Fatal(err)
	}
	defer rep.Stop()
	i := entity.RemoteSnapshotInterest{Key: interestKeyFor(t, 244, 9351), ConsumerSID: 7, ExpiresAt: time.Now().Add(time.Hour).UnixNano()}
	ctx := context.Background()
	// 无代际消息必须拒绝，不能修改兴趣表。
	if err := m.snapshots.transport.PublishRemoteInterest(ctx, i, false); err == nil {
		t.Fatal("publisher accepted generation zero renewal")
	}
	if m.snapshots.interests.interested(i.Key) {
		t.Fatal("legacy renewal accepted")
	}
	if err := m.snapshots.transport.PublishRemoteInterest(ctx, i, true); err == nil {
		t.Fatal("publisher accepted generation zero release")
	}
	if m.snapshots.interests.interested(i.Key) {
		t.Fatal("legacy message created lease")
	}
	// 绕过发布端校验，直接传旧线格式，证明接收端也拒绝。
	for _, release := range []bool{false, true} {
		raw, err := mirror.MarshalPayload(remoteInterestWire{Interest: i, Release: release})
		if err != nil {
			t.Fatal(err)
		}
		err = mirror.New(bus, SyncTopicInterest, nil).Publish(ctx, mirror.Envelope{
			Key: remoteInterestReplicaKey(i), Version: i.ExpiresAt, Op: mirror.OpUpsert, Payload: raw,
		})
		if err == nil || m.snapshots.interests.total != 0 {
			t.Fatalf("legacy wire accepted: err=%v total=%d", err, m.snapshots.interests.total)
		}
	}
	i.Generation = 12
	if err := m.snapshots.transport.PublishRemoteInterest(ctx, i, false); err != nil {
		t.Fatal(err)
	}
	stale := i
	stale.Generation = 11
	stale.ExpiresAt = time.Now().UnixNano()
	if err := m.snapshots.transport.PublishRemoteInterest(ctx, stale, true); err != nil {
		t.Fatal(err)
	}
	if !m.snapshots.interests.interested(i.Key) {
		t.Fatal("stale wire release canceled newer lease")
	}
	// 无 payload Delete 没有完整身份，明确拒绝且不影响现有租约。
	if err := mirror.New(bus, SyncTopicInterest, nil).PublishDelete(ctx, remoteInterestReplicaKey(i), i.ExpiresAt); err == nil {
		t.Fatal("receiver accepted identity-free delete")
	}
	if !m.snapshots.interests.interested(i.Key) {
		t.Fatal("identity-free delete changed registry")
	}
	i.Generation = 13
	i.ExpiresAt = time.Now().UnixNano()
	if err := m.snapshots.transport.PublishRemoteInterest(ctx, i, true); err != nil {
		t.Fatal(err)
	}
	if m.snapshots.interests.interested(i.Key) {
		t.Fatal("current generation release missing")
	}
}

// 同步传输夹具只递送消息；编解码和接收校验使用正式 Replicator。
type interestIdentityBus struct{ sub *fsyncbus.Subscription }

func (b *interestIdentityBus) Publish(msg *fsyncbus.SyncMsg) error {
	return b.sub.Deliver(context.Background(), msg)
}
func (b *interestIdentityBus) Subscribe(topic string, h fsyncbus.Handler) (*fsyncbus.Subscription, error) {
	b.sub = fsyncbus.NewSubscription(topic, h, nil)
	return b.sub, nil
}

// 兴趣续租的广播不拖住同条带其他 key 的读：条带锁只保护代际分配与本机表 / 兴趣表的更新，广播在锁外；同条带另一个 key
// 的 L1 命中不等别人的广播。续租失败的回滚（rollbackLocalInterest）不变。
// 历史：RR-20261006-68。
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

type deadlineInterestPublisher struct{ capturingInterestPublisher }

func (*deadlineInterestPublisher) PublishRemoteInterest(ctx context.Context, _ entity.RemoteSnapshotInterest, _ bool) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestInterestOutageDoesNotConsumeSnapshotReadBudget(t *testing.T) {
	step4RegisterDelta(t)
	key := staleBackfillKey(t, b2WatermarkKind, 97400)
	client, err := NewSnapshotClient(nil, SnapshotClientDeps{ConsumerSID: 2790, Loader: func(ctx context.Context, _ entity.RemoteSnapshotKey, _ entity.RemoteReadConsistency, _ uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
		if err := ctx.Err(); err != nil {
			return entity.RemoteSnapshotEnvelope{}, false, err
		}
		return step4Full(key, 7, "v7"), true, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	client.transport = &deadlineInterestPublisher{}
	defer client.Stop(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	got, found, err := client.ReadSnapshot(ctx, entity.RemoteSnapshotRead{Key: key, Consistency: entity.RemoteReadMonotonic})
	if err != nil || !found || got.StateVersion != 7 {
		t.Fatalf("interest outage blocked healthy authority: version=%d found=%v err=%v", got.StateVersion, found, err)
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("read consumed all its budget: %v", err)
	}
}

// 测试需要观察远端兴趣时显式等后台发布结束；ReadSnapshot 本身不承诺总线已确认。
func waitReadInterests(t *testing.T, client *SnapshotClient) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		client.interestPublishMu.Lock()
		running := client.interestPublishRunning
		client.interestPublishMu.Unlock()
		if !running {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("read interest publication did not drain")
}

type heldReadInterestPublisher struct {
	capturingInterestPublisher
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *heldReadInterestPublisher) PublishRemoteInterest(context.Context, entity.RemoteSnapshotInterest, bool) error {
	p.once.Do(func() { close(p.entered) })
	<-p.release
	return nil
}

func TestReadInterestQueueIsBoundedAndStopDrainsPublisher(t *testing.T) {
	client, err := NewSnapshotClient(nil, SnapshotClientDeps{ConsumerSID: 2791})
	if err != nil {
		t.Fatal(err)
	}
	publisher := &heldReadInterestPublisher{entered: make(chan struct{}), release: make(chan struct{})}
	client.transport = publisher
	stop, released := stopcontract.CallerReleases(client.Stop)
	stopcontract.Check(t, stopcontract.Hooks{
		Block: func(tb testing.TB) {
			key := staleBackfillKey(t, b2WatermarkKind, 97500)
			client.queueReadInterest(key)
			select {
			case <-publisher.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("publisher did not start")
			}
			for i := range interestPublishCapacity + 32 {
				client.queueReadInterest(staleBackfillKey(t, b2WatermarkKind, int64(97501+i)))
			}
			client.interestPublishMu.Lock()
			queued := len(client.interestPublishQueue)
			client.interestPublishMu.Unlock()
			if queued != interestPublishCapacity || client.Stats().LocalInterests != interestPublishCapacity+1 || client.Stats().InterestRejected != 32 {
				t.Fatalf("queue=%d stats=%+v", queued, client.Stats())
			}
		},
		Stop:     stop,
		Release:  func() { close(publisher.release) },
		Released: released,
	})
}

// 兴趣表满时 release 仍留撤销水位：consumer 以代际 g 撤销后，在撤销之前发出（代际 ≤ g）、撤销之后才到的续租不能把租约
// 带回来——release 落到不存在的条目且表已满时也一样。
// 历史：RR-20261006-11。
func TestInterestReleaseOnAFullRegistryStillFencesTheLateRenewal(t *testing.T) {
	registry := newRemoteInterestRegistry(remoteInterestLimits{PerConsumer: 4, Total: 2})
	keyA := interestKeyFor(t, 244, 9601)
	keyB := interestKeyFor(t, 244, 9602)
	keyC := interestKeyFor(t, 244, 9603)
	keyD := interestKeyFor(t, 244, 9604)
	expires := time.Now().Add(time.Minute).UnixNano()
	const filler, consumer int32 = 1, 7

	// 表满：两个 filler 租约。
	for _, key := range []entity.RemoteSnapshotKey{keyA, keyB} {
		if err := registry.renew(entity.RemoteSnapshotInterest{ConsumerSID: filler, Key: key, ExpiresAt: expires, Generation: 1}); err != nil {
			t.Fatal(err)
		}
	}
	// consumer 先续租 C（代际 20），再撤销 C（代际 21）；线上 release 先到，C 的条目还不存在。
	registry.release(keyC, consumer, 21)
	// 表里空出一格（A 的租约被清掉，与到期清理同样不留水位）。
	registry.drop(keyA, filler, 1)

	late := entity.RemoteSnapshotInterest{ConsumerSID: consumer, Key: keyC, ExpiresAt: expires, Generation: 20}
	if err := registry.renew(late); err != nil {
		t.Fatalf("late renewal: %v", err)
	}
	if registry.interested(keyC) {
		t.Fatal("a renewal issued before the release brought the withdrawn lease back after the full registry dropped the watermark")
	}
	if got := registry.consumerLeases(consumer); got != 0 {
		t.Fatalf("consumer holds %d leases after withdrawing its only interest, want 0", got)
	}

	// 撤销之后发出的续租（代际更新）照常建立租约，不被这次撤销挡住。
	fresh := entity.RemoteSnapshotInterest{ConsumerSID: consumer, Key: keyD, ExpiresAt: expires, Generation: 22}
	if err := registry.renew(fresh); err != nil {
		t.Fatalf("a renewal issued after the release: %v", err)
	}
	if !registry.interested(keyD) {
		t.Fatal("a renewal issued after the release was refused")
	}

	// 退路只挡被撤销的 key（指纹位图）：同一窗口里另一个 key 的迟到旧续租，指纹不碰撞时照常建立。
	cWord, cMask := interestOverflowBit(keyC, consumer)
	var keyE entity.RemoteSnapshotKey
	for unique := int64(9605); ; unique++ {
		keyE = interestKeyFor(t, 244, unique)
		if word, mask := interestOverflowBit(keyE, consumer); word != cWord || mask != cMask {
			break
		}
	}
	registry.drop(keyD, consumer, 22) // 腾出一格
	other := entity.RemoteSnapshotInterest{ConsumerSID: consumer, Key: keyE, ExpiresAt: expires, Generation: 19}
	if err := registry.renew(other); err != nil {
		t.Fatalf("late renewal of another key: %v", err)
	}
	if !registry.interested(keyE) {
		t.Fatal("the overflow fence refused a renewal of a key that was never released")
	}
}

// renewIfNeeded must reject an interest whose consumer, key, or expiry is
// unusable instead of registering it — a bad interest would otherwise keep a
// snapshot fanning out to nobody.
func TestRemoteInterestRegistryRejectsUnusableInterest(t *testing.T) {
	const kind entity.EntityKind = 134
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, err := entity.BuildEntityID(1904, kind)
	if err != nil {
		t.Fatal(err)
	}
	key := entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
	future := time.Now().Add(time.Second).UnixNano()
	cases := map[string]entity.RemoteSnapshotInterest{
		"consumer zero":     {ConsumerSID: 0, Key: key, ExpiresAt: future},
		"key kind mismatch": {ConsumerSID: 7, Key: entity.RemoteSnapshotKey{EntityID: id, Kind: kind + 1, Scope: 1}, ExpiresAt: future},
		"key raw id":        {ConsumerSID: 7, Key: entity.RemoteSnapshotKey{EntityID: 1904, Kind: kind, Scope: 1}, ExpiresAt: future},
		"already expired":   {ConsumerSID: 7, Key: key, ExpiresAt: time.Now().Add(-time.Millisecond).UnixNano()},
	}
	for name, interest := range cases {
		t.Run(name, func(t *testing.T) {
			registry := newRemoteInterestRegistry(remoteInterestLimits{})
			renew, err := registry.renewIfNeeded(interest, 0)
			if !errors.Is(err, entity.ErrRemoteRejected) || renew {
				t.Fatalf("renewIfNeeded = %v, %v; want ErrRemoteRejected", renew, err)
			}
			if registry.interested(interest.Key) || registry.total != 0 {
				t.Fatal("a rejected interest was registered")
			}
		})
	}
	var nilRegistry *remoteInterestRegistry
	if _, err := nilRegistry.renewIfNeeded(entity.RemoteSnapshotInterest{ConsumerSID: 7, Key: key, ExpiresAt: future}, 0); !errors.Is(err, entity.ErrRemoteRejected) {
		t.Fatalf("nil registry err = %v", err)
	}
}
