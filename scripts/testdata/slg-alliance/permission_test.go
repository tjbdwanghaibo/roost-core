package alliance

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

// 真实 NATS 只发送失效提示，成员列表仍从持久权威拉取。两个独立连接代表发布端与视野端，
// 不代表跨机部署；本测试串行消费，生产接线应由视野协调循环负责 hold/read/apply/ready。
// 普通 Pub/Sub 会丢通知，必须周期性权威重读；不能据通知传输承诺即时撤权。
func verifyPermissionHints(t *testing.T, ctx context.Context, prefix string, a *alliance, f *fog) {
	t.Helper()
	_, publisher := newBus(t, prefix, "permission-authority", 2000)
	_, consumer := newBus(t, prefix, "permission-view", 1001)
	subject := prefix + ".permission"
	hints := make(chan []byte, 8)
	sub, e := consumer.Subscribe(subject, func(m *fnats.Msg) {
		select {
		case hints <- append([]byte(nil), m.Data...):
		default:
		} // 有界提示队列；满时靠权威重读恢复。
	})
	check(t, e)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	check(t, consumer.FlushContext(ctx))
	latest, e := a.read(ctx)
	check(t, e)
	load := func() (state, error) { return a.read(ctx) }
	// 首次通知高于本地已知代际：即使下一次无提示重试，也不能降低这个读取下限。
	stale := clone(latest)
	stale.PermissionGeneration--
	staleMember := stale.Members["bob"]
	staleMember.Active = true
	stale.Members["bob"] = staleMember
	staleLoad := func() (state, error) { return stale, nil }
	if e := f.reconcileAtLeast(latest.PermissionGeneration, staleLoad); e == nil {
		t.Fatal("lagging first read accepted")
	}
	if e := f.reconcileFrom(staleLoad); e == nil {
		t.Fatal("retry forgot notification generation floor")
	}
	// 新代通知、重复通知、延迟旧代通知都读当前权威，不把旧通知解释成恢复旧授权。
	for _, generation := range []uint64{latest.PermissionGeneration, latest.PermissionGeneration, latest.PermissionGeneration - 1} {
		raw, e := json.Marshal(generation)
		check(t, e)
		check(t, publisher.Publish(subject, raw))
		check(t, publisher.FlushContext(ctx))
		select {
		case raw := <-hints:
			var minimum uint64
			check(t, json.Unmarshal(raw, &minimum))
			check(t, f.reconcileAtLeast(minimum, load))
		case <-time.After(3 * time.Second):
			t.Fatal("permission notification did not arrive")
		}
		if f.members["bob"].Active || f.generation != latest.PermissionGeneration {
			t.Fatal("old notification restored revoked member")
		}
	}
	// 读源落后于通知下限时必须 held，而不是发布旧权限；正常重读也覆盖漏通知后的恢复入口。
	if e := f.reconcileAtLeast(latest.PermissionGeneration, func() (state, error) { old := clone(latest); old.PermissionGeneration--; return old, nil }); e == nil {
		t.Fatal("authority behind notification was accepted")
	}
	check(t, f.reconcileFrom(load))
	t.Log("NATS permission hints: duplicate/out-of-order hints pull current authority; lagging authority holds publication")
}
