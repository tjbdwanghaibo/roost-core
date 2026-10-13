package gateway

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

type testSession struct{ principal Principal }

func (s *testSession) Principal() Principal             { return s.principal }
func (s *testSession) Reply(context.Context, any) error { return nil }
func (s *testSession) Close(error) error                { return nil }

func TestChainOrderAndAuthentication(t *testing.T) {
	var order []string
	middleware := func(name string) Middleware {
		return func(next Endpoint) Endpoint {
			return EndpointFunc(func(ctx context.Context, session Session, request Request) (any, error) {
				order = append(order, name+":before")
				ret, err := next.Handle(ctx, session, request)
				order = append(order, name+":after")
				return ret, err
			})
		}
	}
	endpoint := Chain(EndpointFunc(func(context.Context, Session, Request) (any, error) {
		order = append(order, "handler")
		return "ok", nil
	}), RequireAuthenticated, middleware("a"), middleware("b"))

	if _, err := endpoint.Handle(context.Background(), &testSession{}, Request{}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("unauthenticated error = %v", err)
	}
	order = nil
	ret, err := endpoint.Handle(context.Background(), &testSession{principal: Principal{PlayerID: 7, SessionID: "s"}}, Request{})
	if err != nil || ret != "ok" {
		t.Fatalf("ret=%v err=%v", ret, err)
	}
	want := []string{"a:before", "b:before", "handler", "b:after", "a:after"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order=%v want=%v", order, want)
	}
}

type payloadClient struct {
	fnats.RawClient
	limit int64
}

func (c payloadClient) MaxPayload() (int64, error) { return c.limit, nil }
func TestCompleteChannelEnvelopeMustFitActualLimits(t *testing.T) {
	config := DefaultConfig()
	if err := checkChannelSize(config, payloadClient{limit: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	// 裸正文可装下并不代表完整信封可准入，不能启动后随机截断业务包。
	if err := checkChannelSize(config, payloadClient{limit: int64(config.MaxPayloadBytes)}); err == nil || !strings.Contains(err.Error(), "complete") {
		t.Fatalf("NATS envelope overflow=%v", err)
	}
	config.Outbound.MaxPacketBytes = config.MaxPayloadBytes
	if err := checkChannelSize(config, payloadClient{limit: 1 << 20}); err == nil {
		t.Fatal("outbound limit checked only naked payload")
	}
	config = DefaultConfig()
	config.SubscriptionBytes = 8192
	if err := checkChannelSize(config, payloadClient{limit: 1 << 20}); err == nil {
		t.Fatal("subscription bytes cannot hold one complete control/data packet")
	}
	config = DefaultConfig()
	config.ClockSkew = 2 * config.Lease
	if err := config.Validate(); err == nil {
		t.Fatal("unbounded clock skew accepted")
	}
}

// 多次取消 Stop 只取消调用方的等待；不能为同一批仍未完成的请求累积等待协程。
func TestGateStopRetriesShareOneDrainWaiter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := &Gate{}
		gate.forwards.Add(1)
		defer gate.forwards.Done()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		for range 20 {
			if err := gate.Stop(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("Stop = %v, want canceled while dispatch remains active", err)
			}
		}
		// 等全部新增协程真正阻塞，不依赖 sleep 或任意调度时机。
		synctest.Wait()
		stack := make([]byte, 1<<20)
		n := runtime.Stack(stack, true)
		if n == len(stack) {
			t.Fatal("goroutine stack truncated")
		}
		waiters := 0
		for _, block := range strings.Split(string(stack[:n]), "\n\n") {
			if strings.Contains(block, "gateway.waitRuntime.func") {
				waiters++
			}
		}
		if waiters != 1 {
			t.Fatalf("same Stop phase created %d waiters; want one", waiters)
		}
	})
}

// benchGateClient 是 Gate 基准用的 NATS 替身：Forward 的请求固定得到预先编码的应答，回执发布直接成功。
type benchGateClient struct {
	fnats.RawClient
	reply []byte
}

func (c *benchGateClient) MaxPayload() (int64, error) { return 8 << 20, nil }
func (c *benchGateClient) RequestContext(context.Context, string, []byte) ([]byte, error) {
	return c.reply, nil
}
func (c *benchGateClient) PublishOnce(string, []byte) error { return nil }

// benchGateQueue 是不落地的出站队列：准入即成功。
type benchGateQueue struct{}

