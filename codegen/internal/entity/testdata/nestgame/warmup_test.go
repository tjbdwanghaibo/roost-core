package nestgame

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
)

// 预热标志随请求负载到服务端：服务端据此把预热期的请求排除在时延统计之外，采样标志不受影响。
func TestLoadRequestCarriesSampleAndWarmupFlags(t *testing.T) {
	base := loadClockOrigin.Add(time.Second)
	for _, want := range []invocation{
		{planned: base, admitted: base.Add(time.Millisecond)},
		{planned: base, admitted: base, change: true, sample: true},
		{planned: base, admitted: base, warmup: true},
		{planned: base, admitted: base, change: true, sample: true, warmup: true},
	} {
		sent := want.admitted.Add(time.Millisecond)
		payload := encodeLoadRequest(want, sent)
		got, err := decodeLoadRequest(1, wire.PayloadProtobuf, payload[:])
		if err != nil {
			t.Fatal(err)
		}
		want.sent = sent
		if !got.planned.Equal(want.planned) || !got.admitted.Equal(want.admitted) || !got.sent.Equal(want.sent) ||
			got.change != want.change || got.sample != want.sample || got.warmup != want.warmup || got.heartbeat {
			t.Fatalf("decoded %+v, want %+v", got, want)
		}
	}
	payload := encodeLoadRequest(invocation{planned: base, admitted: base}, base)
	payload[25] = 1 << 2
	if _, err := decodeLoadRequest(1, wire.PayloadProtobuf, payload[:]); err == nil {
		t.Fatal("unknown flag bit accepted")
	}
}

func TestTickerWarmupPlanAndSamples(t *testing.T) {
	start := loadClockOrigin
	for _, rate := range []int{0, 1, 3, 16, 17, 41} {
		for _, warmup := range []time.Duration{0, time.Second, 2 * time.Second} {
			const duration = time.Second
			from, end := start.Add(warmup), start.Add(warmup+duration)
			var measured, samples int
			for seq := range eventsDue(warmup+duration, rate) {
				planned := start.Add(eventOffset(seq+1, rate))
				outside, sample := tickerFlags(seq, eventsDue(warmup, rate))
				if outside == inLoadWindow(planned, from, end) || !planned.After(start) || planned.After(end) {
					t.Fatalf("rate=%d W=%v seq=%d plan=%s outside=%t", rate, warmup, seq, planned.Sub(start), outside)
				}
				if sample != (!outside && measured%16 == 0) || (warmup == 0 && sample != (seq%16 == 0)) {
					t.Fatalf("rate=%d W=%v seq=%d measured=%d sample=%t", rate, warmup, seq, measured, sample)
				}
				if !outside {
					measured++
				}
				if sample {
					samples++
				}
			}
			if measured != rate || samples != (rate+15)/16 {
				t.Fatalf("rate=%d W=%v: got %d events, %d samples", rate, warmup, measured, samples)
			}
		}
	}
}

func TestWarmupCompletionAndRoundTripGate(t *testing.T) {
	var stream loadCounters
	var client loadClients // Gate 和 embedded 共用的回包路径。
	for _, tc := range []struct {
		in invocation
		d  time.Duration
	}{
		{invocation{warmup: true, sample: true, change: true}, time.Second}, // 延迟到正式窗口才完成也仅计入总数。
		{invocation{sample: true}, 5 * time.Millisecond},
		{invocation{change: true}, 8 * time.Millisecond}, // 正式改值即使不按 1/16 抽样也测改值时延。
	} {
		if stream.recordCompletion(tc.in, tc.d) == tc.in.warmup {
			t.Fatalf("wrong segment eligibility: %+v", tc.in)
		}
		client.recordReply(tc.in, tc.d)
	}
	r, trip := stream.result(1), client.roundTrip.result()
	if r.Completed != 3 || r.MeasuredCompleted != 2 || r.EntityChanges != 2 || r.MeasuredEntityChanges != 1 || client.completed.Load() != 3 || client.measuredReplies() != 2 {
		t.Fatalf("bad total/window counts: stream %+v; replies %d/%d", r, client.completed.Load(), client.measuredReplies())
	}
	if r.Completion.Samples != 1 || r.Completion.P99MS != 5 || r.Completion.MaxMS != 8 || trip != r.Completion || r.ChangedEntityCompletion.Samples != 1 || r.ChangedEntityCompletion.P99MS != 8 {
		t.Fatalf("bad measured latencies: completion %+v, changed %+v, round trip %+v", r.Completion, r.ChangedEntityCompletion, trip)
	}
	var empty latencyResult
	if empty.passes(50, true) || !empty.passes(50, false) || !(latencyResult{Samples: 1, P99MS: 50}).passes(50, true) || (latencyResult{Samples: 1, P99MS: 50.01}).passes(50, true) {
		t.Fatal("an active stream needs a sample and p99 <= 50ms; an idle stream needs no sample")
	}
}

