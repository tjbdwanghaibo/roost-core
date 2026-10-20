package robot

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/lockstep"
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

func TestLockstepBotTransientSinkFailureKeepsTheBatchTail(t *testing.T) {
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

func TestLockstepBotSimulateFailureIsTerminal(t *testing.T) {
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
func TestLockstepBotPendingApplyIsBounded(t *testing.T) {
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
func TestLockstepBotRetainedFramesDoNotAliasCallerPayloads(t *testing.T) {
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

// broadcastPackets encodes frames 1..count the way the Match does: each
// packet carries its frame plus depth-1 predecessors.
func broadcastPackets(count int, depth int) [][]byte {
	encoder := lockstep.NewRedundantEncoder(depth)
	packets := make([][]byte, 0, count)
	for id := 1; id <= count; id++ {
		packets = append(packets, encoder.Push(lockstep.Frame{ID: lockstep.FrameID(id)}))
	}
	return packets
}

func TestLockstepBotRequestsCatchupOnAnyUnhealedGap(t *testing.T) {
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

func TestLockstepBotReRequestsAnAbandonedCatchup(t *testing.T) {
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

// 服务端的追帧失败通知让 bot 终止：缺的帧已不在历史里，不能再请求追帧、也不能跳过缺口继续模拟。
func TestLockstepBotCatchupUnservableNoticeIsTerminal(t *testing.T) {
	sink := &gapSink{}
	bot, err := NewLockstepBot(LockstepBotConfig{Player: 1, Sink: sink})
	if err != nil {
		t.Fatal(err)
	}
	if err := bot.HandleBroadcast(lockstep.EncodeBroadcast([]lockstep.Frame{{ID: 5}})); err != nil {
		t.Fatal(err)
	}
	if len(sink.catchups) != 1 || sink.catchups[0] != 1 {
		t.Fatalf("gap did not request a catch-up: %v", sink.catchups)
	}
	err = bot.HandleBroadcast(lockstep.EncodeCatchupUnservable(1, 4))
	var unservable *lockstep.CatchupUnservableError
	if !errors.Is(err, ErrLockstepBotTerminal) || !errors.As(err, &unservable) || unservable.From != 1 || unservable.Start != 4 {
		t.Fatalf("catch-up-failed notice: %v", err)
	}
	if err := bot.HandleBroadcast(lockstep.EncodeBroadcast([]lockstep.Frame{{ID: 6}})); !errors.Is(err, lockstep.ErrCatchupUnservable) {
		t.Fatalf("terminal bot kept consuming: %v", err)
	}
	if len(sink.catchups) != 1 || bot.Stats().FramesApplied != 0 {
		t.Fatalf("terminal bot asked again or simulated: catchups=%v stats=%+v", sink.catchups, bot.Stats())
	}
}

// 进程内 sink 直接把 Match.StartCatchup 的错误交回来时同样终止。
type unservableSink struct{ gapSink }

func (s *unservableSink) RequestCatchup(from lockstep.FrameID) error {
	return &lockstep.CatchupUnservableError{From: from, Start: from + 10}
}

func TestLockstepBotUnservableCatchupRequestIsTerminal(t *testing.T) {
	bot, err := NewLockstepBot(LockstepBotConfig{Player: 1, Sink: &unservableSink{}})
	if err != nil {
		t.Fatal(err)
	}
	err = bot.HandleBroadcast(lockstep.EncodeBroadcast([]lockstep.Frame{{ID: 5}}))
	if !errors.Is(err, ErrLockstepBotTerminal) || !errors.Is(err, lockstep.ErrCatchupUnservable) || bot.Terminal() == nil {
		t.Fatalf("unservable catch-up request: %v", err)
	}
}

// ---- 快照恢复（设计 §7.2 / §7.3） ----

// snapshotPackets 把 data 编成一次快照传输的 CA 分块（每块 chunk 字节）。
func snapshotPackets(t *testing.T, transfer uint64, frame lockstep.FrameID, data []byte, chunk int, hash *uint64) [][]byte {
	t.Helper()
	var packets [][]byte
	for offset := 0; offset < len(data); offset += chunk {
		piece := lockstep.SnapshotChunk{Transfer: transfer, Offset: offset, Data: data[offset:min(offset+chunk, len(data))]}
		if offset == 0 {
			piece.Frame, piece.Total, piece.Digest = frame, len(data), sha256.Sum256(data)
			if hash != nil {
				piece.StateHash, piece.HasStateHash = *hash, true
			}
		}
		packet, err := lockstep.EncodeSnapshotChunk(piece)
		if err != nil {
			t.Fatal(err)
		}
		packets = append(packets, packet)
	}
	return packets
}

// inputFrame 是一帧两席输入（载荷 = 帧号低字节与席位）。
func inputFrame(id lockstep.FrameID) lockstep.Frame {
	return lockstep.Frame{ID: id, Inputs: []lockstep.Input{{Player: 1, Payload: []byte{byte(id), 1}}, {Player: 2, Payload: []byte{byte(id), 2}}}}
}

// sumSim 是测试里的确定性小模拟：状态是一个折叠了全部输入的整数，状态 hash 就是它。
type sumSim struct{ total uint64 }

func (s *sumSim) apply(frame lockstep.Frame) error {
	for _, input := range frame.Inputs {
		s.total = s.total*31 + uint64(input.Player)*7 + uint64(input.Payload[0])
	}
	return nil
}

func (s *sumSim) encode() []byte { return binary.LittleEndian.AppendUint64(nil, s.total) }

// referenceSim 在“服务器”一侧依次应用帧 1..through。
func referenceSim(through lockstep.FrameID) *sumSim {
	sim := &sumSim{}
	for id := lockstep.FrameID(1); id <= through; id++ {
		_ = sim.apply(inputFrame(id))
	}
	return sim
}

type hashSink struct {
	gapSink
	hashes map[lockstep.FrameID]uint64
}

func (s *hashSink) ReportHash(frame lockstep.FrameID, hash uint64) error {
	if s.hashes == nil {
		s.hashes = make(map[lockstep.FrameID]uint64)
	}
	s.hashes[frame] = hash
	return nil
}

// 带业务 Restore 的 bot：断线后收到越界快照，恢复状态、核对 hash、从快照帧 +1 续帧；关键帧 hash 与
// 一直在线、从帧 1 应用到底的参考模拟一致。
func TestLockstepBotRestoresFromASnapshotAndContinues(t *testing.T) {
	sim := &sumSim{}
	sink := &hashSink{}
	bot, err := NewLockstepBot(LockstepBotConfig{
		Player: 1, Sink: sink, KeyframeInterval: 10,
		Simulate: sim.apply,
		Hash:     func(lockstep.FrameID) uint64 { return sim.total },
		Restore: func(snapshot lockstep.ReceivedSnapshot) (uint64, error) {
			sim.total = binary.LittleEndian.Uint64(snapshot.Data)
			return sim.total, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for id := lockstep.FrameID(1); id <= 5; id++ {
		if err := bot.HandleBroadcast(lockstep.EncodeBroadcast([]lockstep.Frame{inputFrame(id)})); err != nil {
			t.Fatal(err)
		}
	}
	// 重连后先到的实时帧远在缺口之后：请求追帧，服务端判定越界、改发快照。
	if err := bot.HandleBroadcast(lockstep.EncodeBroadcast([]lockstep.Frame{inputFrame(300)})); err != nil {
		t.Fatal(err)
	}
	if len(sink.catchups) != 1 || sink.catchups[0] != 6 {
		t.Fatalf("catch-up requests %v, want [6]", sink.catchups)
	}
	reference := referenceSim(280)
	want := reference.total
	data := append(reference.encode(), bytes.Repeat([]byte{0xAB}, 3000)...) // 多块
	for _, packet := range snapshotPackets(t, 1, 280, data, 1024, &want) {
		if err := bot.HandleBroadcast(packet); err != nil {
			t.Fatal(err)
		}
	}
	if bot.Next() != 281 || bot.Stats().SnapshotsRestored != 1 || sim.total != want || bot.Terminal() != nil {
		t.Fatalf("after the snapshot: next %d, stats %+v, state %d want %d, terminal %v", bot.Next(), bot.Stats(), sim.total, want, bot.Terminal())
	}
	page := make([]lockstep.Frame, 0, 20)
	for id := lockstep.FrameID(281); id <= 300; id++ {
		page = append(page, inputFrame(id))
	}
	if err := bot.HandleBroadcast(lockstep.EncodeBroadcast(page)); err != nil {
		t.Fatal(err)
	}
	if bot.Next() != 301 || len(sink.catchups) != 1 {
		t.Fatalf("after the page: next %d, catch-up requests %v", bot.Next(), sink.catchups)
	}
	for _, keyframe := range []lockstep.FrameID{290, 300} {
		if got, want := sink.hashes[keyframe], referenceSim(keyframe).total; got != want {
			t.Fatalf("keyframe %d hash %d, reference %d", keyframe, got, want)
		}
	}
}

// 纯线协议 bot（没设 Simulate / Hash / Restore）：状态就是输入链 hash，快照携带的 StateHash 就是新链值；
// 续帧后的关键帧 hash 与从帧 1 折叠到底的 FrameHasher 一致。
// 修前（dd76c112）同一场景：bot 把分块当坏包请求追帧后停在缺口，Next = 1。
func TestLockstepBotWithoutRestoreContinuesTheInputChain(t *testing.T) {
	sink := &hashSink{}
	bot, err := NewLockstepBot(LockstepBotConfig{Player: 1, Sink: sink, KeyframeInterval: 5})
	if err != nil {
		t.Fatal(err)
	}
	reference := NewFrameHasher()
	for id := lockstep.FrameID(1); id <= 200; id++ {
		reference.Fold(inputFrame(id))
	}
	chain := reference.Sum()
	for _, packet := range snapshotPackets(t, 1, 200, []byte("wire-only bots carry no state"), 10, &chain) {
		if err := bot.HandleBroadcast(packet); err != nil {
			t.Fatal(err)
		}
	}
	for id := lockstep.FrameID(201); id <= 205; id++ {
		reference.Fold(inputFrame(id))
		if err := bot.HandleBroadcast(lockstep.EncodeBroadcast([]lockstep.Frame{inputFrame(id)})); err != nil {
			t.Fatal(err)
		}
	}
	if bot.Next() != 206 || bot.Terminal() != nil || bot.Stats().SnapshotsRestored != 1 {
		t.Fatalf("after a snapshot at frame 200 and pages 201..205: next %d, terminal %v, stats %+v", bot.Next(), bot.Terminal(), bot.Stats())
	}
	if sink.hashes[205] != reference.Sum() {
		t.Fatalf("keyframe 205 hash %d, reference chain %d", sink.hashes[205], reference.Sum())
	}
	// 没有 StateHash 的快照，纯线协议 bot 无从续链：不支持、终止。
	bot, err = NewLockstepBot(LockstepBotConfig{Player: 1, Sink: &hashSink{}})
	if err != nil {
		t.Fatal(err)
	}
	var last error
	for _, packet := range snapshotPackets(t, 1, 200, []byte("no state hash"), 64, nil) {
		last = bot.HandleBroadcast(packet)
	}
	if !errors.Is(last, ErrLockstepBotTerminal) || !errors.Is(last, lockstep.ErrSnapshotUnsupported) {
		t.Fatalf("wire-only snapshot without a state hash: %v", last)
	}
}

// 校验失败一律终止、不重试（§7.3）：hash 不符、Restore 出错、没有恢复回调、摘要不符、坏分块。
func TestLockstepBotSnapshotFailuresAreTerminal(t *testing.T) {
	want := uint64(42)
	good := snapshotPackets(t, 1, 50, []byte("state at fifty"), 64, &want)
	badDigest := snapshotPackets(t, 2, 50, []byte("state at fifty"), 64, &want)
	badDigest[0] = append([]byte(nil), badDigest[0]...)
	badDigest[0][len(badDigest[0])-1] ^= 1
	restoreReturns := func(hash uint64, err error) func(lockstep.ReceivedSnapshot) (uint64, error) {
		return func(lockstep.ReceivedSnapshot) (uint64, error) { return hash, err }
	}
	cases := []struct {
		name    string
		config  LockstepBotConfig
		packets [][]byte
		want    error
		reports int
	}{
		{"hash mismatch", LockstepBotConfig{Restore: restoreReturns(41, nil)}, good, lockstep.ErrSnapshotMismatch, 0},
		{"hash mismatch reported", LockstepBotConfig{Restore: restoreReturns(41, nil), ReportSnapshotMismatch: true}, good, lockstep.ErrSnapshotMismatch, 1},
		{"restore error", LockstepBotConfig{Restore: restoreReturns(0, errors.New("bad snapshot encoding"))}, good, nil, 0},
		{"no restore with a simulation", LockstepBotConfig{Simulate: func(lockstep.Frame) error { return nil }}, good, lockstep.ErrSnapshotUnsupported, 0},
		{"digest mismatch", LockstepBotConfig{Restore: restoreReturns(42, nil)}, badDigest, lockstep.ErrSnapshotCorrupt, 0},
		{"corrupt chunk", LockstepBotConfig{Restore: restoreReturns(42, nil)}, [][]byte{good[0][:12]}, lockstep.ErrFrameCorrupt, 0},
		{"over the bot's limit", LockstepBotConfig{Restore: restoreReturns(42, nil), MaxSnapshotBytes: 8}, good, lockstep.ErrFrameCorrupt, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &hashSink{}
			tc.config.Player, tc.config.Sink = 1, sink
			bot, err := NewLockstepBot(tc.config)
			if err != nil {
				t.Fatal(err)
			}
			var last error
			for _, packet := range tc.packets {
				if last = bot.HandleBroadcast(packet); last != nil {
					break
				}
			}
			if !errors.Is(last, ErrLockstepBotTerminal) || (tc.want != nil && !errors.Is(last, tc.want)) || bot.Terminal() == nil {
				t.Fatalf("got %v, want terminal with %v", last, tc.want)
			}
			if tc.want == lockstep.ErrSnapshotMismatch {
				var mismatch *lockstep.SnapshotMismatchError
				if !errors.As(last, &mismatch) || mismatch.Frame != 50 || mismatch.Want != 42 || mismatch.Got != 41 {
					t.Fatalf("mismatch detail: %v", last)
				}
			}
			if len(sink.hashes) != tc.reports || (tc.reports == 1 && sink.hashes[50] != 41) {
				t.Fatalf("hash reports %v, want %d", sink.hashes, tc.reports)
			}
			// 终止后不再消费，也不再请求追帧。
			if err := bot.HandleBroadcast(lockstep.EncodeBroadcast([]lockstep.Frame{{ID: 51}})); !errors.Is(err, ErrLockstepBotTerminal) || len(sink.catchups) != 0 || bot.Stats().SnapshotsRestored != 0 {
				t.Fatalf("terminal bot kept going: %v, catch-ups %v, stats %+v", err, sink.catchups, bot.Stats())
			}
		})
	}
}

// 快照取代因 sink 失败而未完成的 pending 帧与 apply 游标：恢复后不再重放它们，从快照帧 +1 继续。
func TestLockstepBotSnapshotSupersedesPendingFrames(t *testing.T) {
	sink := &stepFailSink{mode: "input", failures: 1 << 20}
	var simulated []lockstep.FrameID
	bot, err := NewLockstepBot(LockstepBotConfig{
		Player: 1, Sink: sink, KeyframeInterval: 1000,
		Simulate: func(frame lockstep.Frame) error { simulated = append(simulated, frame.ID); return nil },
		Input:    func(lockstep.FrameID) []byte { return []byte{1} },
		Restore:  func(lockstep.ReceivedSnapshot) (uint64, error) { return 9, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := bot.HandleBroadcast(lockstep.EncodeBroadcast([]lockstep.Frame{{ID: 1}, {ID: 2}, {ID: 3}})); err == nil || bot.PendingApply() != 3 {
		t.Fatalf("failing sink: err %v, pending %d", err, bot.PendingApply())
	}
	hash := uint64(9)
	for _, packet := range snapshotPackets(t, 1, 10, []byte("ten"), 64, &hash) {
		if err := bot.HandleBroadcast(packet); err != nil {
			t.Fatal(err)
		}
	}
	if bot.PendingApply() != 0 || bot.Next() != 11 {
		t.Fatalf("after the snapshot: pending %d, next %d", bot.PendingApply(), bot.Next())
	}
	sink.failures = 0
	if err := bot.HandleBroadcast(lockstep.EncodeBroadcast([]lockstep.Frame{{ID: 11}})); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(simulated, []lockstep.FrameID{1, 11}) || !slices.Equal(sink.inputs, []lockstep.FrameID{12}) {
		t.Fatalf("simulated %v, inputs %v; frames 2..3 belong to the superseded state", simulated, sink.inputs)
	}
}

// 传输在推进时分块不计入追帧重试；传输停住（服务端放弃）后，被丢弃的实时包照 CatchupRetryPackets 再请求。
func TestLockstepBotRetriesAStalledSnapshotTransfer(t *testing.T) {
	sink := &gapSink{}
	bot, err := NewLockstepBot(LockstepBotConfig{Player: 1, Sink: sink, KeyframeInterval: 1000, CatchupRetryPackets: 4,
		Restore: func(lockstep.ReceivedSnapshot) (uint64, error) { return 0, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := bot.HandleBroadcast(lockstep.EncodeBroadcast([]lockstep.Frame{{ID: 100}})); err != nil {
		t.Fatal(err)
	}
	chunks := snapshotPackets(t, 1, 90, bytes.Repeat([]byte{7}, 640), 64, nil)
	// 分块与迟到的实时包交替：每个分块都是进展，重试计数清零，不再请求。
	for i, chunk := range chunks[:8] {
		if err := bot.HandleBroadcast(chunk); err != nil {
			t.Fatal(err)
		}
		for j := range 3 {
			if err := bot.HandleBroadcast(lockstep.EncodeBroadcast([]lockstep.Frame{{ID: lockstep.FrameID(101 + 3*i + j)}})); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !slices.Equal(sink.catchups, []lockstep.FrameID{1}) {
		t.Fatalf("catch-up requests while the transfer progresses: %v, want [1]", sink.catchups)
	}
	// 传输停住：之后只有实时包（全被丢弃），每 4 个包再请求一次。
	for i := range 8 {
		if err := bot.HandleBroadcast(lockstep.EncodeBroadcast([]lockstep.Frame{{ID: lockstep.FrameID(200 + i)}})); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(sink.catchups, []lockstep.FrameID{1, 1, 1}) || bot.Next() != 1 {
		t.Fatalf("catch-up requests after the transfer stalled: %v (next %d)", sink.catchups, bot.Next())
	}
}
