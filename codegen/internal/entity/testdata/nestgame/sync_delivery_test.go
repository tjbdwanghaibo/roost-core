package nestgame

import (
	"sync/atomic"
	"testing"
	"time"
)

// 100ms 只适用于已约定的完整预热集中 c4 配置；冷启动、其他人数/扇出/入口均保持原 50ms。
func (c gameConfig) syncP99LimitMS(mode string) int {
	if mode == "on_change" && c == (gameConfig{
		Entities: 10000, Players: 1000, MessageTargets: 1000, MessagesPerPlayer: 10,
		HeartbeatHz: 1, DirtyPercent: 1, Workers: 8, IOWorkers: 8, Queue: 65536,
		Frontend: "gate", SceneSync: true, HeartbeatDriver: "ticker", Layout: "concentrated", ObservedBlocks: 4,
		Warmup: 30 * time.Second, Duration: 30 * time.Second, Settle: 60 * time.Second,
	}) {
		return 100
	}
	return 50
}

func (r *syncLoadReport) passesSyncLatency() bool {
	c := r.CommittedToClient
	if c.Samples == 0 || c.NegativeDurations != 0 || r.ChangedToClient.NegativeDurations != 0 || r.PlannedToClient.NegativeDurations != 0 {
		return false
	}
	switch r.CommittedP99LimitMS {
	case 50:
		return c.P99MS <= 50
	case 100:
		// All 包括正式计划但截止后才能解码的增量。100ms 是宽桶边界；-1（空或 >60s 溢出）不能通过。
		all := r.CommittedDeliveries.All
		return all.Samples == c.Samples && all.P99LEMS >= 0 && all.P99LEMS <= 100
	default:
		return false
	}
}

// ticker 的第一条计划在 start 之后、末条恰在 end；前后两段采用同一 (from,end] 口径。
type syncMeasureWindow struct{ from, end int64 }

func (load *syncGameLoad) setMeasureWindow(from, end time.Time) {
	load.window.Store(&syncMeasureWindow{loadTimestamp(from), loadTimestamp(end)})
}

func (w *syncMeasureWindow) includes(planned int64) bool {
	return w != nil && planned > w.from && planned <= w.end
}

// 0，正时长向上取整到 10ms（直至 60s），另列 >60s。沿用客户端同一个 now，不在发压中排序。
const syncWideBuckets = 6002

type syncDeliveryHist [2][syncWideBuckets]atomic.Uint64

// 两个直方图复用同一个解码时钟端点；窄表保留 50ms 判据、负时长和实测最大值。
func (load *syncGameLoad) recordDelivery(w *syncMeasureWindow, committed int64, decoded time.Time) {
	d := decoded.Sub(loadTime(committed))
	load.committed.add(d, true)
	load.deliveries.add(loadTimestamp(decoded) > w.end, d)
}

type syncDeliveryPart struct {
	Samples, Over50MS, Over60S uint64
	// -1 表示该段没有样本或分位落在溢出桶；由 Samples 区分。
	P50LEMS, P95LEMS, P99LEMS int
}

type syncDeliveryReport struct {
	// 截止后含最后的调度、network.drain、引擎排空及最终 awaitValues，不是纯收尾耗时。
	ThroughWindowEnd, AfterWindowEnd, All syncDeliveryPart
}

func (h *syncDeliveryHist) add(afterEnd bool, d time.Duration) {
	phase, bin := 0, 0
	if afterEnd {
		phase = 1
	}
	if d > 0 {
		bin = min(syncWideBuckets-1, int((d-1)/(10*time.Millisecond))+1)
	}
	h[phase][bin].Add(1)
}

func syncDeliveryQuantiles(bins *[syncWideBuckets]uint64) syncDeliveryPart {
	var r syncDeliveryPart
	for i, n := range bins {
		r.Samples += n
		if i > 5 {
			r.Over50MS += n
		}
	}
	r.Over60S = bins[syncWideBuckets-1]
	q := func(p uint64) int {
		if r.Samples == 0 {
			return -1
		}
		var cum uint64
		for i, n := range bins {
			cum += n
			if cum >= (r.Samples*p+99)/100 {
				if i == syncWideBuckets-1 {
					return -1
				}
				return i * 10
			}
		}
		panic("invalid sync delivery histogram")
	}
	r.P50LEMS, r.P95LEMS, r.P99LEMS = q(50), q(95), q(99)
	return r
}

func (h *syncDeliveryHist) result() syncDeliveryReport {
	var in, after, all [syncWideBuckets]uint64
	for i := range all {
		in[i], after[i] = h[0][i].Load(), h[1][i].Load()
		all[i] = in[i] + after[i]
	}
	return syncDeliveryReport{syncDeliveryQuantiles(&in), syncDeliveryQuantiles(&after), syncDeliveryQuantiles(&all)}
}

