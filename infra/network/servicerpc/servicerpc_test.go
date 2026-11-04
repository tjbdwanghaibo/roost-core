package servicerpc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	fetcd "github.com/tjbdwanghaibo/roost-core/infra/network/etcd"
)

type statusResponse struct {
	Code   int32
	Reason string
}

func (r statusResponse) StatusCode() int32    { return r.Code }
func (r statusResponse) StatusReason() string { return r.Reason }

type recordingBus struct {
	bus.IBus
	calls     []string
	lastSid   int32
	reliable  bool
	returnErr error
}

func (b *recordingBus) Call(_ context.Context, svcType, method string, _ any, _ any) error {
	b.calls = append(b.calls, "call:"+svcType+":"+method)
	return b.returnErr
}

func (b *recordingBus) CallTo(_ context.Context, svcType string, sid int32, method string, _ any, _ any) error {
	b.calls = append(b.calls, "callTo:"+svcType+":"+method)
	b.lastSid = sid
	return b.returnErr
}

func (b *recordingBus) CallReliable(_ context.Context, svcType, method string, _ any, _ any) error {
	b.reliable = true
	b.calls = append(b.calls, "reliable:"+svcType+":"+method)
	return b.returnErr
}

func (b *recordingBus) CallToReliable(_ context.Context, svcType string, sid int32, method string, _ any, _ any) error {
	b.reliable = true
	b.lastSid = sid
	b.calls = append(b.calls, "reliableTo:"+svcType+":"+method)
	return b.returnErr
}

type fakeDiscovery struct {
	fetcd.IDiscovery
	infos []*fetcd.ServiceInfo
	err   error
}

func (d *fakeDiscovery) Discover(context.Context, string) ([]*fetcd.ServiceInfo, error) {
	return d.infos, d.err
}

// A business failure carried in the response envelope must become a Go error
// the caller can act on, and must keep the peer's reason rather than being
// flattened into the fallback.
func TestCheckResponseSurfacesTheEnvelopeStatus(t *testing.T) {
	if err := CheckResponse(statusResponse{Code: CodeOK}, "fallback"); err != nil {
		t.Fatalf("an OK envelope returned %v", err)
	}
	err := CheckResponse(statusResponse{Code: 42, Reason: "board id is empty"}, "fallback")
	if err == nil {
		t.Fatal("a failing envelope returned no error")
	}
	if !strings.Contains(err.Error(), "board id is empty") {
		t.Fatalf("the peer's reason was dropped: %v", err)
	}
	// A response that carries no status at all must be reported, not assumed
	// successful: silently treating "no status" as OK is how a decode failure
	// looks like a successful call.
	if err := CheckResponse(struct{}{}, "fallback"); !errors.Is(err, ErrResponseStatusMissing) {
		t.Fatalf("a status-less response returned %v, want ErrResponseStatusMissing", err)
	}
}

func TestBusClientRoutesToDiscoveredInstanceAndHonoursTransport(t *testing.T) {
	b := &recordingBus{}
	discovery := &fakeDiscovery{infos: []*fetcd.ServiceInfo{{Sid: 7}}}
	client := NewDiscoveredBusClient(b, "rank", time.Second, discovery)

	var resp statusResponse
	if err := client.CallDiscoveredChecked(context.Background(), "rank.GetTop", struct{}{}, &resp, "rank"); err != nil {
		t.Fatal(err)
	}
	if b.lastSid != 7 {
		t.Fatalf("call went to sid %d, want the discovered 7", b.lastSid)
	}
	if b.reliable {
		t.Fatal("the lightweight transport used the reliable path")
	}

	reliableBus := &recordingBus{}
	reliableClient := NewDiscoveredBusClient(reliableBus, "rank", time.Second, discovery, WithTransport(TransportJetStream))
	if err := reliableClient.CallDiscoveredChecked(context.Background(), "rank.GetTop", struct{}{}, &resp, "rank"); err != nil {
		t.Fatal(err)
	}
	if !reliableBus.reliable {
		t.Fatal("TransportJetStream did not take the reliable path")
	}
}

func TestPickServerReportsAnEmptyDiscoverySet(t *testing.T) {
	client := NewDiscoveredBusClient(&recordingBus{}, "rank", time.Second, &fakeDiscovery{})
	if _, err := client.PickServer(context.Background()); err == nil {
		t.Fatal("an empty discovery set was accepted")
	}
	failing := NewDiscoveredBusClient(&recordingBus{}, "rank", time.Second, &fakeDiscovery{err: errors.New("etcd down")})
	if _, err := failing.PickServer(context.Background()); err == nil {
		t.Fatal("a discovery failure was swallowed")
	}
	// Instances advertising sid 0 are not addressable and must be filtered,
	// not picked and then called with sid 0 — which silently broadcasts.
	zeroed := NewDiscoveredBusClient(&recordingBus{}, "rank", time.Second,
		&fakeDiscovery{infos: []*fetcd.ServiceInfo{{Sid: 0}, nil}})
	if _, err := zeroed.PickServer(context.Background()); err == nil {
		t.Fatal("an unaddressable instance was picked")
	}
}

