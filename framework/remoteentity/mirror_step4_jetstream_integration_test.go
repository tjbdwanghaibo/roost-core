//go:build integration

package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sync"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
	gojs "github.com/nats-io/nats.go/jetstream"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	natsdriver "github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus"
	syncdriver "github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus/driver"
)

// Mirror 第 4 步在真实 JetStream（+ 真实 Redis）上的验收（docs/feature/MIRROR-STEP-4-AND-O4-2026-10-06.md）。
//
// 隔离：流名 / 前缀与 L2 部署前缀都带 m4.<pid>.<ns>，内存存储，用例结束删流（durable 随流删除）并删 L2 键。
// 以 ROOST_DATAENGINE_IT=1 与 ROOST_DATAENGINE_IT_NATS_URL / _REDIS_ADDR 准入。单独运行：
//
//	go test -tags integration -run '^TestRealJetStreamLive' ./framework/remoteentity

type step4JetStream struct {
	t      *testing.T
	ctx    context.Context
	url    string
	prefix string
}

func newStep4JetStream(t *testing.T, ctx context.Context) *step4JetStream {
	t.Helper()
	if os.Getenv("ROOST_DATAENGINE_IT") != "1" {
		t.Skip("set ROOST_DATAENGINE_IT=1 with the isolated environment")
	}
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Skip("ROOST_DATAENGINE_IT_NATS_URL is not set")
	}
	env := &step4JetStream{t: t, ctx: ctx, url: url, prefix: fmt.Sprintf("m4.p%d%d.sync", os.Getpid(), time.Now().UnixNano()%1_000_000_000)}
	t.Cleanup(func() {
		nc, err := gonats.Connect(url, gonats.Timeout(2*time.Second))
		if err != nil {
			t.Logf("cleanup connect: %v", err)
			return
		}
		defer nc.Close()
		js, err := gojs.New(nc)
		if err != nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		t.Logf("delete stream %s: %v", syncdriver.JetStreamSyncStream(env.prefix), js.DeleteStream(cleanupCtx, syncdriver.JetStreamSyncStream(env.prefix)))
	})
	return env
}

func (e *step4JetStream) bus(sid int32) fsyncbus.ISyncBus {
	e.t.Helper()
	client, err := natsdriver.NewClient(fnats.DefaultConfig(e.url), natsdriver.ClientOptions{})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(client.Close)
	js, err := natsdriver.NewJetStreamClient(client)
	if err != nil {
		e.t.Fatal(err)
	}
	bus, err := syncdriver.NewJetStreamSyncBus(e.ctx, js, syncdriver.JetStreamSyncConfig{
		LocalSid: sid, Prefix: e.prefix, Storage: fnats.JetStreamStorageMemory, StreamMaxAge: 10 * time.Minute,
		AckWait: 2 * time.Second,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = bus.StopWithContext(stopCtx)
	})
	return bus
}

