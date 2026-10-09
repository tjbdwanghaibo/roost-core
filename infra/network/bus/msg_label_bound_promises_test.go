package bus

// RR-20261006-72：异步消息的指标标签与死信列表键不能取对端任意的 (module, msg)。
//
// 修前 dispatchMsg 找不到 handler 时进死信并计 bus_dead_letter_total{module,msg}，派发被拒计
// bus_dispatch_drop_total{module,msg,reason}，死信列表键也按这对名字建——名字全部来自对端的 envelope，
// 版本混跑或错发时每个新名字多一组序列、多一个 Redis 列表，只受注册表每指标 2048 的兜底约束。RR-20261006-19
// 只给 RPC 的 method 加了上界。承诺（同 RR-19 的被调方规则）：标签与死信桶只取本进程注册过的名字，其余
// 归并为 unregisteredMsgLabel；死信条目本身保留原来的 module / msg，重投仍发回原名字。

import (
	"fmt"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/base/worker"
)

func labelValues(name, label string) map[string]bool {
	out := map[string]bool{}
	for _, metric := range metrics.Snapshot() {
		if metric.Name == name {
			out[metric.Labels[label]] = true
		}
	}
	return out
}

func TestAsyncMessageLabelsAndDeadLetterKeysAreBoundedByRegistration(t *testing.T) {
	store := newReliableMemoryStore()
	b := New(nil, nil, JSONCodec{}, Config{Sid: 1, SvcType: "game", Prefix: "roost"})
	b.EnableReliable(store, ReliableConfig{Enabled: true})
	if err := b.Handle("mail71", "Changed", func(*MsgContext) { panic("handler failed") }); err != nil {
		t.Fatal(err)
	}
	const strays = 300
	registeredBefore := busMetricValue("bus_dead_letter_total", map[string]string{"module": "mail71", "msg": "Changed"})
	for i := range strays {
		b.dispatchMsg(&incomingTask{natsMsg: &fnats.NatsMsg{ToSid: 1, ToModule: fmt.Sprintf("stray71-%d", i), MsgName: fmt.Sprintf("Msg%d", i), MsgID: fmt.Sprintf("s71-%d", i)}})
	}
	// 注册过的名字照常有自己的标签与死信桶。
	b.dispatchMsg(&incomingTask{natsMsg: &fnats.NatsMsg{ToSid: 1, ToModule: "mail71", MsgName: "Changed", MsgID: "m71-1"}})

	for module := range labelValues("bus_dead_letter_total", "module") {
		if len(module) > 7 && module[:7] == "stray71" {
			t.Fatalf("bus_dead_letter_total carries a peer-chosen module label %q", module)
		}
	}
	if got := busMetricValue("bus_dead_letter_total", map[string]string{"module": unregisteredMsgLabel, "msg": unregisteredMsgLabel}); got < strays {
		t.Fatalf("bus_dead_letter_total{_unregistered} = %d, want >= %d", got, strays)
	}
	if got := busMetricValue("bus_dead_letter_total", map[string]string{"module": "mail71", "msg": "Changed"}); got != registeredBefore+1 {
		t.Fatalf("registered message lost its own label: %d", got)
	}

	store.mu.Lock()
	buckets := len(store.dead)
	stray := store.dead[DeadLetterKey(unregisteredMsgLabel, unregisteredMsgLabel)]
	store.mu.Unlock()
	if buckets != 2 {
		t.Fatalf("dead letters spread over %d buckets, want 2 (_unregistered + mail71:Changed)", buckets)
	}
	if len(stray) != strays || stray[0].ToModule != "stray71-0" || stray[0].MsgName != "Msg0" {
		t.Fatalf("unregistered bucket = %d entries, first %+v; entries must keep the original names", len(stray), stray[0])
	}

	// 派发被拒（池已停）同理。
	pool := worker.NewPool[*incomingTask](worker.PoolConfig{Name: "bus71", WorkerNum: 1, QueueCap: 1}, b.handleTask)
	pool.Start()
	pool.Stop()
	b.lifeMu.Lock()
	b.pool = pool
	b.lifeMu.Unlock()
	for i := range strays {
		b.dispatchTask(int64(i), &incomingTask{natsMsg: &fnats.NatsMsg{ToSid: 1, ToModule: fmt.Sprintf("drop71-%d", i), MsgName: "X", MsgID: fmt.Sprintf("d71-%d", i)}})
	}
	b.lifeMu.Lock()
	b.pool = nil
	b.lifeMu.Unlock()
	for module := range labelValues("bus_dispatch_drop_total", "module") {
		if len(module) > 6 && module[:6] == "drop71" {
			t.Fatalf("bus_dispatch_drop_total carries a peer-chosen module label %q", module)
		}
	}
}
