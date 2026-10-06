//go:build integration

package remoteentity

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
	gojs "github.com/nats-io/nats.go/jetstream"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	syncdriver "github.com/tjbdwanghaibo/roost-core/sync/syncbus/driver"
	"github.com/tjbdwanghaibo/roost-core/sync/syncbus/mirror"
)

// 兴趣消息的 handler 出错时 JetStream 怎样结算（RR-20261006-11 复核；发版文档 REM-5 原写“未核对”）。
//
// 设计：兴趣主题走普通 Subscribe（DeliverAll durable），同步总线对本地 handler 的错误是“记日志、继续”
// （jetStreamSyncBus.invoke），consume 回调返回 nil，驱动 Ack——不 NAK 重投、也不 Term。兴趣被拒
// （配额 / 表满 / 已过期 / 身份不符）由同样的消息历史决定，重投同一条只会再被拒；恢复靠 consumer 自己：
// 它本机先按同一张表判定（被拒不广播），被 owner 单方拒绝的租约在剩余不足一半时（或 owner 启动时的
// 续租请求，O-M6-1）以更新的代际重新续租。拒绝期间这个 key 没有推送，读取按 cached_max_staleness 回源。
//
// 这里在真实 JetStream 上断言：两条出错的兴趣消息（表满被拒、身份不符）各只投递一次；服务端 ack floor
// 推进到末尾、没有待确认、没有重投，等过 AckWait 也不再投递，Term 计数不变；之后表里有空位时，consumer 以
// 更新代际的续租照常建立租约。
//
//	go test -tags integration -run '^TestRealJetStreamInterestHandlerErrorIsAcknowledged$' ./remoteentity
func TestRealJetStreamInterestHandlerErrorIsAcknowledged(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	env := newStep4JetStream(t, ctx)
	const ownerSID, consumerSID, fillerSID int32 = 2531, 2532, 2598

	owner := &SnapshotClient{interests: newRemoteInterestRegistry(remoteInterestLimits{PerConsumer: 4, Total: 1})}
	fillerKey := interestKeyFor(t, 245, 9801)
	key := interestKeyFor(t, 245, 9802)
	if err := owner.interests.renew(entity.RemoteSnapshotInterest{ConsumerSID: fillerSID, Key: fillerKey, ExpiresAt: time.Now().Add(time.Hour).UnixNano(), Generation: 1}); err != nil {
		t.Fatal(err)
	}
	store := &recordingInterestStore{inner: InterestReplicaStore{client: owner}}
	ownerRep := mirror.New(env.bus(ownerSID), SyncTopicInterest, store)
	if err := ownerRep.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ownerRep.StopWithContext(context.Background()) })

	publisher := &remoteSyncer{interestRep: mirror.New(env.bus(consumerSID), SyncTopicInterest, InterestReplicaStore{})}
	terminalBefore := metricTotal("nats.jetstream.terminal.total")

	// 1. 表满：owner 拒绝这条续租（ErrInterestRegistryFull）。
	if err := publisher.PublishRemoteInterest(ctx, entity.RemoteSnapshotInterest{ConsumerSID: consumerSID, Key: key, ExpiresAt: time.Now().Add(time.Minute).UnixNano(), Generation: 10}, false); err != nil {
		t.Fatal(err)
	}
	// 2. 信封身份与载荷不符：owner 拒绝（永久错误）。
	raw, err := json.Marshal(remoteInterestWire{Interest: entity.RemoteSnapshotInterest{ConsumerSID: consumerSID, Key: key, ExpiresAt: time.Now().Add(time.Minute).UnixNano(), Generation: 11}})
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.interestRep.Publish(ctx, mirror.Envelope{Key: 12345, Version: 1, Op: mirror.OpUpsert, Payload: raw}); err != nil {
		t.Fatal(err)
	}
	store.waitCalls(t, 2)
	if errs := store.errors(); len(errs) != 2 || !errors.Is(errs[0], ErrInterestRegistryFull) {
		t.Fatalf("handler errors %v; want the registry-full refusal and the identity mismatch", errs)
	}

	consumer := interestDurable(t, ctx, env)
	deadline := time.Now().Add(10 * time.Second)
	var info *gojs.ConsumerInfo
	for {
		info, err = consumer.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if info.NumAckPending == 0 && info.AckFloor.Stream == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never settled the failed interest deliveries: ack_floor=%d pending=%d redelivered=%d delivered=%d",
				info.AckFloor.Stream, info.NumAckPending, info.NumRedelivered, info.Delivered.Consumer)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// AckWait 是 2s（newStep4JetStream）：NAK 会立刻重投，漏 ack 会在 AckWait 后重投。等过它再看。
	time.Sleep(3 * time.Second)
	if info, err = consumer.Info(ctx); err != nil {
		t.Fatal(err)
	}
	if calls := store.callCount(); calls != 2 || info.NumRedelivered != 0 || info.Delivered.Consumer != 2 || info.NumAckPending != 0 {
		t.Fatalf("after AckWait: handler calls=%d, server delivered=%d redelivered=%d pending=%d; want each failed interest delivered once and acknowledged",
			calls, info.Delivered.Consumer, info.NumRedelivered, info.NumAckPending)
	}
	if after := metricTotal("nats.jetstream.terminal.total"); after != terminalBefore {
		t.Fatalf("nats.jetstream.terminal.total went %d -> %d; the failed interest deliveries must be acknowledged, not terminated", terminalBefore, after)
	}

	// 恢复路径：表里有了空位，consumer 下一次续租（代际更新）照常建立租约。
	owner.interests.drop(fillerKey, fillerSID, 1)
	if err := publisher.PublishRemoteInterest(ctx, entity.RemoteSnapshotInterest{ConsumerSID: consumerSID, Key: key, ExpiresAt: time.Now().Add(time.Minute).UnixNano(), Generation: 12}, false); err != nil {
		t.Fatal(err)
	}
	store.waitCalls(t, 3)
	if !owner.interests.interested(key) || owner.interests.consumerLeases(consumerSID) != 1 {
		t.Fatal("the consumer's next renewal did not establish the lease the full registry had refused")
	}
}

// recordingInterestStore 记下兴趣 handler 的每次调用与返回的错误，原样把错误交回复制器。
type recordingInterestStore struct {
	inner InterestReplicaStore
	mu    sync.Mutex
	calls int
	errs  []error
}

func (s *recordingInterestStore) ApplyReplica(ctx context.Context, env mirror.Envelope) error {
	err := s.inner.ApplyReplica(ctx, env)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if err != nil {
		s.errs = append(s.errs, err)
	}
	return err
}

func (s *recordingInterestStore) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *recordingInterestStore) errors() []error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]error(nil), s.errs...)
}

func (s *recordingInterestStore) waitCalls(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for s.callCount() < want {
		if time.Now().After(deadline) {
			t.Fatalf("interest handler called %d times, want %d", s.callCount(), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// interestDurable 找到兴趣主题在这个隔离流上唯一的 durable 消费者。
func interestDurable(t *testing.T, ctx context.Context, env *step4JetStream) gojs.Consumer {
	t.Helper()
	nc, err := gonats.Connect(env.url, gonats.Timeout(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
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
	if len(names) != 1 {
		t.Fatalf("durables %v; want the one interest consumer", names)
	}
	consumer, err := stream.Consumer(ctx, names[0])
	if err != nil {
		t.Fatal(err)
	}
	return consumer
}

func metricTotal(name string) int64 {
	var total int64
	for _, m := range metrics.Snapshot() {
		if m.Name == name {
			total += m.Value
		}
	}
	return total
}