func TestSyncP99HighPressureScope(t *testing.T) {
	c := gameConfig{Entities: 10000, Players: 1000, MessageTargets: 1000, MessagesPerPlayer: 10, HeartbeatHz: 1, DirtyPercent: 1, Workers: 8, IOWorkers: 8, Queue: 65536, Frontend: "gate", SceneSync: true, HeartbeatDriver: "ticker", Layout: "concentrated", ObservedBlocks: 4, Duration: 30 * time.Second, Warmup: 30 * time.Second, Settle: time.Minute}
	if c.syncP99LimitMS("on_change") != 100 || c.syncP99LimitMS("periodic") != 50 {
		t.Fatal("wrong gate for warmed c4 / periodic")
	}
	for _, change := range []func(*gameConfig){
		func(c *gameConfig) { c.Warmup = 0 }, func(c *gameConfig) { c.Players = 400 },
		func(c *gameConfig) { c.ObservedBlocks = 0 }, func(c *gameConfig) { c.Frontend = "embedded" },
		func(c *gameConfig) { c.DirtyPercent = 2 },
	} {
		other := c
		change(&other)
		if other.syncP99LimitMS("on_change") != 50 {
			t.Fatalf("nonmatching config received high-pressure limit: %+v", other)
		}
	}
}

func TestSyncWideP99GateBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name                string
		limit, onTime, late int
		d, lateD            time.Duration
		want                bool
	}{
		{"empty", 100, 0, 0, 0, 0, false},
		{"exact100", 100, 1, 0, 100 * time.Millisecond, 0, true},
		{"over100", 100, 1, 0, 100*time.Millisecond + 1, 0, false},
		{"one_percent_late_over", 100, 99, 1, 100 * time.Millisecond, time.Second, true},
		{"two_percent_late_over", 100, 98, 2, 100 * time.Millisecond, time.Second, false},
		{"wide_overflow", 100, 1, 0, time.Minute + 1, 0, false},
		{"negative", 100, 1, 0, -1, 0, false},
		{"original50", 50, 1, 0, 50 * time.Millisecond, 0, true},
		{"original50_over", 50, 1, 0, 50*time.Millisecond + 1, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wide syncDeliveryHist
			var narrow timings
			for i := 0; i < tc.onTime+tc.late; i++ {
				after := i >= tc.onTime
				d := tc.d
				if after {
					d = tc.lateD
				}
				wide.add(after, d)
				narrow.add(d, true)
			}
			r := syncLoadReport{CommittedP99LimitMS: tc.limit, CommittedToClient: narrow.result(), CommittedDeliveries: wide.result()}
			if r.passesSyncLatency() != tc.want {
				t.Fatalf("limit=%d narrow=%+v wide=%+v want=%v", tc.limit, r.CommittedToClient, r.CommittedDeliveries, tc.want)
			}
			if tc.name == "exact100" {
				r.PlannedToClient.NegativeDurations = 1
				if r.passesSyncLatency() {
					t.Fatal("negative planned clock accepted")
				}
				r.PlannedToClient.NegativeDurations = 0
				r.ChangedToClient.NegativeDurations = 1
				if r.passesSyncLatency() {
					t.Fatal("negative change clock accepted")
				}
			}
		})
	}
}

func TestSyncWarmupWindowAndDeliveryCut(t *testing.T) {
	load := &syncGameLoad{}
	from := loadClockOrigin.Add(time.Second)
	end := from.Add(time.Second)
	if load.window.Load().includes(loadTimestamp(from)) {
		t.Fatal("unpublished window measured an event")
	}
	load.setMeasureWindow(from, end)
	w := load.window.Load()
	for _, tc := range []struct {
		planned, decoded time.Time
		accepted         bool
		lag              time.Duration
	}{
		{from.Add(-time.Nanosecond), end, false, time.Second}, // 预热变更晚到
		{from, end, false, time.Second},                       // 边界属于预热
		{from.Add(time.Nanosecond), end, true, 50 * time.Millisecond},
		{end, end.Add(time.Nanosecond), true, 50*time.Millisecond + 1},
		{end.Add(time.Nanosecond), end.Add(time.Second), false, time.Second},
	} {
		ok := w.includes(loadTimestamp(tc.planned))
		if ok != tc.accepted {
			t.Fatalf("planned %s, accepted %v", tc.planned, ok)
		}
		if ok {
			load.recordDelivery(w, loadTimestamp(tc.decoded.Add(-tc.lag)), tc.decoded)
		}
	}
	r := load.deliveries.result()
	if stock := load.committed.result(); stock.Samples != r.All.Samples || stock.Over50MS != r.All.Over50MS {
		t.Fatalf("stock vs wide: %+v vs %+v", stock, r.All)
	}
	if r.ThroughWindowEnd.Samples != 1 || r.ThroughWindowEnd.Over50MS != 0 || r.ThroughWindowEnd.P99LEMS != 50 || r.AfterWindowEnd.Samples != 1 || r.AfterWindowEnd.Over50MS != 1 || r.AfterWindowEnd.P99LEMS != 60 || r.All.Samples != 2 || r.All.Over50MS != 1 || r.All.P99LEMS != 60 {
		t.Fatalf("bad split: %+v", r)
	}
	var long syncDeliveryHist
	long.add(false, 60*time.Second+1)
	e := long.result()
	if e.All.Samples != 1 || e.All.Over60S != 1 || e.All.P99LEMS != -1 || e.AfterWindowEnd.Samples != 0 || e.AfterWindowEnd.P99LEMS != -1 {
		t.Fatalf("overflow/empty: %+v", e)
	}
}
