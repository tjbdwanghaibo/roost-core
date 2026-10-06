package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/internal/stopcontract"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/sync/syncbus"
)

// Mirror 方案第 1、3 步（docs/feature/MIRROR-STEPS-1-3-2026-10-06.md）：共享 SnapshotClient。
//
// 承诺：
//   - 客户端只要权威 loader（可选）、共享 L2（可选）和总线，不要求写 backend；它不实现任何写 / 提交 /
//     发布 / 所有权接口；
//   - owner（Manager）与只读客户端同身份、同进程、同一条总线时互不冲突：只读方经兴趣让 owner 发布，经复制
//     消息收到快照，不注册 kind 或解码器；
//   - Linearizable 只在 loader 声明线性化能力时开放；
//   - 启动失败逐步回收、重试不重复订阅；停止套用 A3 三步停机骨架（internal/stopcontract）。
//
// kind 253 与 B2 用例共用（同一注册定义），EntityID 用 97xx 段。

// loopbackBus 把 Publish 同步投递给同一主题的全部订阅者（同一进程里的 owner 与只读方共用一条总线）。
type loopbackBus struct {
	mu       sync.Mutex
	next     int
	handlers map[string]map[int]*fsyncbus.Subscription
	// failSubscribe 非空时，对这个主题的下一次订阅失败一次。
	failSubscribe string
	subscribes    map[string]int
}

func newLoopbackBus() *loopbackBus {
	return &loopbackBus{handlers: make(map[string]map[int]*fsyncbus.Subscription), subscribes: make(map[string]int)}
}

func (b *loopbackBus) Publish(msg *fsyncbus.SyncMsg) error {
	b.mu.Lock()
	subs := make([]*fsyncbus.Subscription, 0, len(b.handlers[msg.Topic]))
	for _, sub := range b.handlers[msg.Topic] {
		subs = append(subs, sub)
	}
	b.mu.Unlock()
	for _, sub := range subs {
		if err := sub.Deliver(context.Background(), msg); err != nil && !errors.Is(err, fsyncbus.ErrUnsubscribed) {
			return err
		}
	}
	return nil
}

func (b *loopbackBus) Subscribe(topic string, h fsyncbus.Handler) (*fsyncbus.Subscription, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subscribes[topic]++
	if b.failSubscribe == topic {
		b.failSubscribe = ""
		return nil, fmt.Errorf("subscribe %s: broker unavailable", topic)
	}
	if b.handlers[topic] == nil {
		b.handlers[topic] = make(map[int]*fsyncbus.Subscription)
	}
	b.next++
	id := b.next
	sub := fsyncbus.NewSubscription(topic, h, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.handlers[topic], id)
	})
	b.handlers[topic][id] = sub
	return sub, nil
}

// SubscribeLive：同步进程内投递，订阅返回即确认（fsyncbus.ILiveSubscriber，快照推送开着）。
func (b *loopbackBus) SubscribeLive(topic string, h fsyncbus.Handler) (*fsyncbus.Subscription, error) {
	return b.Subscribe(topic, h)
}

func (b *loopbackBus) active(topic string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.handlers[topic])
}

func clientCommit(key entity.RemoteSnapshotKey, version uint64, payload string) entity.RemoteCommit {
	data := []byte(payload)
	return entity.RemoteCommit{EntityID: key.EntityID, NextVersion: version, Snapshots: []entity.RemoteSnapshotRecord{{
		Key: key, StateVersion: version, BaseVersion: version - 1, MarkerEpoch: 1, RouteEpoch: 1,
		Schema: 1, Codec: 1, Full: true, Data: data, Checksum: entity.RemoteSnapshotChecksum(data),
	}}}
}

