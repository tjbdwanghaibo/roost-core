package syncbus_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus"
)

// 本文件是 SyncBus 有限投递的兜底模板（docs/framework/guide/04-sync.md §5“丢失兜底”）。
//
// SyncBus 默认当作可丢广播：JetStream 有 MaxDeliver、保留期限与容量，普通 NATS 最多一次。
// 需要最终一致的订阅方必须自己闭环：
//
//  1. 丢失检测：每个 key 的 Version 由权威方逐次 +1；订阅方收到 Version > 本地+1 即序号缺口。
//  2. 版本落后 / 投递耗尽：缺口只在“后面还有消息”时才看得见，末尾丢失必须靠周期对账
//     （拉权威版本摘要，与本地比较）和投递终止告警（nats.jetstream.terminal.total{reason="max_deliver"}）触发。
//  3. 对账：发现缺口或落后时不再应用增量，向权威拉取该 key 的快照整体替换，之后继续按版本接收。
//
// 快照来源由业务决定：权威服务的 RPC、权威存储（Mongo / Remote 快照读）等；本例用函数代替。

// counterDelta 是一条增量消息：只携带变化量，丢一条就会算错，所以必须有缺口检测。
type counterDelta struct {
	Add int64 `json:"add"`
}

// counterSnapshot 是权威方给出的整体状态，Version 与增量消息共用同一序列。
type counterSnapshot struct {
	Version int64
	Value   int64
}

// authority 是权威方：每次修改把该 key 的版本 +1，并发布增量。
type authority struct {
	bus      syncbus.ISyncBus
	ids      *syncbus.DeliveryIDs
	sid      int32
	topic    string
	counters map[int64]counterSnapshot
}

func (a *authority) add(ctx context.Context, key, delta int64) error {
	state := a.counters[key]
	state.Version++
	state.Value += delta
	a.counters[key] = state
	data, err := json.Marshal(counterDelta{Add: delta})
	if err != nil {
		return err
	}
	msg := &syncbus.SyncMsg{MessageID: a.ids.Next(), Topic: a.topic, Key: key, Version: state.Version, Data: data, FromSid: a.sid}
	if publisher, ok := a.bus.(syncbus.IContextPublisher); ok {
		return publisher.PublishContext(ctx, msg)
	}
	return a.bus.Publish(msg)
}

// snapshot 与 versions 是对账接口：真实部署里是一次 RPC 或一次权威存储读。
func (a *authority) snapshot(_ context.Context, key int64) (counterSnapshot, error) {
	return a.counters[key], nil
}

func (a *authority) versions(_ context.Context) (map[int64]int64, error) {
	out := make(map[int64]int64, len(a.counters))
	for key, state := range a.counters {
		out[key] = state.Version
	}
	return out, nil
}

// replica 是需要最终一致的订阅方。
//
// 契约：applied[key] 是已经整体正确的版本；只有 Version == applied+1 的增量可以直接应用。
// 旧版本与重复投递直接丢弃（JetStream 可能重投，同一身份重订也可能从旧游标续投一段）。
// 一旦发现缺口就不再接受该 key 的增量，直到快照替换完成。
type replica struct {
	pullSnapshot func(context.Context, int64) (counterSnapshot, error)
	pullVersions func(context.Context) (map[int64]int64, error)
	values       map[int64]int64
	applied      map[int64]int64
	log          []string
}

func (r *replica) handle(ctx context.Context, msg *syncbus.SyncMsg) error {
	current := r.applied[msg.Key]
	switch {
	case msg.Version <= current:
		r.log = append(r.log, fmt.Sprintf("key=%d v%d: stale or duplicate, dropped", msg.Key, msg.Version))
		return nil
	case msg.Version > current+1:
		r.log = append(r.log, fmt.Sprintf("key=%d v%d: gap after v%d, pull snapshot", msg.Key, msg.Version, current))
		return r.resync(ctx, msg.Key)
	}
	var delta counterDelta
	if err := json.Unmarshal(msg.Data, &delta); err != nil {
		// 坏载荷同样意味着这一版丢了：按缺口处理，不跳过版本。
		return r.resync(ctx, msg.Key)
	}
	r.values[msg.Key] += delta.Add
	r.applied[msg.Key] = msg.Version
	return nil
}

