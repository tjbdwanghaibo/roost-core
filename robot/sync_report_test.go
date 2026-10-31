package robot

import (
	"sync"
	"testing"
	"time"
)

func TestSyncProbeSeparatesDeliveryTimeoutAndReconnect(t *testing.T) {
	p, err := NewSyncProbe(2)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	full := SyncKey{1, 1, SyncFull}
	delta := SyncKey{1, 2, SyncDelta}
	remove := SyncKey{1, 3, SyncRemove}
	if err = p.Expect(full, start, time.Second); err != nil {
		t.Fatal(err)
	}
	if err = p.Expect(full, start, time.Second); err == nil {
		t.Fatal("duplicate reset start")
	}
	if err = p.Expect(delta, start, time.Second); err != nil {
		t.Fatal(err)
	}
	if err = p.Expect(remove, start, time.Second); err == nil {
		t.Fatal("unbounded pending")
	}
	if !p.Observe(full, start.Add(900*time.Microsecond)) {
		t.Fatal("full missing")
	}
	if p.Observe(full, start.Add(time.Millisecond)) {
		t.Fatal("duplicate counted")
	}
	p.CancelConnection(1)
	if p.Observe(delta, start.Add(time.Millisecond)) {
		t.Fatal("closed connection counted")
	}
	remove.Connection = 2
	if err = p.Expect(remove, start, time.Second); err != nil {
		t.Fatal(err)
	}
	if p.Observe(remove, start.Add(time.Second)) {
		t.Fatal("expired counted as success")
	}
	r := p.Snapshot(start.Add(time.Second))
	if r.Slowest[0].Connection != 1 || r.Slowest[0].Sequence != 1 || r.Events[0].Received != 1 || r.Events[0].P99MS != .9 || r.Events[1].Canceled != 1 || r.Events[2].TimedOut != 1 || r.CapacityRejected != 1 || r.Unmatched != 3 {
		t.Fatalf("report=%+v", r)
	}
	for _, v := range r.Events {
		if v.Expected != v.Received+v.Pending+v.TimedOut+v.Canceled {
			t.Fatal("lost accounting", v)
		}
	}
	r.Events[0].Received = 999
	r.Slowest[0].DurationMS = 999
	if v := p.Snapshot(start.Add(time.Second)); v.Events[0].Received != 1 || v.Slowest[0].DurationMS != .9 {
		t.Fatal("report aliases retained state")
	}
}

func TestSyncProbeRejectsInvalidAndKeepsBoundedSlowSamples(t *testing.T) {
	if _, err := NewSyncProbe(0); err == nil {
		t.Fatal("invalid budget")
	}
	p, _ := NewSyncProbe(1)
	now := time.Now()
	if err := p.Expect(SyncKey{1, 1, "unknown"}, now, time.Second); err == nil {
		t.Fatal("unknown kind")
	}
	for i := uint64(1); i <= 100; i++ {
		key := SyncKey{1, i, SyncDelta}
		if err := p.Expect(key, now, time.Second); err != nil {
			t.Fatal(err)
		}
		if p.Observe(key, now.Add(-time.Millisecond)) {
			t.Fatal("negative duration accepted")
		}
		if !p.Observe(key, now.Add(time.Duration(i)*time.Millisecond)) {
			t.Fatal("missing sample")
		}
	}
	r := p.Snapshot(now)
	if len(r.Slowest) != 32 || r.Events[1].Received != 100 || r.Events[1].MaxMS != 100 || r.Events[1].Pending != 0 {
		t.Fatalf("report=%+v", r)
	}
}

// 多个机器人共享报告时，控制面快照可以与登记/完成并发，连接代际不能串样本。
func TestSyncProbeConcurrentConnections(t *testing.T) {
	probe, err := NewSyncProbe(32)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for connection := uint64(1); connection <= 16; connection++ {
		workers.Go(func() {
			for sequence := uint64(1); sequence <= 64; sequence++ {
				key := SyncKey{Connection: connection, Sequence: sequence, Kind: SyncDelta}
				start := time.Now()
				if err := probe.Expect(key, start, time.Hour); err != nil {
					t.Error(err)
					return
				}
				_ = probe.Snapshot(time.Now())
				if !probe.Observe(key, time.Now()) {
					t.Error("connection sample lost")
					return
				}
			}
			probe.CancelConnection(connection)
		})
	}
	workers.Wait()
	report := probe.Snapshot(time.Now())
	delta := report.Events[1]
	if delta.Expected != 1024 || delta.Received != 1024 || delta.Pending != 0 || delta.Canceled != 0 || delta.TimedOut != 0 || report.Unmatched != 0 || report.CapacityRejected != 0 {
		t.Fatalf("concurrent accounting: %+v", report)
	}
}
