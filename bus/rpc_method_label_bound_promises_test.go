package bus

// RR-20261006-19：JetStream 可靠 RPC 的 bus_rpc_pending{method}（第十二轮 O3）与同源的
// bus_rpc_call_total{method}、被调方的 bus_rpc_request_total{method} / bus_rpc_consumer_delivery{method}
// 没有删除入口，也没有自己的上界：调用方的 method 是 CallReliable 的任意字符串，被调方的 method 取自
// 请求 envelope 的 MsgName，每个新值多一组序列（pendingByMethod 也多一条，永不删除），直到注册表
// 的每指标上限 2048 把后来的序列连同真实方法的一起丢掉。
//
// method 没有注销概念（HandleRpc 只注册、Bus 没有撤销单个方法的入口），所以承诺是有界而不是删除：
//   - 被调方：标签只取本进程注册过的方法，其余记为 "_unregistered"——上界是 HandleRpc 注册的方法数 + 1；
//   - 调用方：每个 Bus 至多 jetStreamRPCMethodLabelLimit 个不同的方法有自己的标签，之后的新方法记为
//     "_other"（调用照常进行，只是指标合并）——上界是 limit + 1，pendingByMethod 同界。
// 框架生成的 RPC 客户端方法全部是生成常量（全仓 57 个），远在上限之内，不受影响。

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/metrics"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
)

func methodLabels(name string) map[string]bool {
	out := map[string]bool{}
	for _, metric := range metrics.Snapshot() {
		if metric.Name == name {
			out[metric.Labels["method"]] = true
		}
	}
	return out
}

func TestCallerRPCMethodLabelsAreBounded(t *testing.T) {
	metrics.DefaultRegistry().Reset()
	t.Cleanup(func() { metrics.DefaultRegistry().Reset() })
	js := &captureJetStreamRPC{}
	b := New(nil, nil, nil, Config{Sid: 2001, SvcType: "game"})
	if err := b.EnableJetStreamRPC(js, JetStreamRPCConfig{CallTimeout: time.Second, RequestTTL: time.Second}); err != nil {
		t.Fatalf("EnableJetStreamRPC: %v", err)
	}
	js.publishHook = func(_ string, data []byte, _ fnats.JetStreamPublishOptions) {
		var req fnats.NatsMsg
		if err := b.codec.Unmarshal(data, &req); err != nil {
			t.Errorf("unmarshal request: %v", err)
			return
		}
		if handler := js.handlerForFilter("roost.rpc_resp.2001.>"); handler != nil {
			_ = handler(context.Background(), &fnats.JetStreamMsg{Subject: req.ReplySubject, Data: rpcTestSuccessBytes(b, map[string]int{"code": 0})})
		}
	}

	calls := jetStreamRPCMethodLabelLimit + 44
	for i := range calls {
		var resp struct{}
		if err := b.CallReliable(context.Background(), "svc", fmt.Sprintf("svc.M%03d", i), nil, &resp); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	for _, name := range []string{"bus_rpc_pending", "bus_rpc_call_total"} {
		if got := len(methodLabels(name)); got > jetStreamRPCMethodLabelLimit+1 {
			t.Fatalf("%s has %d method labels after %d distinct methods; the bound is %d", name, got, calls, jetStreamRPCMethodLabelLimit+1)
		}
	}
	tracked := 0
	b.jsRPC.pendingByMethod.Range(func(any, any) bool { tracked++; return true })
	if tracked > jetStreamRPCMethodLabelLimit+1 {
		t.Fatalf("the bus tracks %d pending counters after %d distinct methods; the bound is %d", tracked, calls, jetStreamRPCMethodLabelLimit+1)
	}
	if got := busMetricValue("bus_rpc_call_total", map[string]string{"method": "svc.M000", "result": "ok"}); got != 1 {
		t.Fatalf("a method within the bound lost its own label: ok calls = %d, want 1", got)
	}
	if got := busMetricValue("bus_rpc_call_total", map[string]string{"method": jetStreamRPCOtherMethod, "result": "ok"}); got != int64(calls-jetStreamRPCMethodLabelLimit) {
		t.Fatalf("calls past the bound counted under %q = %d, want %d", jetStreamRPCOtherMethod, got, calls-jetStreamRPCMethodLabelLimit)
	}
}

func TestServedRPCMethodLabelsAreTheRegisteredMethods(t *testing.T) {
	metrics.DefaultRegistry().Reset()
	t.Cleanup(func() { metrics.DefaultRegistry().Reset() })
	js := &captureJetStreamRPC{}
	b := New(nil, nil, nil, Config{Sid: 5001, SvcType: "mail"})
	if err := b.EnableJetStreamRPC(js, JetStreamRPCConfig{RequestTTL: time.Second}); err != nil {
		t.Fatalf("EnableJetStreamRPC: %v", err)
	}
	if err := b.HandleRpc("mail.List", func(*RpcContext) (any, error) { return map[string]int{"code": 0}, nil }); err != nil {
		t.Fatalf("HandleRpc: %v", err)
	}
	handler := js.handlerForFilter("roost.rpc.mail.mail.List")
	if handler == nil {
		t.Fatalf("service rpc subscription was not registered: %+v", js.consumers)
	}
	deliver := func(method, id string) {
		t.Helper()
		data, err := b.codec.Marshal(&fnats.NatsMsg{
			FromSid: 2001, MsgName: method, SessionId: id, MsgID: id,
			ReplySubject: "roost.rpc_resp.2001." + id, DeadlineAt: time.Now().Add(time.Second).UnixMilli(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := handler(context.Background(), &fnats.JetStreamMsg{Subject: "roost.rpc.mail.mail.List", Data: data, NumDelivered: 1}); err != nil {
			t.Fatalf("handler(%s): %v", method, err)
		}
	}
	deliver("mail.List", "req-ok")
	for i := range 300 {
		deliver(fmt.Sprintf("forged.M%03d", i), fmt.Sprintf("req-%d", i))
	}
	want := map[string]bool{"mail.List": true, jetStreamRPCUnregisteredMethod: true}
	for _, name := range []string{"bus_rpc_request_total", "bus_rpc_consumer_delivery"} {
		labels := methodLabels(name)
		for label := range labels {
			if !want[label] {
				t.Fatalf("%s has %d method labels (e.g. %q) after 300 requests naming unregistered methods; want only %v", name, len(labels), label, want)
			}
		}
	}
	if got := busMetricValue("bus_rpc_request_total", map[string]string{"method": jetStreamRPCUnregisteredMethod, "result": "no_handler"}); got != 300 {
		t.Fatalf("requests for unregistered methods counted %d, want 300", got)
	}
	if got := busMetricValue("bus_rpc_request_total", map[string]string{"method": "mail.List", "result": "ok"}); got != 1 {
		t.Fatalf("the registered method's requests counted %d, want 1", got)
	}
}
