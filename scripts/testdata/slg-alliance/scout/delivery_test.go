package scout

import (
	"context"
	"errors"
	"testing"
)

func TestReportDeliveryFailureBoundaries(t *testing.T) {
	for _, stage := range []string{"wrong-map", "wrong-key", "read-failure", "read-late", "receive-failure", "receive-late", "ack-failure", "success"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := errors.New("injected transport failure")
			received, acked := 0, 0
			r := Report{MapID: 10, Recipient: 20, HomeSID: 1000, Key: "march", Target: 503, At: 100}
			d := ReportDelivery{MapID: 10,
				Read: func(context.Context, string) (Report, error) {
					switch stage {
					case "wrong-map":
						r.MapID++
					case "wrong-key":
						r.Key = "other"
					case "read-failure":
						return Report{}, failure
					case "read-late":
						cancel()
					}
					return r, nil
				},
				Receive: func(context.Context, Report) error {
					received++
					if stage == "receive-failure" {
						return failure
					}
					if stage == "receive-late" {
						cancel()
					}
					return nil
				},
				Ack: func(context.Context, Report) error {
					acked++
					if stage == "ack-failure" {
						return failure
					}
					return nil
				},
			}
			err := d.Deliver(ctx, "march")
			if (err == nil) != (stage == "success") {
				t.Fatalf("result=%v", err)
			}
			wantReceive, wantAck := 0, 0
			switch stage {
			case "receive-failure", "receive-late":
				wantReceive = 1
			case "ack-failure", "success":
				wantReceive = 1
				wantAck = 1
			}
			if received != wantReceive || acked != wantAck {
				t.Fatalf("receive=%d ack=%d", received, acked)
			}
			if (stage == "read-late" || stage == "receive-late") && !errors.Is(err, context.Canceled) {
				t.Fatalf("late success lost cancellation: %v", err)
			}
		})
	}
}
