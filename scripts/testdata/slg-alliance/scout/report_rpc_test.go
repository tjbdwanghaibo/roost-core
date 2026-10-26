package scout

import (
	"context"
	"errors"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	"testing"
	"time"
)

// 普通 Bus 的 caller deadline 不传播；这里专门验证接收端自己的期限和迟到成功边界。
func TestRoutedReportReceiverBudget(t *testing.T) {
	route := OwnerRoute{SID: 3100, Marker: 1, Route: 1}
	report := Report{MapID: 10, Recipient: 20, HomeSID: 1001, Key: "march", Target: 503, At: 100}
	codec := bus.MessagePackCodec{}
	raw, err := codec.Marshal(RoutedReport{Report: report, Owner: route})
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"resolve", "receive", "invalid"} {
		t.Run(stage, func(t *testing.T) {
			c := bus.NewRPCContext(context.Background(), "receive", raw, codec)
			c.FromSid = route.SID
			received := false
			r := RoutedReportReceiver{Timeout: 5 * time.Millisecond, HomeSID: 1001, MapID: 10, Recipient: 20,
				Resolve: func(ctx context.Context, _ int64) (OwnerRoute, error) {
					if stage == "invalid" {
						t.Fatal("invalid budget queried authority")
					}
					if stage == "resolve" {
						<-ctx.Done()
					}
					return route, nil
				},
				Receive: func(ctx context.Context, _ Report) error { received = true; <-ctx.Done(); return nil },
			}
			if stage == "invalid" {
				r.Timeout = -time.Second
			}
			out, err := r.Handle(c)
			if err == nil || out != nil {
				t.Fatalf("late success acknowledged: out=%v err=%v", out, err)
			}
			if stage != "invalid" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("deadline lost: %v", err)
			}
			if received != (stage == "receive") {
				t.Fatalf("receive called=%v", received)
			}
		})
	}
}
