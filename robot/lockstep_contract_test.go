package robot

import (
	"errors"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/lockstep"
	"slices"
	"testing"
)

type stepFailSink struct {
	mode      string
	failures  int // 还要失败几次
	inputs    []lockstep.FrameID
	hashes    []lockstep.FrameID
	catchups  int
	lastInput []byte
}

func (s *stepFailSink) SubmitInput(frame lockstep.FrameID, payload []byte) error {
	if s.mode == "input" && s.failures > 0 {
		s.failures--
		return errors.New("temporary send failure")
	}
	s.inputs = append(s.inputs, frame)
	s.lastInput = append([]byte(nil), payload...)
	return nil
}

func (s *stepFailSink) ReportHash(frame lockstep.FrameID, _ uint64) error {
	if s.mode == "hash" && s.failures > 0 {
		s.failures--
		return errors.New("temporary report failure")
	}
	s.hashes = append(s.hashes, frame)
	return nil
}

func (s *stepFailSink) RequestCatchup(lockstep.FrameID) error { s.catchups++; return nil }

func newStepBot(t *testing.T, sink *stepFailSink, simulate func(lockstep.Frame) error) *LockstepBot {
	t.Helper()
	bot, err := NewLockstepBot(LockstepBotConfig{
		Player: 1, Sink: sink, KeyframeInterval: 1,
		Input:    func(lockstep.FrameID) []byte { return []byte{1} },
		Simulate: simulate,
	})
	if err != nil {
		t.Fatal(err)
	}
	return bot
}

func TestLockstepBotPromiseTransientSinkFailureKeepsTheBatchTail(t *testing.T) {
	for _, mode := range []string{"input", "hash"} {
		t.Run(mode, func(t *testing.T) {
			sink := &stepFailSink{mode: mode, failures: 1}
			var applied []lockstep.FrameID
			bot := newStepBot(t, sink, func(f lockstep.Frame) error {
				applied = append(applied, f.ID)
				return nil
			})
			page := lockstep.EncodeBroadcast([]lockstep.Frame{{ID: 1}, {ID: 2}})
			if err := bot.HandleBroadcast(page); err == nil {
				t.Fatal("injection did not fire")
			}
			// 重传原包(被 Assembler 去重)必须把保留下来的那一步接着做完。
			if err := bot.HandleBroadcast(page); err != nil {
				t.Fatalf("resume after a transient sink failure: %v", err)
			}
			if err := bot.HandleFrames([]lockstep.Frame{{ID: 3}}); err != nil {
				t.Fatal(err)
			}
			want := []lockstep.FrameID{1, 2, 3}
			if len(applied) != len(want) || applied[0] != 1 || applied[1] != 2 || applied[2] != 3 {
				t.Fatalf("continued successfully after failure but simulation skipped frames: applied=%v next=%d stats=%+v",
					applied, bot.Next(), bot.Stats())
			}
			// 出站也要补齐:三帧各一次输入(frame+1)与一次 hash,没有重复。
			if got := len(sink.inputs); got != 3 {
				t.Fatalf("inputs=%v want one per frame", sink.inputs)
			}
			if got := len(sink.hashes); got != 3 {
				t.Fatalf("hashes=%v want one per frame", sink.hashes)
			}
			if stats := bot.Stats(); stats.FramesApplied != 3 {
				t.Fatalf("FramesApplied=%d want 3 (no frame simulated twice)", stats.FramesApplied)
			}
		})
	}
}

func TestLockstepBotPromiseSimulateFailureIsTerminal(t *testing.T) {
	sink := &stepFailSink{}
	var applied []lockstep.FrameID
	failed := false
	bot := newStepBot(t, sink, func(f lockstep.Frame) error {
		if !failed {
			failed = true
			return errors.New("simulation rejected frame")
		}
		applied = append(applied, f.ID)
		return nil
	})
	page := lockstep.EncodeBroadcast([]lockstep.Frame{{ID: 1}, {ID: 2}})
	err := bot.HandleBroadcast(page)
	if err == nil {
		t.Fatal("injection did not fire")
	}
	if !errors.Is(err, ErrLockstepBotTerminal) {
		t.Fatalf("a rejected frame must be terminal: %v", err)
	}
	if err := bot.HandleBroadcast(page); !errors.Is(err, ErrLockstepBotTerminal) {
		t.Fatalf("a terminal bot kept accepting broadcasts: %v (applied=%v)", err, applied)
	}
	if err := bot.HandleFrames([]lockstep.Frame{{ID: 3}}); !errors.Is(err, ErrLockstepBotTerminal) {
		t.Fatalf("a terminal bot kept accepting frames: %v (applied=%v)", err, applied)
	}
	if len(applied) != 0 {
		t.Fatalf("a terminal bot kept simulating: applied=%v", applied)
	}
	if bot.Terminal() == nil {
		t.Fatal("Terminal() must report why the bot stopped")
	}
}

