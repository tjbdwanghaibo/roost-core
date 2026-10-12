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
	if err = m.StartCatchup(1, first-1); err != nil {
		t.Fatal(err)
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
