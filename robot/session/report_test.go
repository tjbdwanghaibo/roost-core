package session

import (
	"context"
	"errors"
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