// Round robin is the right default for a stateless read and the wrong one for
// a mutation on shared per-key state: it sends consecutive operations on one
// key to different replicas, so an in-process lock guards nothing.
func TestRoundRobinSpreadsWhileKeyAffinityPins(t *testing.T) {
	infos := []*fetcd.ServiceInfo{{Sid: 1}, {Sid: 2}, {Sid: 3}}
	ctx := context.Background()

	spread := map[int32]int{}
	for sequence := uint64(0); sequence < 6; sequence++ {
		sid, err := (RoundRobinPicker{}).Pick(ctx, "match", infos, sequence)
		if err != nil {
			t.Fatal(err)
		}
		spread[sid]++
	}
	if len(spread) != 3 {
		t.Fatalf("round robin reached %d of 3 instances: %v", len(spread), spread)
	}

	picker := KeyAffinityPicker{Key: AffinityKeyFromContext}
	keyed := WithAffinityKey(ctx, "queue:ranked:2")
	first, err := picker.Pick(keyed, "match", infos, 0)
	if err != nil {
		t.Fatal(err)
	}
	for sequence := uint64(1); sequence < 20; sequence++ {
		sid, err := picker.Pick(keyed, "match", infos, sequence)
		if err != nil {
			t.Fatal(err)
		}
		if sid != first {
			t.Fatalf("the same key routed to sid %d then %d; affinity must pin it", first, sid)
		}
	}
	// Discovery order must not change the mapping, or two callers holding the
	// same key still disagree.
	reordered := []*fetcd.ServiceInfo{{Sid: 3}, {Sid: 1}, {Sid: 2}}
	sid, err := picker.Pick(keyed, "match", reordered, 0)
	if err != nil {
		t.Fatal(err)
	}
	if sid != first {
		t.Fatalf("reordering discovery changed the target from %d to %d", first, sid)
	}
	// Different keys must still spread, or affinity has become a single point.
	targets := map[int32]bool{}
	for _, key := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		sid, err := picker.Pick(WithAffinityKey(ctx, key), "match", infos, 0)
		if err != nil {
			t.Fatal(err)
		}
		targets[sid] = true
	}
	if len(targets) < 2 {
		t.Fatalf("every key routed to one instance: %v", targets)
	}
}

// orderKeyBus 记下每次 CallTo 的 ctx 里的顺序键（bus.OrderKeyFromContext）。
type orderKeyBus struct {
	bus.IBus
	keys []string
}

func (b *orderKeyBus) CallTo(ctx context.Context, _ string, _ int32, _ string, _ any, resp any) error {
	key, _ := bus.OrderKeyFromContext(ctx)
	b.keys = append(b.keys, key)
	if out, ok := resp.(*routedResponse); ok {
		*out = routedResponse{}
	}
	return nil
}

// affinity 方法的键同时是顺序键：选中的实例上同一个键的请求按到达顺序逐个处理（保持此前同一方法串行时“同键不互相
// 争用”的效果），不同键并行；路由 RPC 的顺序键是实体 ID；不带 affinity 键的调用不带顺序键。
func TestAffinityAndRoutedCallsCarryAnOrderKey(t *testing.T) {
	ctx := context.Background()
	b := &orderKeyBus{}
	client := NewDiscoveredBusClient(b, "match", time.Second, &fakeDiscovery{infos: []*fetcd.ServiceInfo{{Sid: 1}, {Sid: 2}}},
		WithKeyAffinity(AffinityKeyFromContext))
	if err := client.CallDiscoveredChecked(WithAffinityKey(ctx, "queue:ranked:2"), "Enqueue", struct{}{}, &routedResponse{}, "enqueue"); err != nil {
		t.Fatal(err)
	}
	if err := client.CallDiscoveredChecked(ctx, "Enqueue", struct{}{}, &routedResponse{}, "enqueue"); err != nil {
		t.Fatal(err)
	}
	routed := NewBusClient(b, "alliance", time.Second)
	if err := routed.CallRoutedChecked(ctx, &mapResolver{owners: map[int64]int32{routedAlliance: 3000}}, routedAlliance, "Join", struct{}{}, &routedResponse{}, "join"); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("[queue:ranked:2  %d]", routedAlliance); fmt.Sprint(b.keys) != want {
		t.Fatalf("order keys %q; want %s (affinity key, none, entity id)", b.keys, want)
	}
}