func (benchGateQueue) Register(uint64, int64) error                  { return nil }
func (benchGateQueue) Enqueue(context.Context, uint64, []byte) error { return nil }
func (benchGateQueue) Remove(uint64) bool                            { return true }
func (benchGateQueue) Drain(context.Context) error                   { return nil }

type benchConnectionSession struct {
	testSession
	id string
}

func (s *benchConnectionSession) ConnectionID() string { return s.id }
func (s *benchConnectionSession) WritePacket(context.Context, uint32, uint32, wire.PayloadKind, bool, []byte) error {
	return nil
}

// benchGate 构造一个已就绪、挂着 connections 条活跃连接（都绑定同一个 Game）的 Gate，不订阅 NATS。
// Forward 的应答是 Completed 且出站水位为 0，准入后立即返回。
func benchGate(b *testing.B, connections int) (*Gate, ProcessIdentity, []*benchConnectionSession) {
	b.Helper()
	game := ProcessIdentity{ServerID: 2, Incarnation: "game-incarnation-0001"}
	identity := ProcessIdentity{ServerID: 1, Incarnation: "gate-incarnation-0001"}
	reply, err := (bus.MessagePackCodec{}).Marshal(ExecutionResult{Version: InternalVersion, Outcome: Completed})
	if err != nil {
		b.Fatal(err)
	}
	gate, err := NewGate(DefaultConfig(), identity, GateDependencies{
		Client: &benchGateClient{reply: reply},
		QueueFactory: func(OutboundLimits, func(context.Context, uint64, []byte) error, func(uint64, error)) (ReliableQueue, error) {
			return benchGateQueue{}, nil
		},
		Ready:       func() bool { return true },
		ResolveGame: func(context.Context, int32) (ProcessIdentity, error) { return game, nil },
		Matches:     func(context.Context, string, ProcessIdentity) (bool, error) { return true, nil },
	})
	if err != nil {
		b.Fatal(err)
	}
	gate.healthy.Store(true)
	sessions := make([]*benchConnectionSession, connections)
	for i := range sessions {
		nonce := newBindingToken()
		sessions[i] = &benchConnectionSession{testSession: testSession{principal: Principal{PlayerID: int64(i + 1), SessionID: nonce}}, id: nonce}
		gate.nextReceiver++
		connection := &gateConnection{
			binding:    Binding{PlayerID: int64(i + 1), SessionID: nonce, Gate: identity, Game: game, ConnectionNonce: nonce, BindID: newBindingToken()},
			session:    sessions[i],
			receiverID: gate.nextReceiver,
			active:     true,
			changed:    make(chan struct{}),
		}
		gate.connections[nonce] = connection
		gate.receivers[connection.receiverID] = connection
	}
	return gate, game, sessions
}

// BenchmarkGateForward 覆盖 Forward 的成功路径：准入与预留、Game 与全局槽、预算、编码与按实际大小结算、
// 请求应答解码、出站水位确认。
func BenchmarkGateForward(b *testing.B) {
	gate, _, sessions := benchGate(b, 1)
	payload := make([]byte, 256)
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := gate.Forward(ctx, sessions[0], 100, 1, wire.PayloadProtobuf, payload); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGateAdmitBroadcast 覆盖 admitBroadcast：内容摘要与去重记录、过期记录清理、在 Gate 锁内按受众
// 遍历连接并准入出站。每轮换一个广播 ID，记录表到达上限前清空，避免测到“已有记录”分支。
func BenchmarkGateAdmitBroadcast(b *testing.B) {
	gate, game, _ := benchGate(b, 256)
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	request := BroadcastRequest{Version: InternalVersion, Source: game, Target: gate.identity, Audience: Audience{Kind: GameOnline, GameID: game.ServerID}, MessageID: 100, Payload: make([]byte, 128)}
	ids := make([]string, 1024)
	for i := range ids {
		ids[i] = newBindingToken()
	}
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		if i%len(ids) == 0 {
			gate.mu.Lock()
			clear(gate.broadcasts)
			gate.mu.Unlock()
		}
		request.BroadcastID = ids[i%len(ids)]
		i++
		result := gate.admitBroadcast(ctx, "game", request)
		if result.Outcome != Completed || result.Accepted != 256 {
			b.Fatalf("result=%+v", result)
		}
	}
}
