package servicerpc

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/bus"
	fetcd "github.com/tjbdwanghaibo/roost-core/etcd"
)

// plainBus is an IBus without the reliable (JetStream) calls.
type plainBus struct{ bus.IBus }

// U-0147 · C2 · nightly gap map core `servicerpc` 5/7：两个选择器对空实例表报错；
// JetStream 传输下总线不支持可靠调用时拒绝（定向与非定向两条路）；发现结果全无效时
// PickServer 报错而不是选 0 号。
func TestPickersAndReliableTransportRefuseWhatIsMissing(t *testing.T) {
	ctx := context.Background()
	// 带 Key 的亲和选择器：没有这条守卫，空实例表会在取模处除零。
	affinity := KeyAffinityPicker{Key: func(context.Context) (string, bool) { return "player-7", true }}
	if _, err := affinity.Pick(ctx, "game", nil, 0); err == nil || !strings.Contains(err.Error(), "no game service discovered") {
		t.Fatalf("KeyAffinityPicker with no instances = %v", err)
	}
	if _, err := (RoundRobinPicker{}).Pick(ctx, "game", nil, 0); err == nil || !strings.Contains(err.Error(), "no game service discovered") {
		t.Fatalf("RoundRobinPicker with no instances = %v", err)
	}
	client := NewBusClient(plainBus{}, "game", time.Second, WithTransport(TransportJetStream))
	var resp struct{}
	if err := client.Call(ctx, 0, "Ping", struct{}{}, &resp); err == nil || !strings.Contains(err.Error(), "reliable transport unavailable") {
		t.Fatalf("Call over JetStream on a plain bus = %v", err)
	}
	if err := client.Call(ctx, 3, "Ping", struct{}{}, &resp); err == nil || !strings.Contains(err.Error(), "reliable transport unavailable") {
		t.Fatalf("directed Call over JetStream on a plain bus = %v", err)
	}
	client.discovery = &fakeDiscovery{infos: []*fetcd.ServiceInfo{nil, {Sid: 0}}}
	if _, err := client.PickServer(ctx); err == nil || !strings.Contains(err.Error(), "no game service discovered") {
		t.Fatalf("PickServer with only unusable instances = %v", err)
	}
	client.discovery = &fakeDiscovery{infos: []*fetcd.ServiceInfo{{Sid: 9}}}
	if sid, err := client.PickServer(ctx); err != nil || sid != 9 {
		t.Fatalf("PickServer with one instance = (%d, %v)", sid, err)
	}
}