// resync 用权威快照整体替换一个 key。快照版本可能已经超过触发它的那条消息，这正是想要的：
// 之后到达的、版本不大于快照的增量会被当作重复丢弃。
func (r *replica) resync(ctx context.Context, key int64) error {
	snapshot, err := r.pullSnapshot(ctx, key)
	if err != nil {
		// 拉取失败不能假装已一致：保持旧版本，下一次缺口或周期对账会再试。
		return fmt.Errorf("resync key %d: %w", key, err)
	}
	if snapshot.Version < r.applied[key] {
		return fmt.Errorf("resync key %d: authority v%d is behind local v%d", key, snapshot.Version, r.applied[key])
	}
	r.values[key] = snapshot.Value
	r.applied[key] = snapshot.Version
	r.log = append(r.log, fmt.Sprintf("key=%d resynced to v%d value=%d", key, snapshot.Version, snapshot.Value))
	return nil
}

// audit 是周期对账：比较权威版本摘要，补齐末尾丢失（后面没有消息，缺口检测永远看不到）。
// 触发时机：定时器、启动 / 重订之后、收到投递终止告警之后。
func (r *replica) audit(ctx context.Context) error {
	authoritative, err := r.pullVersions(ctx)
	if err != nil {
		return err
	}
	keys := make([]int64, 0, len(authoritative))
	for key := range authoritative {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, key := range keys {
		if authoritative[key] > r.applied[key] {
			r.log = append(r.log, fmt.Sprintf("key=%d audit: local v%d behind authority v%d", key, r.applied[key], authoritative[key]))
			if err := r.resync(ctx, key); err != nil {
				return err
			}
		}
	}
	return nil
}

// lossyBus 是进程内的 ISyncBus，按发布序号丢弃指定消息，模拟 MaxDeliver 耗尽或保留期过期。
type lossyBus struct {
	subs      map[string][]*syncbus.Subscription
	published int
	drop      []int
}

func (b *lossyBus) Publish(msg *syncbus.SyncMsg) error {
	b.published++
	if slices.Contains(b.drop, b.published) {
		return nil
	}
	for _, sub := range b.subs[msg.Topic] {
		if err := sub.Deliver(context.Background(), msg); err != nil && err != syncbus.ErrUnsubscribed {
			return err
		}
	}
	return nil
}

func (b *lossyBus) Subscribe(topic string, handler syncbus.Handler) (*syncbus.Subscription, error) {
	var sub *syncbus.Subscription
	sub = syncbus.NewSubscription(topic, handler, func() {
		b.subs[topic] = slices.DeleteFunc(b.subs[topic], func(s *syncbus.Subscription) bool { return s == sub })
	})
	b.subs[topic] = append(b.subs[topic], sub)
	return sub, nil
}

// Example_reconcileAfterLoss 演示有限投递下的兜底：中间丢失由缺口检测触发快照，
// 末尾丢失由周期对账补齐，重复投递按版本丢弃。
func Example_reconcileAfterLoss() {
	ctx := context.Background()
	// 第 2 条（key=7 v2）与第 5 条（key=8 v2，之后再无 key=8 的消息）丢失。
	bus := &lossyBus{subs: map[string][]*syncbus.Subscription{}, drop: []int{2, 5}}
	source := &authority{bus: bus, ids: syncbus.NewDeliveryIDs("counter"), sid: 1, topic: "counter", counters: map[int64]counterSnapshot{}}
	view := &replica{pullSnapshot: source.snapshot, pullVersions: source.versions, values: map[int64]int64{}, applied: map[int64]int64{}}
	sub, err := bus.Subscribe("counter", view.handle)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = sub.Unsubscribe(ctx) }()

	for _, step := range []struct{ key, delta int64 }{{7, 10}, {7, 5}, {7, 1}, {8, 3}, {8, 4}} {
		if err := source.add(ctx, step.key, step.delta); err != nil {
			fmt.Println(err)
			return
		}
	}
	// 重投一条已经应用过的旧消息：按版本丢弃，不会加两次。
	_ = sub.Deliver(ctx, &syncbus.SyncMsg{Topic: "counter", Key: 7, Version: 1, Data: []byte(`{"add":10}`)})
	fmt.Printf("before audit: key7=%d key8=%d\n", view.values[7], view.values[8])
	if err := view.audit(ctx); err != nil {
		fmt.Println(err)
		return
	}
	for _, line := range view.log {
		fmt.Println(line)
	}
	fmt.Printf("after audit: key7=%d key8=%d\n", view.values[7], view.values[8])
	// Output:
	// before audit: key7=16 key8=3
	// key=7 v3: gap after v1, pull snapshot
	// key=7 resynced to v3 value=16
	// key=7 v1: stale or duplicate, dropped
	// key=8 audit: local v1 behind authority v2
	// key=8 resynced to v2 value=7
	// after audit: key7=16 key8=7
}