// With no key in context the picker must fall back, not fail or pin
// everything to one instance.
func TestKeyAffinityFallsBackWithoutAKey(t *testing.T) {
	infos := []*fetcd.ServiceInfo{{Sid: 1}, {Sid: 2}}
	picker := KeyAffinityPicker{Key: AffinityKeyFromContext}
	seen := map[int32]bool{}
	for sequence := uint64(0); sequence < 4; sequence++ {
		sid, err := picker.Pick(context.Background(), "match", infos, sequence)
		if err != nil {
			t.Fatal(err)
		}
		seen[sid] = true
	}
	if len(seen) != 2 {
		t.Fatalf("fallback did not spread: %v", seen)
	}
	if _, err := (KeyAffinityPicker{}).Pick(context.Background(), "match", nil, 0); err == nil {
		t.Fatal("an empty instance set was accepted")
	}
}

func TestOptionsFromConfigSelectsTheReliableTransport(t *testing.T) {
	client := NewBusClient(&recordingBus{}, "rank", time.Second, OptionsFromConfig(stubConfig{"jetstream"})...)
	if client.transport != TransportJetStream {
		t.Fatalf("transport = %q, want jetstream", client.transport)
	}
	plain := NewBusClient(&recordingBus{}, "rank", time.Second, OptionsFromConfig(stubConfig{""})...)
	if plain.transport == TransportJetStream {
		t.Fatal("an unset transport selected jetstream")
	}
}

type stubConfig struct{ value string }

func (c stubConfig) GetString(string) string { return c.value }

func TestBusClientRejectsANilBus(t *testing.T) {
	client := NewBusClient(nil, "rank", time.Second)
	if err := client.Call(context.Background(), 0, "rank.GetTop", struct{}{}, &statusResponse{}); !errors.Is(err, ErrBusNil) {
		t.Fatalf("a nil bus returned %v, want ErrBusNil", err)
	}
}

// plainBus is an IBus without the reliable (JetStream) calls.
type plainBus struct{ bus.IBus }

// 两个选择器对空实例表报错；JetStream 传输下总线不支持可靠调用时拒绝（定向与非定向两条路）；发现结果全无效时
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

func TestBusinessWorkersRefuseRPCBeforeTransportAndDiscovery(t *testing.T) {
	for _, option := range []fctx.Option{fctx.WithShortWorker(), fctx.WithLongWorker()} {
		func() {
			_, release := fctx.NewContext(option)
			defer release()
			var client *BusClient
			if err := client.Call(context.Background(), 1, "rank", nil, nil); !errors.Is(err, fctx.ErrBlockingInBusinessWorker) {
				t.Fatal(err)
			}
			if _, err := client.PickServer(context.Background()); !errors.Is(err, fctx.ErrBlockingInBusinessWorker) {
				t.Fatal(err)
			}
		}()
	}
	_, release := fctx.NewContext(fctx.WithIOWorker())
	defer release()
	var client *BusClient
	if err := client.Call(context.Background(), 1, "rank", nil, nil); !errors.Is(err, ErrBusNil) {
		t.Fatalf("I/O rejected before transport: %v", err)
	}
}

func TestPinnedOwnerNeverUsesQueueGroup(t *testing.T) {
	b := &recordingBus{}
	c := NewBusClient(b, "physicsroom", time.Second, WithServerID(42))
	if err := c.Call(context.Background(), 0, "Poll", nil, nil); err != nil {
		t.Fatal(err)
	}
	if b.lastSid != 42 || b.calls[0] != "callTo:physicsroom:Poll" {
		t.Fatalf("routing=%v sid=%d", b.calls, b.lastSid)
	}
	bad := NewBusClient(b, "physicsroom", time.Second, WithServerID(0))
	if err := bad.Call(context.Background(), 0, "Poll", nil, nil); err == nil {
		t.Fatal("invalid owner fell back to random routing")
	}
	if len(b.calls) != 1 {
		t.Fatal("invalid owner touched bus")
	}
}

func TestPinnedOwnerDoesNotDriftThroughDiscovery(t *testing.T) {
	b := &recordingBus{}
	c := NewDiscoveredBusClient(b, "physicsroom", time.Second, &fakeDiscovery{err: errors.New("discovery must not be used")}, WithServerID(42))
	if err := c.CallDiscoveredChecked(context.Background(), "Poll", nil, statusResponse{}, "failed"); err != nil {
		t.Fatal(err)
	}
	if b.lastSid != 42 {
		t.Fatalf("owner drifted to %d", b.lastSid)
	}
}
