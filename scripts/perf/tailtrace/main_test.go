package main

import "testing"

// 夹具 tailtrace_test.go 的 trace.Log 消息格式与这里的解析、分段顺序是一份约定，两边一起改。
func TestParseTailLogAndSegmentsFollowFixtureOrder(t *testing.T) {
	ev := parseTailLog("stream=business total_us=80000 admit_us=5000 queue_us=60000 handler_us=10000 release_us=5000")
	if ev.Stream != "business" || ev.TotalMS != 80 || ev.Parts["queue"] != 60000 {
		t.Fatalf("parsed=%+v", ev)
	}
	ev.end = 1_000_000_000
	ev.start = ev.end - 80_000_000
	segs := buildSegments(ev)
	want := []struct {
		name       string
		start, end int64
	}{{"window", 920_000_000, 1_000_000_000}, {"admit", 920_000_000, 925_000_000}, {"queue", 925_000_000, 985_000_000}, {"handler", 985_000_000, 995_000_000}, {"release", 995_000_000, 1_000_000_000}}
	if len(segs) != len(want) {
		t.Fatalf("segments=%+v", segs)
	}
	for i, w := range want {
		if segs[i].Name != w.name || segs[i].Start != w.start || segs[i].End != w.end {
			t.Fatalf("segment %d=%+v want %+v", i, segs[i], w)
		}
	}
	segs[2].Running = 50
	ev.Segments = segs
	if got := classify(ev); got != "queue:worker-busy(head-of-line)" {
		t.Fatalf("label=%q", got)
	}
}
