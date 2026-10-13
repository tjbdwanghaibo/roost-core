package gateway

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"

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