// 第 1 / 3 步：只读客户端没有写能力。类型上不实现任何写 / 提交 / 发布 / 所有权接口；构造只要 loader 与
// 消费者身份。负对照：给 SnapshotClient 加一个导出的 PublishRemoteSnapshot 方法，本用例即红。
func TestSnapshotClientHasNoWriteCapability(t *testing.T) {
	client, err := NewSnapshotClient(nil, SnapshotClientDeps{ConsumerSID: 2201,
		Loader: func(context.Context, entity.RemoteSnapshotKey, entity.RemoteReadConsistency, uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
			return entity.RemoteSnapshotEnvelope{}, false, nil
		}})
	if err != nil {
		t.Fatalf("a read-only client needs no write backend: %v", err)
	}
	var value any = client
	for _, capability := range []reflect.Type{
		reflect.TypeFor[entity.RemoteWriteBatchManager](),
		reflect.TypeFor[entity.RemoteOwnershipManager](),
		reflect.TypeFor[entity.RemoteCommitApplier](),
		reflect.TypeFor[entity.IRemoteSnapshotPublisher](),
		reflect.TypeFor[entity.IRemoteEntityManager](),
	} {
		if reflect.TypeOf(value).Implements(capability) {
			t.Errorf("SnapshotClient implements %v: a read-only client must not carry write capability", capability)
		}
	}
	if _, ok := value.(entity.RemoteSnapshotReadOnly); !ok {
		t.Fatal("SnapshotClient must provide entity.RemoteSnapshotReadOnly")
	}
	for _, bad := range []SnapshotClientDeps{
		{}, // 没有消费者身份
		{ConsumerSID: 1, LinearizableLoader: true}, // 声明线性化却没有 loader
	} {
		if _, err := NewSnapshotClient(nil, bad); err == nil {
			t.Errorf("NewSnapshotClient(%+v) accepted an illegal combination", bad)
		}
	}
}

// 第 1 / 3 步：owner 与只读方同身份、同进程、同一条总线。只读方读 → 续租兴趣 → owner 的兴趣表收到 →
// owner 提交后发布 → 只读方的复制接收按 key 落进自己的缓存 → 只读方经 RemoteMirrorReader 读到 DTO。
// 整个过程不注册 kind 或解码器，也不需要写 backend。
func TestSnapshotClientReadsWhatTheOwnerPublishesInTheSameProcess(t *testing.T) {
	ctx := context.Background()
	bus := newLoopbackBus()
	key := staleBackfillKey(t, b2WatermarkKind, 9701)
	owner := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 2301)
	ownerSnapshots, ownerInterests := owner.BindSync(bus)
	for _, rep := range []interface{ Start() error }{ownerSnapshots, ownerInterests} {
		if err := rep.Start(); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(ownerSnapshots.Stop)
	t.Cleanup(ownerInterests.Stop)

	consumer, err := NewSnapshotClient(nil, SnapshotClientDeps{ConsumerSID: 2302})
	if err != nil {
		t.Fatal(err)
	}
	if err := consumer.Start(bus); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = consumer.Stop(context.Background()) })

	reader, err := entity.NewRemoteMirrorReader(consumer, entity.RemoteMirrorSpec{Kind: key.Kind, Scope: key.Scope, Schema: 1, Codec: 1},
		func(data []byte) (string, error) { return string(data), nil })
	if err != nil {
		t.Fatal(err)
	}
	// 第一次读：缓存与 L2 都没有，Cached 不回源；读本身续租了兴趣。
	if _, found, err := reader.Read(ctx, key.EntityID, entity.RemoteReadCached, entity.RemoteObservation{}); err != nil || found {
		t.Fatalf("first read before any publish: found=%v err=%v", found, err)
	}
	if !owner.snapshots.interests.interested(key) {
		t.Fatal("the consumer's read did not reach the owner's interest table")
	}
	if err := owner.snapshots.publishCommitted(ctx, clientCommit(key, 1, "guild-summary-v1")); err != nil {
		t.Fatal(err)
	}
	got, found, err := reader.Read(ctx, key.EntityID, entity.RemoteReadCached, entity.RemoteObservation{})
	if err != nil || !found || got.Value != "guild-summary-v1" || got.Observation.StateVersion != 1 {
		t.Fatalf("consumer read value=%q token=%+v found=%v err=%v; want the owner's v1", got.Value, got.Observation, found, err)
	}
	// 同进程的另一个只读方直接用 owner 组合的客户端，不另建订阅。
	sameProcess, err := entity.NewRemoteMirrorReader(owner.SnapshotClient(), entity.RemoteMirrorSpec{Kind: key.Kind, Scope: key.Scope, Schema: 1, Codec: 1},
		func(data []byte) (string, error) { return string(data), nil })
	if err != nil {
		t.Fatal(err)
	}
	if got, found, err := sameProcess.Read(ctx, key.EntityID, entity.RemoteReadCached, got.Observation); err != nil || !found || got.Value != "guild-summary-v1" {
		t.Fatalf("owner-side reader value=%q found=%v err=%v", got.Value, found, err)
	}
	if bus.active(SyncTopicSnapshot) != 2 || bus.active(SyncTopicInterest) != 2 {
		t.Fatalf("subscriptions snapshot=%d interest=%d; want one per client (owner + consumer)", bus.active(SyncTopicSnapshot), bus.active(SyncTopicInterest))
	}
}