// 保留的工作要有上界:sink 一直失败时进入 terminal,而不是让待应用队列无限增长。
func TestLockstepBotPromisePendingApplyIsBounded(t *testing.T) {
	sink := &stepFailSink{mode: "input", failures: 1 << 30}
	bot, err := NewLockstepBot(LockstepBotConfig{
		Player: 1, Sink: sink, KeyframeInterval: 1, MaxPendingApply: 8,
		Input:    func(lockstep.FrameID) []byte { return []byte{1} },
		Simulate: func(lockstep.Frame) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	var last error
	for id := lockstep.FrameID(1); id <= 64; id++ {
		last = bot.HandleFrames([]lockstep.Frame{{ID: id}})
		if errors.Is(last, ErrLockstepBotTerminal) {
			return
		}
	}
	t.Fatalf("a permanently failing sink never turned the bot terminal: last=%v next=%d", last, bot.Next())
}

// HandleFrames 收的是调用方的帧,payload 是调用方的内存;保留到下次调用时必须是自己的副本。
func TestLockstepBotPromiseRetainedFramesDoNotAliasCallerPayloads(t *testing.T) {
	sink := &stepFailSink{mode: "input", failures: 1}
	var seen []byte
	bot := newStepBot(t, sink, func(f lockstep.Frame) error {
		if f.ID == 2 && len(f.Inputs) == 1 {
			seen = append([]byte(nil), f.Inputs[0].Payload...)
		}
		return nil
	})
	payload := []byte{7}
	if err := bot.HandleFrames([]lockstep.Frame{
		{ID: 1},
		{ID: 2, Inputs: []lockstep.Input{{Player: 2, Payload: payload}}},
	}); err == nil {
		t.Fatal("injection did not fire")
	}
	payload[0] = 99 // 调用方复用了自己的缓冲区
	if err := bot.HandleFrames(nil); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(seen) != 1 || seen[0] != 7 {
		t.Fatalf("a retained frame aliased the caller's payload: simulate saw %v, want [7]", seen)
	}
}

type gapSink struct{ catchups []lockstep.FrameID }

func (s *gapSink) SubmitInput(lockstep.FrameID, []byte) error { return nil }
func (s *gapSink) ReportHash(lockstep.FrameID, uint64) error  { return nil }
func (s *gapSink) RequestCatchup(from lockstep.FrameID) error {
	s.catchups = append(s.catchups, from)
	return nil
}

// broadcastPackets encodes frames 1..count the way the Room does: each
// packet carries its frame plus depth-1 predecessors.
func broadcastPackets(count int, depth int) [][]byte {
	encoder := lockstep.NewRedundantEncoder(depth)
	packets := make([][]byte, 0, count)
	for id := 1; id <= count; id++ {
		packets = append(packets, encoder.Push(lockstep.Frame{ID: lockstep.FrameID(id)}))
	}
	return packets
}

func TestLockstepBotPromiseRequestsCatchupOnAnyUnhealedGap(t *testing.T) {
	sink := &gapSink{}
	bot, err := NewLockstepBot(LockstepBotConfig{Player: 1, Sink: sink, KeyframeInterval: 1000})
	if err != nil {
		t.Fatal(err)
	}
	packets := broadcastPackets(40, 3)
	for index, packet := range packets {
		id := index + 1
		if id >= 4 && id <= 8 { // 5 lost packets: frames 4..6 are beyond redundancy
			continue
		}
		if err := bot.HandleBroadcast(packet); err != nil {
			t.Fatal(err)
		}
	}
	if bot.Next() != 4 {
		t.Fatalf("Next = %d, want 4 (frames 4..6 lost)", bot.Next())
	}
	if !slices.Equal(sink.catchups, []lockstep.FrameID{4}) {
		t.Fatalf("catch-up requests = %v, want exactly [4] once the gap is seen", sink.catchups)
	}
	// The server serves the gap: the bot runs to the head.
	if err := bot.HandleFrames([]lockstep.Frame{{ID: 4}, {ID: 5}, {ID: 6}}); err != nil {
		t.Fatal(err)
	}
	if bot.Next() != 41 {
		t.Fatalf("Next after catch-up = %d, want 41", bot.Next())
	}
}

func TestLockstepBotPromiseReRequestsAnAbandonedCatchup(t *testing.T) {
	sink := &gapSink{}
	bot, err := NewLockstepBot(LockstepBotConfig{Player: 1, Sink: sink, KeyframeInterval: 1000})
	if err != nil {
		t.Fatal(err)
	}
	packets := broadcastPackets(200, 3)
	for index, packet := range packets {
		id := index + 1
		if id >= 4 && id <= 8 {
			continue
		}
		if err := bot.HandleBroadcast(packet); err != nil {
			t.Fatal(err)
		}
	}
	// The server never answered (abandoned the catch-up): ~190 packets later
	// the gap is still open and the bot must have asked again.
	if len(sink.catchups) < 2 {
		t.Fatalf("catch-up requests = %v after ~190 packets with the gap still open; an abandoned catch-up is never retried", sink.catchups)
	}
	for _, from := range sink.catchups {
		if from != 4 {
			t.Fatalf("catch-up requested from %d, want 4", from)
		}
	}
}
