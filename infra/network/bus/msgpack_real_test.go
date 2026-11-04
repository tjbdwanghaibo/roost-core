//go:build integration

package bus

import (
	"context"
	"fmt"
	"math"
	"os"
	"reflect"
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