// 第 2 步：Linearizable 只在 loader 声明线性化能力时开放，不静默退化。Manager 的 backend 是写 owner 的
// 权威存储，沿用它一直提供的 Linearizable 读。
func TestSnapshotClientLinearizableNeedsADeclaredLoader(t *testing.T) {
	ctx := context.Background()
	key := staleBackfillKey(t, b2WatermarkKind, 9702)
	loads := 0
	loader := func(_ context.Context, k entity.RemoteSnapshotKey, _ entity.RemoteReadConsistency, _ uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
		loads++
		return staleBackfillEnvelope(k, 2, 1, "v2"), true, nil
	}
	plain, err := NewSnapshotClient(nil, SnapshotClientDeps{ConsumerSID: 2303, Loader: loader})
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := plain.ReadSnapshot(ctx, entity.RemoteSnapshotRead{Key: key, Consistency: entity.RemoteReadLinearizable}); !errors.Is(err, entity.ErrRemoteReadUnsupported) || found || loads != 0 {
		t.Fatalf("Linearizable without a declared loader: found=%v err=%v loads=%d; want ErrRemoteReadUnsupported without loading", found, err, loads)
	}
	declared, err := NewSnapshotClient(nil, SnapshotClientDeps{ConsumerSID: 2304, Loader: loader, LinearizableLoader: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, found, err := declared.ReadSnapshot(ctx, entity.RemoteSnapshotRead{Key: key, Consistency: entity.RemoteReadLinearizable}); err != nil || !found || got.StateVersion != 2 {
		t.Fatalf("Linearizable with a declared loader: version=%d found=%v err=%v", got.StateVersion, found, err)
	}
	owner := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 2305)
	owner.SetBackend(&countingSnapshotBackend{answer: func(k entity.RemoteSnapshotKey) (entity.RemoteSnapshotEnvelope, bool, error) {
		return staleBackfillEnvelope(k, 3, 1, "v3"), true, nil
	}})
	if got, found, err := owner.ReadRemoteSnapshot(ctx, key, entity.RemoteReadLinearizable, 0); err != nil || !found || got.StateVersion != 3 {
		t.Fatalf("Manager Linearizable read: version=%d found=%v err=%v", got.StateVersion, found, err)
	}
}

// 第 3 步：启动失败逐步回收，重试不重复订阅。第二个订阅失败时第一个已退掉；重试后每个主题恰好一个订阅；
// 重复 Start 幂等。负对照：去掉 Start 里失败分支的 snapshotRep.Stop()，快照主题留下 1 个订阅、重试后 2 个。
func TestSnapshotClientStartFailureLeavesNoSubscription(t *testing.T) {
	bus := newLoopbackBus()
	bus.failSubscribe = SyncTopicInterest
	client, err := NewSnapshotClient(nil, SnapshotClientDeps{ConsumerSID: 2306})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(bus); err == nil {
		t.Fatal("Start succeeded although the interest subscription failed")
	}
	if n := bus.active(SyncTopicSnapshot); n != 0 {
		t.Fatalf("a failed Start left %d snapshot subscription(s)", n)
	}
	for range 2 {
		if err := client.Start(bus); err != nil {
			t.Fatalf("retry: %v", err)
		}
	}
	if s, i := bus.active(SyncTopicSnapshot), bus.active(SyncTopicInterest); s != 1 || i != 1 {
		t.Fatalf("after a retry and a repeated Start: snapshot=%d interest=%d subscriptions, want 1 and 1", s, i)
	}
	if err := client.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s, i := bus.active(SyncTopicSnapshot), bus.active(SyncTopicInterest); s != 0 || i != 0 {
		t.Fatalf("after Stop: snapshot=%d interest=%d subscriptions", s, i)
	}
	if err := client.Start(bus); !errors.Is(err, ErrSnapshotClientStopped) {
		t.Fatalf("Start after Stop = %v, want ErrSnapshotClientStopped (one lifetime; build a new client)", err)
	}
	key := staleBackfillKey(t, b2WatermarkKind, 9703)
	if _, _, err := client.ReadSnapshot(context.Background(), entity.RemoteSnapshotRead{Key: key}); !errors.Is(err, ErrSnapshotClientStopped) {
		t.Fatalf("read after Stop = %v, want ErrSnapshotClientStopped", err)
	}
}

// 第 3 步：停止套用 A3 三步停机骨架。卡住的工作是一次不响应取消的权威加载（Monotonic 未命中回源）；
// 客户端不持有 Redis / 权威 / 总线，它们由调用方在 Stop 返回 nil 之后释放（CallerReleases）。
// 负对照：去掉 Stop 末尾的 c.work.Wait(ctx)（只等复制 handler），第 1、2 步变红。
func TestSnapshotClientStopContract(t *testing.T) {
	key := staleBackfillKey(t, b2WatermarkKind, 9704)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	read := make(chan error, 1)
	var client *SnapshotClient
	var stop func(context.Context) error
	var released func() bool
	stopcontract.Check(t, stopcontract.Hooks{
		Start: func(t testing.TB) {
			var err error
			client, err = NewSnapshotClient(nil, SnapshotClientDeps{ConsumerSID: 2307,
				Loader: func(context.Context, entity.RemoteSnapshotKey, entity.RemoteReadConsistency, uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
					entered <- struct{}{}
					<-release // 不响应取消的 loader
					return entity.RemoteSnapshotEnvelope{}, false, nil
				}})
			if err != nil {
				t.Fatal(err)
			}
			if err := client.Start(newLoopbackBus()); err != nil {
				t.Fatal(err)
			}
			stop, released = stopcontract.CallerReleases(client.Stop)
		},
		Block: func(t testing.TB) {
			go func() {
				_, _, err := client.ReadSnapshot(context.Background(), entity.RemoteSnapshotRead{Key: key, Consistency: entity.RemoteReadMonotonic})
				read <- err
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("the read never reached the loader")
			}
		},
		Stop:     func(ctx context.Context) error { return stop(ctx) },
		Release:  func() { close(release) },
		Released: func() bool { return released() },
	})
	select {
	case <-read:
	case <-time.After(5 * time.Second):
		t.Fatal("the admitted read never returned")
	}
}

// 第 3 步：Stop 取消在途的权威加载。响应取消的 loader 在 Stop 发起后立刻返回，Stop 在预算内返回 nil。
func TestSnapshotClientStopCancelsLoads(t *testing.T) {
	key := staleBackfillKey(t, b2WatermarkKind, 9705)
	entered := make(chan struct{}, 1)
	client, err := NewSnapshotClient(nil, SnapshotClientDeps{ConsumerSID: 2308,
		Loader: func(ctx context.Context, _ entity.RemoteSnapshotKey, _ entity.RemoteReadConsistency, _ uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
			entered <- struct{}{}
			<-ctx.Done()
			return entity.RemoteSnapshotEnvelope{}, false, ctx.Err()
		}})
	if err != nil {
		t.Fatal(err)
	}
	read := make(chan error, 1)
	go func() {
		_, _, err := client.ReadSnapshot(context.Background(), entity.RemoteSnapshotRead{Key: key, Consistency: entity.RemoteReadMonotonic})
		read <- err
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Stop(ctx); err != nil {
		t.Fatalf("Stop with a cancellable load in flight = %v; the load should have been cancelled", err)
	}
	if err := <-read; !errors.Is(err, context.Canceled) {
		t.Fatalf("the cancelled read returned %v, want context.Canceled", err)
	}
}