func TestBeatWindowAndSamples(t *testing.T) {
	from, end := loadClockOrigin, loadClockOrigin.Add(17*time.Millisecond)
	var n atomic.Uint64
	for i := -1; i <= 18; i++ {
		outside, sample := beatFlags(from.Add(time.Duration(i)*time.Millisecond), from, end, &n)
		if outside != (i <= 0 || i > 17) || sample != (i == 1 || i == 17) {
			t.Fatalf("beat %d: outside=%t sample=%t", i, outside, sample)
		}
	}
	if n.Load() != 17 {
		t.Fatalf("measured %d beats, want 17", n.Load())
	}
}

func TestBeatIntervalRequiresAdjacentMeasuredBeats(t *testing.T) {
	from := loadClockOrigin
	end := from.Add(3 * time.Second)
	l := beatLoad{from: from, end: end, lastBeat: make([]time.Time, 2)}
	beat := func(index int, at time.Time, first bool) {
		l.observeInterval(index, heartbeat.Beat{Now: at, First: first, Elapsed: time.Second})
	}
	beat(0, from, true)
	beat(0, from.Add(1900*time.Millisecond), false) // Elapsed 已被上限截断，按它倒推会把这条跨窗间隔计入。
	beat(1, from.Add(2*time.Second), true)          // 另一 Unit 的首拍不是 Unit 0 的相邻拍。
	beat(0, end, false)                             // 两端都在正式窗口，真实间隔 1100ms，超过 1Hz 门槛。
	beat(1, end, true)                              // 重启首拍不记间隔。
	beat(0, end.Add(time.Nanosecond), false)
	r := l.interval.resultWithBucket(beatIntervalBucket)
	if r.Samples != 1 || r.P99MS != 1100 || r.passes(1050, true) {
		t.Fatalf("interval: %+v", r)
	}
}

func TestBeatIntervalGateBucketEdges(t *testing.T) {
	for _, limit := range []time.Duration{100 * time.Millisecond, 150 * time.Millisecond, 1050 * time.Millisecond} {
		for _, delta := range []time.Duration{0, time.Nanosecond} {
			var h timings
			h.addWithBucket(limit+delta, true, beatIntervalBucket)
			r := h.resultWithBucket(beatIntervalBucket)
			if r.passes(ms(limit), true) != (delta == 0) {
				t.Fatalf("limit=%s delta=%s: %+v", limit, delta, r)
			}
		}
	}
}

func TestTickTailRequiresAdjacentMeasuredTicks(t *testing.T) {
	from, end := loadClockOrigin, loadClockOrigin.Add(140*time.Millisecond)
	c := tailCollector{threshold: 50 * time.Millisecond, events: make(chan tailEvent, 8)}
	for _, offsetMS := range []int{-200, -50, 0, 100, 115, 140, 200} {
		c.observeTick(from.Add(time.Duration(offsetMS)*time.Millisecond), from, end)
	}
	if n := c.seen.Load(); n != 0 { // 包括终点的 25ms 间隔恰等于（不超过）半阈值。
		t.Fatalf("counted %d warmup/cross-window/short tick gaps", n)
	}
	c = tailCollector{threshold: 50 * time.Millisecond, events: make(chan tailEvent, 8)}
	for _, at := range []time.Time{from, from.Add(100 * time.Millisecond), end, end.Add(time.Second)} {
		c.observeTick(at, from, end)
	}
	if c.seen.Load() != 1 || len(c.events) != 1 {
		t.Fatalf("got %d tick gaps, want just the one ending at end", c.seen.Load())
	}
	if ev := <-c.events; ev.Stream != "tick" || ev.TotalMS != 40 {
		t.Fatalf("wrong tick gap: %+v", ev)
	}
}
