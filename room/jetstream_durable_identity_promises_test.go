package room

import (
	"context"
	"testing"

	fsyncbus "github.com/tjbdwanghaibo/roost-core/syncbus"
)

// U-0210 · C8 · RR-20260916-02:持久消费者的身份必须覆盖它过滤的完整 subject。旧实现只用
// topic 与 SID 做散列,Prefix 不在身份里:两个总线配置不同 Prefix(`one` / `two`)却共用
// Stream 与 LocalSid、订阅同一 topic,得到同一个 durable 名对应不同 FilterSubject,服务端无法
// 表示为两个独立消费者。承诺:非默认 Prefix 进入身份;默认 Prefix 的名字保持不变——已部署的
// durable 游标不能因升级而被抛弃。
func TestDurableSyncNamePromiseDistinguishesPrefixes(t *testing.T) {
	one := newFakeJetStream()
	two := newFakeJetStream()
	busOne, err := NewJetStreamSyncBus(context.Background(), one, JetStreamSyncConfig{LocalSid: 7, Prefix: "one", Stream: "SHARED"})
	if err != nil {
		t.Fatal(err)
	}
	defer busOne.Stop()
	busTwo, err := NewJetStreamSyncBus(context.Background(), two, JetStreamSyncConfig{LocalSid: 7, Prefix: "two", Stream: "SHARED"})
	if err != nil {
		t.Fatal(err)
	}
	defer busTwo.Stop()
	noop := func(*fsyncbus.SyncMsg) error { return nil }
	if _, err := busOne.Subscribe("state", noop); err != nil {
		t.Fatal(err)
	}
	if _, err := busTwo.Subscribe("state", noop); err != nil {
		t.Fatal(err)
	}
	a, b := one.consumers[0], two.consumers[0]
	if a.Stream == b.Stream && a.Durable == b.Durable && a.FilterSubject != b.FilterSubject {
		t.Fatalf("same (stream, durable) %q/%q for different subjects %q vs %q", a.Stream, a.Durable, a.FilterSubject, b.FilterSubject)
	}
}

func TestDurableSyncNamePromiseKeepsTheDefaultPrefixNameStable(t *testing.T) {
	// 审查记录的、升级前的名字:默认 Prefix 下必须逐字不变,否则已部署消费者的 ACK 游标被抛弃。
	const legacy = "sync_state_7_aeabf91c58d07db1"
	cfg := normalizeJetStreamSyncConfig(JetStreamSyncConfig{LocalSid: 7})
	if got := durableSyncName(cfg.Prefix, "state", 7); got != legacy {
		t.Fatalf("default-prefix durable name changed: %q want %q", got, legacy)
	}
	if got := durableSyncName("one", "state", 7); got == legacy {
		t.Fatal("a non-default prefix must not collide with the default identity")
	}
	if durableSyncName("one", "state", 7) != durableSyncName("one", "state", 7) {
		t.Fatal("durable name must be stable across calls")
	}
}
