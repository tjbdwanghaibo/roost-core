//go:build integration

package bus

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
)

type realCodecMessage struct {
	ID      int64
	Payload []byte
}

func TestRealMessagePackCoreRPCErrorAndAsyncBus(t *testing.T) {
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Skip("private NATS URL not configured")
	}
	assembly, err := driver.Assemble(fnats.DefaultConfig(url), driver.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(assembly.Client.Close)
	prefix := fmt.Sprintf("roost.codec.%d", time.Now().UnixNano())
	server := New(assembly.Client, assembly.RPC, nil, Config{Sid: 7301, SvcType: "game", Prefix: prefix, WorkerNum: 2, QueueCap: 8})
	client := New(assembly.Client, assembly.RPC, nil, Config{Sid: 7302, SvcType: "game", Prefix: prefix, WorkerNum: 2, QueueCap: 8})
	for _, b := range []*Bus{server, client} {
		if err := b.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := b.StopWithContext(ctx); err != nil {
				t.Error(err)
			}
		})
	}
	if err := server.HandleRpc("Echo", func(ctx *RpcContext) (any, error) {
		var msg realCodecMessage
		if err := ctx.Decode(&msg); err != nil {
			return nil, err
		}
		return msg, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := server.HandleRpc("Denied", func(*RpcContext) (any, error) { return nil, errcode.Remote(4242, "denied", "business refused") }); err != nil {
		t.Fatal(err)
	}
	received := make(chan realCodecMessage, 1)
	if err := server.Handle("world", msgName(realCodecMessage{}), func(ctx *MsgContext) {
		var msg realCodecMessage
		if err := ctx.Decode(&msg); err != nil {
			t.Error(err)
			return
		}
		received <- msg
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := assembly.Client.FlushContext(ctx); err != nil {
		t.Fatal(err)
	}
	want := realCodecMessage{ID: math.MaxInt64, Payload: []byte{0, 255, 7}}
	var got realCodecMessage
	if err := client.CallTo(ctx, "game", 7301, "Echo", want, &got); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("RPC=%+v err=%v", got, err)
	}
	if err := client.CallTo(ctx, "game", 7301, "Denied", want, &got); errcode.CodeOf(err) != 4242 {
		t.Fatalf("business error=%v", err)
	}
	if err := client.SendByType("game", 7301, "world", want); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-received:
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("async=%+v", got)
		}
	case <-ctx.Done():
		t.Fatal("async message did not arrive")
	}
}

// 缩容第一步在真实 NATS 上（联盟服务方案 §2.7）：实例 A 退出 Found 的队列组后，不指定 sid 的 Call 全部落到实例 B；
// CallTo A 的实例主题照常由 A 处理。
func TestRealLeaveRPCQueueStopsNewQueueRequestsReachingTheInstance(t *testing.T) {
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Skip("private NATS URL not configured")
	}
	assembly, err := driver.Assemble(fnats.DefaultConfig(url), driver.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(assembly.Client.Close)
	prefix := fmt.Sprintf("roost.leave.%d", time.Now().UnixNano())
	newBus := func(sid int32) *Bus {
		b := New(assembly.Client, assembly.RPC, nil, Config{Sid: sid, SvcType: "alliance", Prefix: prefix, WorkerNum: 2, QueueCap: 64})
		if err := b.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := b.StopWithContext(ctx); err != nil {
				t.Error(err)
			}
		})
		return b
	}
	a, b, caller := newBus(3000), newBus(3001), newBus(9000)
	for _, server := range []*Bus{a, b} {
		sid := server.cfg.Sid
		if err := server.HandleRpc("Found", func(*RpcContext) (any, error) { return sid, nil }); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := assembly.Client.FlushContext(ctx); err != nil {
		t.Fatal(err)
	}
	served := func(calls int) map[int32]int {
		counts := map[int32]int{}
		for range calls {
			var sid int32
			if err := caller.Call(ctx, "alliance", "Found", struct{}{}, &sid); err != nil {
				t.Fatal(err)
			}
			counts[sid]++
		}
		return counts
	}
	if before := served(64); before[3000] == 0 || before[3001] == 0 {
		t.Fatalf("before leaving, the queue group served %v; want both instances", before)
	}
	if err := a.LeaveRPCQueue("Found"); err != nil {
		t.Fatal(err)
	}
	if err := assembly.Client.FlushContext(ctx); err != nil {
		t.Fatal(err)
	}
	if after := served(64); after[3000] != 0 || after[3001] != 64 {
		t.Fatalf("after 3000 left the queue group, Call was served %v; want all 64 by 3001", after)
	}
	var sid int32
	if err := caller.CallTo(ctx, "alliance", 3000, "Found", struct{}{}, &sid); err != nil || sid != 3000 {
		t.Fatalf("CallTo the instance that left = %d, %v; its instance subject must keep serving", sid, err)
	}
}

