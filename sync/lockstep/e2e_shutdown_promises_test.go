package lockstep_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/robot"
	"github.com/tjbdwanghaibo/roost-core/sync/lockstep"
)

// RR-20261008-40：E2E 主动停止不能在机器人正在发送时先撤销网络 context，
// 但停止前的业务错误（包括业务自己返回的 context.Canceled）必须原样保留。
func TestE2EBotShutdownWaitsForInFlightOutput(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
	}{
		{"graceful", nil}, {"business_error", errors.New("send failed")}, {"business_canceled", context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered, release := make(chan struct{}), make(chan struct{})
			sink := shutdownSink{send: func() error {
				close(entered)
				<-release
				if tc.failure != nil {
					return tc.failure
				}
				return ctx.Err()
			}}
			bot, err := robot.NewLockstepBot(robot.LockstepBotConfig{Sink: sink, Input: func(lockstep.FrameID) []byte { return []byte{1} }})
			if err != nil {
				t.Fatal(err)
			}
			stats := make(chan robot.LockstepBotStats, 1)
			result := make(chan error, 1)
			go func() { result <- bot.HandleFrames([]lockstep.Frame{{ID: 1}}); stats <- bot.Stats() }()
			<-entered
			got := finishE2EBot(cancel, func() { close(release) }, stats)
			err = <-result
			if tc.failure == nil && err != nil {
				t.Fatalf("shutdown canceled in-flight output: %v", err)
			}
			if tc.failure != nil && !errors.Is(err, tc.failure) {
				t.Fatalf("business error lost: %v", err)
			}
			if tc.failure == nil && got.InputsSubmitted != 1 {
				t.Fatalf("inputs=%d, want 1", got.InputsSubmitted)
			}
			if ctx.Err() == nil {
				t.Fatal("transport not canceled after join")
			}
		})
	}
}

type shutdownSink struct{ send func() error }

func (s shutdownSink) SubmitInput(lockstep.FrameID, []byte) error { return s.send() }
func (s shutdownSink) ReportHash(lockstep.FrameID, uint64) error  { return nil }
func (s shutdownSink) RequestCatchup(lockstep.FrameID) error      { return nil }
