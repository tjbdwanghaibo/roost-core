package session

import (
	"context"
	"errors"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/robot/protocol"
	"github.com/tjbdwanghaibo/roost-core/robot/transport"
	"testing"
	"time"
)

type reportEcho struct{ *scriptedConn }

func (c reportEcho) WritePackets(p []*transport.Packet) error {
	for _, p := range p {
		c.inbound <- p
	}
	return nil
}
func TestReportSeparatesBusinessOutcomeAndCancellation(t *testing.T) {
	recorder := &Recorder{}
	registry := protocol.NewRegistry(protocol.JSONCodec{})
	if err := protocol.RegisterMessage[echoMsg](registry, msgEcho); err != nil {
		t.Fatal(err)
	}
	s := New(reportEcho{newScriptedConn()}, registry, recorder)
	defer s.Close()
	if _, err := s.Call(context.Background(), msgEcho, msgEcho, &echoMsg{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CallChecked(context.Background(), msgEcho, msgEcho, &echoMsg{}, func(*Message) error { return ErrRejected }); !errors.Is(err, ErrRejected) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Call(ctx, msgEcho, msgEcho, &echoMsg{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	if _, err := s.Call(ctx, msgEcho, msgEcho, &echoMsg{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	report := recorder.Snapshot()
	got := report.Messages[0]
	if got.Started != 4 || got.Completed != 4 || got.Inflight != 0 || got.Sent != 2 || got.OK != 1 || got.Rejected != 1 || got.Timeout != 1 || got.Canceled != 1 || got.Errors != 0 {
		t.Fatalf("misclassified: %+v", got)
	}
	report.Messages[0].Started = 99
	if recorder.Snapshot().Messages[0].Started != 4 {
		t.Fatal("snapshot aliases recorder")
	}
}
func TestReportBoundsAndQuantiles(t *testing.T) {
	r := &Recorder{}
	for i := range 1000 {
		series := r.begin(uint32(i))
		r.finish(series, uint32(i), "ok", time.Duration(i+1)*time.Microsecond)
	}
	s := r.Snapshot()
	if len(s.Messages) > 257 || len(s.Slowest) != 32 || s.Slowest[0].DurationMS != 1 {
		t.Fatalf("unbounded or wrong tail: %+v", s)
	}
	var total int64
	for _, m := range s.Messages {
		total += m.Completed
		if m.P99MS > m.MaxMS || m.P50MS > m.P99MS {
			t.Fatal(m)
		}
	}
	if total != 1000 {
		t.Fatal(total)
	}
}

// 逐步计时是“整场景时间花在哪一步”的依据：每步的样本数、成败、时长分位数、
// Retry 次数与 Wait 时间都要如实汇总，步骤按名字排序，步骤名个数有上限、超出的并进溢出项不丢样本。
func TestStepReportAggregatesTimeRetriesAndWaits(t *testing.T) {
	r := &Recorder{}
	r.FinishStep("02 retry", StepSample{Elapsed: 100 * time.Millisecond, Waited: 50 * time.Millisecond, RetryAttempts: 1, OK: true})
	r.FinishStep("02 retry", StepSample{Elapsed: 200 * time.Millisecond, Waited: 100 * time.Millisecond, RetryAttempts: 2, OK: true})
	r.FinishStep("02 retry", StepSample{Elapsed: time.Second, Waited: 500 * time.Millisecond, RetryAttempts: 6})
	r.FinishStep("01 action", StepSample{Elapsed: 3 * time.Millisecond, OK: true})
	steps := r.StepSnapshot()
	if len(steps) != 2 || steps[0].Name != "01 action" || steps[1].Name != "02 retry" {
		t.Fatalf("steps = %+v", steps)
	}
	got := steps[1]
	if got.Samples != 3 || got.OK != 2 || got.Failed != 1 || got.RetryAttemptsMax != 6 || got.RetryAttemptsAvg != 3 || got.MaxMS != 1000 || got.P95MS != 1000 {
		t.Fatalf("retry step = %+v", got)
	}
	if got.MeanMS < 433 || got.MeanMS > 434 || got.WaitMeanMS < 216 || got.WaitMeanMS > 217 || got.P50MS < 200 || got.P50MS > 200*1.09 {
		t.Fatalf("retry step means / p50 = %+v", got)
	}
	if steps[0].Samples != 1 || steps[0].RetryAttemptsAvg != 0 || steps[0].WaitMeanMS != 0 || steps[0].MaxMS != 3 {
		t.Fatalf("plain step = %+v", steps[0])
	}
	steps[1].Samples = 99
	if r.StepSnapshot()[1].Samples != 3 {
		t.Fatal("step snapshot aliases recorder")
	}

	bounded := &Recorder{}
	for i := range 300 {
		bounded.FinishStep(fmt.Sprintf("step %03d", i), StepSample{Elapsed: time.Millisecond, OK: true})
	}
	var total int64
	overflow := 0
	all := bounded.StepSnapshot()
	for _, s := range all {
		total += s.Samples
		if s.Overflow {
			overflow++
		}
	}
	if len(all) != 257 || overflow != 1 || total != 300 {
		t.Fatalf("unbounded or lost steps: len=%d overflow=%d total=%d", len(all), overflow, total)
	}
}
