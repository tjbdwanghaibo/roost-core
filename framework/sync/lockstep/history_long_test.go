package lockstep

import (
	"context"
	"errors"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/nettransport"
	"math"
	"testing"
)

// 完整 Tick 链推进一小时逻辑帧，再在持续推进和裁剪中从保留窗口追到直播。
func TestMatchLongRunCatchupAfterRollingTrim(t *testing.T) {
	players := []PlayerID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	m, err := NewMatch(MatchConfig{Sequencer: SequencerConfig{Players: players, MaxInputBytes: 8}, Datagrams: nullTransport{}, Reliable: nullTransport{}, CatchupBatchFrames: 64, CatchupSendWait: testSendWait})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for _, player := range players {
		if err = m.Attach(player, nettransport.SessionID(player)); err != nil {
			t.Fatal(err)
		}
	}
	const window = 1800
	peak := 0
	for range 108000 {
		next := m.NextFrame()
		payload := []byte{byte(next), byte(next >> 8), byte(next >> 16), 0, 1, 2, 3, 4}
		for _, player := range players {
			if _, err = m.SubmitInput(player, next, payload); err != nil {
				t.Fatal(err)
			}
		}
		f, err := m.Tick(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if f.ID > window {
			m.TrimHistory(f.ID - window + 1)
		}
		if f.ID%30 == 0 {
			for _, player := range players {
				if err = m.ReportHash(player, f.ID, uint64(f.ID)); err != nil {
					t.Fatal(err)
				}
			}
			if f.ID > window {
				m.TrimHashReports(f.ID - window + 1)
			}
			if len(m.detector.reports) > window/30 || len(m.ruled) > window/30 {
				t.Fatal("hash reports grew beyond retention window")
			}
		}
		peak = max(peak, m.history.retainedCapacity)
	}
	if m.history.Len() != window || peak > 4*window {
		t.Fatal("long match history not bounded")
	}
	transport := newRecordingTransport()
	m.datagrams = transport
	m.reliable = transport
	if err = m.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	first := m.History().FirstID()
	if err = m.StartCatchup(1, first); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100 && m.CatchingUp(1); i++ {
		f, err := m.Tick(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		m.TrimHistory(f.ID - window + 1)
	}
	if m.CatchingUp(1) {
		t.Fatal("catchup failed to reach head")
	}
	if _, err = m.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	received := decodeAll(t, transport.reliable[101])
	for id, f := range decodeAll(t, transport.datagrams[101]) {
		received[id] = f
	}
	for id := first; id <= m.History().Latest(); id++ {
		f, ok := received[id]
		if !ok {
			t.Fatalf("catchup gap %d", id)
		}
		if id <= 108000 {
			if len(f.Inputs) != 10 {
				t.Fatal("lost long-run inputs")
			}
			for _, input := range f.Inputs {
				if len(input.Payload) != 8 || input.Payload[0] != byte(id) || input.Payload[1] != byte(id>>8) {
					t.Fatal("corrupt retained payload")
				}
			}
		}
	}
	if err = m.StartCatchup(1, first-1); !errors.Is(err, ErrCatchupUnservable) {
		t.Fatalf("expired history must fail at the request: %v", err)
	}
	if _, err = m.Tick(context.Background()); !errors.Is(err, ErrCatchupUnservable) {
		t.Fatalf("expired history must fail explicitly: %v", err)
	}
	t.Logf("108000 ticks, 10 players, rolling window %d, peak capacity %d, hash report frames %d, catchup pages %d", window, peak, len(m.detector.reports), len(transport.reliable[101]))
}

func TestHistoryExtremeRangeAndTrim(t *testing.T) {
	t.Run("limit", func(t *testing.T) {
		h := NewHistory()
		h.Append(Frame{ID: 1})
		h.Append(Frame{ID: 2})
		if page := h.ReadRange(2, math.MaxInt); len(page) != 1 || page[0].ID != 2 {
			t.Fatal(page)
		}
	})
	t.Run("frame-id", func(t *testing.T) {
		h := NewHistory()
		h.Append(Frame{ID: FrameID(math.MaxUint32 - 1)})
		h.Append(Frame{ID: FrameID(math.MaxUint32)})
		h.TrimBefore(FrameID(math.MaxUint32))
		if h.Len() != 1 || h.FirstID() != FrameID(math.MaxUint32) {
			t.Fatal("trim near maximum frame ID failed")
		}
	})
}

// 每轮追加一帧、裁掉一帧并读取追帧页，模拟稳定保留窗口。
func BenchmarkHistoryRollingWindow(b *testing.B) {
	for _, window := range []int{300, 18000} {
		b.Run(fmt.Sprint(window), func(b *testing.B) {
			h := NewHistory()
			for id := FrameID(1); id <= FrameID(window); id++ {
				h.Append(Frame{ID: id})
			}
			id := FrameID(window)
			b.ReportAllocs()
			for b.Loop() {
				id++
				h.Append(Frame{ID: id})
				h.TrimBefore(id - FrameID(window) + 1)
				page := h.ReadRange(h.FirstID(), 32)
				if len(page) != 32 || page[0].ID != h.FirstID() || h.Len() != window {
					b.Fatal("history changed")
				}
			}
		})
	}
}

func TestHistoryLongRollingWindow(t *testing.T) {
	const window = 1800 // 30Hz 下保留一分钟；推进一小时逻辑帧，不等待墙钟一小时。
	h := NewHistory()
	peak := 0
	for id := FrameID(1); id <= 108000; id++ {
		h.Append(Frame{ID: id, Inputs: []Input{{Player: 1, Payload: []byte{byte(id), byte(id >> 8)}}}})
		if id > window {
			h.TrimBefore(id - window + 1)
		}
		peak = max(peak, h.retainedCapacity)
		if h.Len() != min(int(id), window) || h.Latest() != id {
			t.Fatalf("frame %d range mismatch", id)
		}
		if id%97 == 0 {
			page := h.ReadRange(h.FirstID(), math.MaxInt)
			for i, f := range page {
				if f.ID != h.FirstID()+FrameID(i) || len(f.Inputs) != 1 || f.Inputs[0].Payload[0] != byte(f.ID) || f.Inputs[0].Payload[1] != byte(f.ID>>8) {
					t.Fatalf("corrupt page at %d", id)
				}
			}
		}
	}
	if peak > 4*window {
		t.Fatalf("history capacity unbounded: %d", peak)
	}
	dropped := h.frames[:h.Len()-1]
	h.TrimBefore(h.Latest())
	for _, f := range dropped {
		if f.Inputs != nil {
			t.Fatal("trim retained payload reference")
		}
	}
	if h.retainedCapacity > 256 {
		t.Fatal("large trim retained oversized array")
	}
	h.TrimBefore(h.Latest() + 1)
	if h.frames != nil || h.FirstID() != 0 || h.Latest() != 0 {
		t.Fatal("empty history retained storage")
	}
	t.Logf("108000 frames, retained window=%d, peak frame capacity=%d", window, peak)
}

func TestHistoryGradualShrinkReleasesBackingArray(t *testing.T) {
	h := NewHistory()
	for id := FrameID(1); id <= 10000; id++ {
		h.Append(Frame{ID: id})
	}
	// 多次小幅裁剪也必须收缩整块数组，不能因 cap 随切片起点变小而漏判。
	for keep := FrameID(2); keep < 10000; keep++ {
		h.TrimBefore(keep)
	}
	if h.Len() != 2 || h.retainedCapacity > 256 {
		t.Fatalf("small tail retained backing allocation: len=%d retained_capacity=%d", h.Len(), h.retainedCapacity)
	}
	h.TrimBefore(10001)
	if h.retainedCapacity != 0 {
		t.Fatal("empty history retained backing allocation")
	}
}

// 默认自动保留：长局里历史与 hash 报告都停在保留窗口，不随对局长度增长；宿主不调用任何裁剪。
func TestMatchDefaultRetentionBoundsLongMatch(t *testing.T) {
	m, err := NewMatch(MatchConfig{Sequencer: SequencerConfig{Players: []PlayerID{1, 2}, MaxInputBytes: 8}, Datagrams: nullTransport{}, Reliable: nullTransport{}, CatchupSendWait: testSendWait})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	const ticks = 3*DefaultHistoryRetainFrames + 100
	peak := 0
	for range ticks {
		f, err := m.Tick(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if f.ID%30 == 0 {
			for player := PlayerID(1); player <= 2; player++ {
				if err := m.ReportHash(player, f.ID, uint64(f.ID)); err != nil {
					t.Fatal(err)
				}
			}
		}
		peak = max(peak, m.history.retainedCapacity)
	}
	if m.History().Len() != DefaultHistoryRetainFrames || m.History().FirstID() != ticks-DefaultHistoryRetainFrames+1 {
		t.Fatalf("history len %d first %d after %d ticks, want the last %d frames", m.History().Len(), m.History().FirstID(), ticks, DefaultHistoryRetainFrames)
	}
	if peak > 4*DefaultHistoryRetainFrames {
		t.Fatalf("history backing array peaked at %d slots", peak)
	}
	limit := (DefaultHistoryRetainFrames+int(hashTrimStride))/30 + 1
	if len(m.detector.reports) > limit || len(m.ruled) > limit {
		t.Fatalf("hash reports %d (ruled %d) beyond the retention window bound %d", len(m.detector.reports), len(m.ruled), limit)
	}
}

// 追帧途中游标被自动裁剪越过（页面一直发不出去）：本次 Tick 结束追帧、返回 ErrCatchupUnservable，
// 客户端只收到失败通知；会话随后回到实时广播。
func TestMatchAutoRetentionOvertakesStalledCatchup(t *testing.T) {
	transport := newRecordingTransport()
	m, err := NewMatch(MatchConfig{Sequencer: SequencerConfig{Players: []PlayerID{1}, MaxInputBytes: 8}, Datagrams: transport, Reliable: transport,
		HistoryRetainFrames: MinHistoryRetainFrames, CatchupBatchFrames: 8, CatchupMaxFailures: 1000, CatchupSendWait: testSendWait})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for range 2 * MinHistoryRetainFrames {
		if _, err := m.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	from := m.History().FirstID()
	if err := m.StartCatchup(1, from); err != nil {
		t.Fatal(err)
	}
	transport.failing[101] = errors.New("link down")
	if _, err := m.Tick(ctx); errors.Is(err, ErrCatchupUnservable) {
		t.Fatalf("catch-up abandoned before the window passed its cursor: %v", err)
	}
	delete(transport.failing, 101)
	start := m.History().FirstID() // 失败那次 Tick 末尾已把窗口推过游标
	if start <= from {
		t.Fatalf("window start %d did not pass cursor %d", start, from)
	}
	_, err = m.Tick(ctx)
	var unservable *CatchupUnservableError
	if !errors.As(err, &unservable) || unservable.From != from || unservable.Start != start {
		t.Fatalf("overtaken catch-up: %v", err)
	}
	if m.CatchingUp(1) || len(transport.reliable[101]) != 1 || !IsCatchupNotice(transport.reliable[101][0]) {
		t.Fatalf("catching up %v, reliable packets %d", m.CatchingUp(1), len(transport.reliable[101]))
	}
	before := len(transport.datagrams[101])
	if _, err := m.Tick(ctx); err != nil || len(transport.datagrams[101]) != before+1 {
		t.Fatalf("session did not return to live broadcasts: %v", err)
	}
}

// 显式关闭自动裁剪：历史与 hash 报告保留整局，从帧 1 追帧仍能补齐（关闭前的行为）。
func TestMatchRetainFullHistoryKeepsEveryFrame(t *testing.T) {
	transport := newRecordingTransport()
	m, err := NewMatch(MatchConfig{Sequencer: SequencerConfig{Players: []PlayerID{1}, MaxInputBytes: 8}, Datagrams: transport, Reliable: transport,
		HistoryRetainFrames: RetainFullHistory, CatchupBatchFrames: 64, CatchupSendWait: testSendWait})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx := context.Background()
	const ticks = DefaultHistoryRetainFrames + 500
	for range ticks {
		f, err := m.Tick(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.ReportHash(1, f.ID, 1); err != nil {
			t.Fatal(err)
		}
	}
	if m.History().Len() != ticks || m.History().FirstID() != 1 || len(m.detector.reports) != ticks {
		t.Fatalf("history %d first %d reports %d, want the whole match", m.History().Len(), m.History().FirstID(), len(m.detector.reports))
	}
	if err := m.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	if err := m.StartCatchup(1, 1); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200 && m.CatchingUp(1); i++ {
		if _, err := m.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	received := decodeAll(t, transport.reliable[101])
	for id, f := range decodeAll(t, transport.datagrams[101]) {
		received[id] = f
	}
	for id := FrameID(1); id <= m.History().Latest(); id++ {
		if _, ok := received[id]; !ok {
			t.Fatalf("full-history catch-up gap at %d", id)
		}
	}
}

func TestMatchHistoryRetainFramesValidation(t *testing.T) {
	for _, tc := range []struct {
		configured int
		valid      bool
		want       int
	}{
		{0, true, DefaultHistoryRetainFrames},
		{RetainFullHistory, true, 0},
		{MinHistoryRetainFrames, true, MinHistoryRetainFrames},
		{MinHistoryRetainFrames - 1, false, 0},
		{1, false, 0},
		{-2, false, 0},
	} {
		m, err := NewMatch(MatchConfig{Sequencer: SequencerConfig{Players: []PlayerID{1}, MaxInputBytes: 8}, Datagrams: nullTransport{}, HistoryRetainFrames: tc.configured})
		if tc.valid != (err == nil) || err != nil && !errors.Is(err, ErrMatchConfigInvalid) {
			t.Fatalf("HistoryRetainFrames %d: %v", tc.configured, err)
		}
		if err == nil && m.retain != tc.want {
			t.Fatalf("HistoryRetainFrames %d resolved to %d, want %d", tc.configured, m.retain, tc.want)
		}
	}
}