// durable 名在服务端的实际形状（发版文档 REM-4 疑点闭环，2026-10-06）：durableSyncName 经 sanitizeSyncName
// 把主题里的 "." 换成 "_"，所以快照主题的 DeliverNew durable 是 sync_remote_entity_snapshot_live_<sid>_<16 位
// 十六进制>，旧的 DeliverAll durable 是 sync_remote_entity_snapshot_<sid>_<16 位十六进制>（MIRROR-STEP-4 与
// USER_GUIDE 之前写成 sync_remote_entity_snapshot.live_<sid>_…）。这里从服务端列出消费者名核对。
func TestRealJetStreamLiveDurableNameShape(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	env := newStep4JetStream(t, ctx)
	bus := env.bus(2503)
	live, ok := bus.(fsyncbus.ILiveSubscriber)
	if !ok {
		t.Fatal("the JetStream sync bus must provide confirmed subscriptions")
	}
	nop := func(context.Context, *fsyncbus.SyncMsg) error { return nil }
	unsubAll, err := bus.Subscribe(SyncTopicSnapshot, nop)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unsubAll.Unsubscribe(context.Background()) }()
	unsubLive, err := live.SubscribeLive(SyncTopicSnapshot, nop)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unsubLive.Unsubscribe(context.Background()) }()

	nc, err := gonats.Connect(env.url, gonats.Timeout(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := gojs.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(ctx, syncdriver.JetStreamSyncStream(env.prefix))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	lister := stream.ConsumerNames(ctx)
	for name := range lister.Name() {
		names = append(names, name)
	}
	if err := lister.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(names)
	t.Logf("server-side durable names: %v", names)
	liveName := regexp.MustCompile(`^sync_remote_entity_snapshot_live_2503_[0-9a-f]{16}$`)
	allName := regexp.MustCompile(`^sync_remote_entity_snapshot_2503_[0-9a-f]{16}$`)
	var sawLive, sawAll bool
	for _, name := range names {
		sawLive = sawLive || liveName.MatchString(name)
		sawAll = sawAll || allName.MatchString(name)
	}
	if !sawLive || !sawAll || len(names) != 2 {
		t.Fatalf("durables %v; want one sync_remote_entity_snapshot_live_2503_<hex16> (DeliverNew) and one sync_remote_entity_snapshot_2503_<hex16> (DeliverAll)", names)
	}
}

// 可确认订阅：确认之前入流的历史不重放（DeliverNew）；确认之后发布的每条都投递；退订期间发布的，同一身份
// 重新订阅后从游标续投；退订时被停掉的拉取请求已交出、未确认的，在 AckWait 之后重投（重连恢复，
// 不被静默丢掉）。
func TestRealJetStreamLiveSubscriptionConfirmsAndResumes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	env := newStep4JetStream(t, ctx)
	publisher, subscriber := env.bus(2501), env.bus(2502)
	live, ok := subscriber.(fsyncbus.ILiveSubscriber)
	if !ok {
		t.Fatal("the JetStream sync bus must provide confirmed subscriptions")
	}
	const topic = "m4_live"
	publish := func(version int64) {
		t.Helper()
		if err := publisher.Publish(&fsyncbus.SyncMsg{MessageID: fmt.Sprintf("m4-%d", version), Topic: topic, Key: 1, Version: version, Data: []byte("x")}); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var got []int64
	handler := func(_ context.Context, msg *fsyncbus.SyncMsg) error {
		mu.Lock()
		got = append(got, msg.Version)
		mu.Unlock()
		return nil
	}
	waitFor := func(version int64) []int64 {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			mu.Lock()
			seen := slices.Clone(got)
			mu.Unlock()
			if slices.Contains(seen, version) {
				return seen
			}
			if time.Now().After(deadline) {
				t.Fatalf("version %d never delivered; got %v", version, seen)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	publish(1) // 订阅确认之前的历史
	unsub, err := live.SubscribeLive(topic, handler)
	if err != nil {
		t.Fatal(err)
	}
	publish(2)
	if seen := waitFor(2); !slices.Equal(seen, []int64{2}) {
		t.Fatalf("delivered %v; want only the message published after the subscription was confirmed (no history replay)", seen)
	}
	if err := unsub.Unsubscribe(context.Background()); err != nil {
		t.Fatal(err)
	}
	publish(3) // 退订期间
	// RR-25：退订关闭本地准入，传输 Closed 可能稍晚；不得把 Busy 当订阅成功。
	deadline := time.Now().Add(5 * time.Second)
	for {
		unsub, err = live.SubscribeLive(topic, handler)
		if err == nil {
			break
		}
		if !errors.Is(err, fsyncbus.ErrSubscriptionBusy) || time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	defer func() { _ = unsub.Unsubscribe(context.Background()) }()
	publish(4)
	// 退订时停掉的拉取请求可能已把 3 交出（未确认）：它在 AckWait（这里 2s）之后重投，不会丢。
	waitFor(4)
	seen := waitFor(3)
	if slices.Contains(seen, 1) {
		t.Fatalf("after resubscribing delivered %v; want the message published while unsubscribed (3) resumed from the durable cursor and never the history (1)", seen)
	}
}

// SnapshotClient 在真实 JetStream 上开推送：只读方读（续租兴趣）→ owner 收到兴趣 → owner 提交后发布 →
// 只读方经确认订阅收到、按 key 准入（先过真实 Redis L2 的版本 CAS）。只读方启动之前入流的历史快照不被重放。
func TestRealJetStreamLiveSnapshotPushReachesTheReader(t *testing.T) {
	r := realRemoteRedis(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	env := newStep4JetStream(t, ctx)
	l2Prefix := "m4:" + env.prefix
	key := staleBackfillKey(t, b2WatermarkMatrixKind, 9850)
	redisKey := l2Prefix + ":" + remoteSnapshotL2Key(key)
	b27DeleteKeys(t, r, redisKey)

	cfg := DefaultConfig()
	newL2 := func() *remoteSnapshotL2Store {
		l2, err := NewSnapshotL2StoreWithKeyPrefix(r, cfg.SnapshotL2TTL, l2Prefix)
		if err != nil {
			t.Fatal(err)
		}
		return l2
	}
	owner := NewManager(newMockVersionedLockFactory(), cfg, 2511, newL2())
	if err := owner.snapshots.Start(env.bus(2511)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.snapshots.Stop(context.Background()) })
	if !owner.snapshots.Stats().PushEnabled {
		t.Fatal("the owner's client did not enable push on JetStream")
	}
	// 只读方启动前的一次发布（有兴趣，进流）：它不能被重放到只读方。
	if err := owner.snapshots.interests.renew(entity.RemoteSnapshotInterest{ConsumerSID: 2599, Key: key, ExpiresAt: time.Now().Add(time.Minute).UnixNano(), Generation: 1}); err != nil {
		t.Fatal(err)
	}
	if err := owner.snapshots.publishCommitted(ctx, clientCommit(key, 1, "v1")); err != nil {
		t.Fatal(err)
	}

	reader, err := NewSnapshotClient(cfg, SnapshotClientDeps{ConsumerSID: 2512, L2: newL2()})
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Start(env.bus(2512)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Stop(context.Background()) })
	if !reader.Stats().PushEnabled {
		t.Fatal("the reader did not enable push on JetStream")
	}
	// 读：L2 有 v1（经 L2 确认），同时续租兴趣。
	if got, found, err := reader.ReadSnapshot(ctx, entity.RemoteSnapshotRead{Key: key, Consistency: entity.RemoteReadCached}); err != nil || !found || got.StateVersion != 1 {
		t.Fatalf("first read: version=%d found=%v err=%v", got.StateVersion, found, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !owner.snapshots.interests.interested(key) || owner.snapshots.interests.consumerLeases(2512) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the reader's interest never reached the owner")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := owner.snapshots.publishCommitted(ctx, clientCommit(key, 2, "v2")); err != nil {
		t.Fatal(err)
	}
	// v1 在陈旧上限（30s）内仍是确认过的，Cached 读不回源：这里读到 v2 只能来自推送。
	deadline = time.Now().Add(10 * time.Second)
	for {
		got, found, err := reader.ReadSnapshot(ctx, entity.RemoteSnapshotRead{Key: key, Consistency: entity.RemoteReadCached})
		if err != nil {
			t.Fatal(err)
		}
		if found && got.StateVersion == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the push of v2 never reached the reader (Cached read version=%d found=%v)", got.StateVersion, found)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if stats := reader.Stats().Bootstrap; stats.Overflows != 0 {
		t.Fatalf("bootstrap stats %+v", stats)
	}
}