// 撤下 handler（WithdrawRpc）在真实 NATS 上：撤下开始之后发往本实例的每个请求都有答复——撤下前已进 handler 的照常
// 答复，排空期间到达的答 CodeRetryable，排空之后的得到 no responders。服务端与调用方连在同一个节点上。
//
// ROOST_WITHDRAW_SPLIT_NODES=1 时两边连集群的不同节点，只记录结局、不断言：退订在集群里传播完之前，别的节点转来的
// 请求到达时本地订阅已经没了、被服务端丢掉，调用方等到超时（2026-11-04 私有 3 节点 10 次里 3 次、每次 3～4 个请求）。
// 这个窗口任何订阅退掉时都有（进程退出、Bus 停止同样），WithdrawRpc 没有新增它，见 KNOWN-LIMITS。
func TestRealWithdrawRpcAnswersEveryRequestDefinitely(t *testing.T) {
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Skip("private NATS URL not configured")
	}
	prefix := fmt.Sprintf("roost.withdraw.%d", time.Now().UnixNano())
	urls := strings.Split(url, ",")
	split := os.Getenv("ROOST_WITHDRAW_SPLIT_NODES") == "1" && len(urls) > 1
	newBus := func(sid int32) *Bus {
		node := urls[0]
		if sid != 3000 && split {
			node = urls[1]
		}
		assembly, err := driver.Assemble(fnats.DefaultConfig(node), driver.ClientOptions{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(assembly.Client.Close)
		b := New(assembly.Client, assembly.RPC, nil, Config{Sid: sid, SvcType: "alliance", Prefix: prefix, WorkerNum: 4, QueueCap: 256})
		if err := b.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := b.StopWithContext(ctx); err != nil {
				t.Error(err)
			}
		})
		return b
	}
	server, caller := newBus(3000), newBus(9000)
	// Slow 的第一个请求卡在 handler 里（在 Nest 里等提交确认）；Join 一直有请求进来。两个方法在不同的派发 worker 上，
	// 撤下期间 Join 的请求照常到达。
	entered, release := make(chan struct{}), make(chan struct{})
	if err := server.HandleRpc("Slow", func(*RpcContext) (any, error) {
		close(entered)
		<-release
		return int32(3000), nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := server.HandleRpc("Join", func(*RpcContext) (any, error) { return int32(3000), nil }); err != nil {
		t.Fatal(err)
	}
	if hashString("Slow")%4 == hashString("Join")%4 {
		t.Fatal("Slow and Join share a dispatch worker; rename one")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	time.Sleep(100 * time.Millisecond) // 两条连接各自的订阅传播到集群
	inflight := make(chan error, 1)
	go func() {
		var sid int32
		inflight <- caller.CallTo(ctx, "alliance", 3000, "Slow", struct{}{}, &sid)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first request never reached the handler")
	}

	// 每个请求的结局按类计数（不攒在通道里：请求很快，有界通道会把发请求的 goroutine 卡住）。
	var mu sync.Mutex
	counts := map[string]int{}
	var withdrawn atomic.Bool
	tally := func(err error, after bool) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case err == nil && !after:
			counts["served before"]++
		case errcode.CodeOf(err) == errcode.CodeRetryable && errors.As(err, new(*errcode.IntError)):
			counts["retryable"]++
		case errors.Is(err, fnats.ErrNoResponders):
			counts["no responders"]++
		default:
			if counts["indefinite"] == 0 && !split {
				t.Errorf("a request sent %s the withdrawal was answered %v; want an answer, CodeRetryable or no responders",
					map[bool]string{false: "around", true: "after"}[after], err)
			}
			counts["indefinite"]++
		}
	}
	stopHammer := make(chan struct{})
	var hammers sync.WaitGroup
	for range 4 {
		hammers.Add(1)
		go func() {
			defer hammers.Done()
			for {
				select {
				case <-stopHammer:
					return
				default:
				}
				after := withdrawn.Load()
				callCtx, callCancel := context.WithTimeout(ctx, 2*time.Second)
				var sid int32
				err := caller.CallTo(callCtx, "alliance", 3000, "Join", struct{}{}, &sid)
				callCancel()
				tally(err, after)
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	short, shortCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	if err := server.WithdrawRpc(short, "Join", "Slow"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("WithdrawRpc with a request in the handler = %v; want the ctx error", err)
	}
	shortCancel()
	time.Sleep(50 * time.Millisecond)
	close(release)
	if err := server.WithdrawRpc(ctx, "Join", "Slow"); err != nil {
		t.Fatalf("WithdrawRpc after the handler returned: %v", err)
	}
	withdrawn.Store(true)
	time.Sleep(200 * time.Millisecond)
	close(stopHammer)
	hammers.Wait()
	if err := <-inflight; err != nil {
		t.Fatalf("the request admitted before the withdrawal = %v; want its own answer", err)
	}
	t.Logf("outcomes %v", counts)
	if split {
		return
	}
	if counts["no responders"] == 0 {
		t.Fatalf("no request after the withdrawal got no responders: %v", counts)
	}
	var sid int32
	if err := caller.Call(ctx, "alliance", "Join", struct{}{}, &sid); !errors.Is(err, fnats.ErrNoResponders) {
		t.Fatalf("Call through the queue group after the only instance withdrew = %v; want no responders", err)
	}
}
