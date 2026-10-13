// SubmitInput：重传幂等、乱序未来帧保留、身份环准入即有界、先校验再去重。

package lockstep

import (
	"errors"
	"testing"
)

// 同一份原始输入 (player, frame) 只入帧一次：重传幂等（返回当初折入的帧号、不再入帧），同时保留“首次迟到折入下一帧”的策略；
// 身份按每玩家有界窗口保留，乱序到达的低帧号不被误丢。
// 历史：RR-20260914-04、RR-20260914-08。

func TestSubmitInputPromiseReplayAfterCutIsIdempotent(t *testing.T) {
	for _, lateFirst := range []bool{false, true} {
		name := "on_time"
		if lateFirst {
			name = "first_arrives_late"
		}
		t.Run(name, func(t *testing.T) {
			s, err := NewSequencer(SequencerConfig{Players: []PlayerID{1}})
			if err != nil {
				t.Fatal(err)
			}
			if lateFirst {
				s.Advance()
			}
			target, err := s.SubmitInput(1, 1, []byte("fire"))
			if err != nil {
				t.Fatal(err)
			}
			first := s.Advance()
			if len(first.Inputs) != 1 || first.ID != target {
				t.Fatalf("first input missing or misplaced: frame=%d target=%d inputs=%d", first.ID, target, len(first.Inputs))
			}
			again, err := s.SubmitInput(1, 1, []byte("fire"))
			if err != nil {
				return // rejecting a replay is acceptable
			}
			repeated := s.Advance()
			if len(repeated.Inputs) != 0 {
				t.Fatalf("same original input executed in frames %d and %d", first.ID, repeated.ID)
			}
			if again != target {
				t.Fatalf("replay reported frame %d, the original was folded into %d", again, target)
			}
		})
	}
}

// 乱序的未来帧不能被"最大已见帧号"误杀:先交 3 再交 2,两者都要入各自的帧。
func TestSubmitInputPromiseOutOfOrderFutureFramesAreKept(t *testing.T) {
	s, err := NewSequencer(SequencerConfig{Players: []PlayerID{1}, SubmitWindow: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitInput(1, 3, []byte("c")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitInput(1, 2, []byte("b")); err != nil {
		t.Fatal(err)
	}
	s.Advance() // frame 1, empty
	if got := s.Advance(); len(got.Inputs) != 1 || string(got.Inputs[0].Payload) != "b" {
		t.Fatalf("frame 2 lost the lower out-of-order input: %+v", got)
	}
	if got := s.Advance(); len(got.Inputs) != 1 || string(got.Inputs[0].Payload) != "c" {
		t.Fatalf("frame 3 lost its input: %+v", got)
	}
}

// 显式输入仍然覆盖迟到占位;之后迟到输入的重传不再入帧。
func TestSubmitInputPromiseExplicitInputStillOverridesFoldedPlaceholder(t *testing.T) {
	s, err := NewSequencer(SequencerConfig{Players: []PlayerID{1}})
	if err != nil {
		t.Fatal(err)
	}
	s.Advance() // frame 1 cut, empty
	if _, err := s.SubmitInput(1, 1, []byte("late")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitInput(1, 2, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitInput(1, 1, []byte("late")); err != nil { // replay of the folded one
		t.Fatal(err)
	}
	if got := s.Advance(); len(got.Inputs) != 1 || string(got.Inputs[0].Payload) != "new" {
		t.Fatalf("frame 2: %+v", got)
	}
	if got := s.Advance(); len(got.Inputs) != 0 {
		t.Fatalf("replayed late input resurfaced in frame %d", got.ID)
	}
}

// 去重身份表在准入时就有界：每个座位的身份结构是固定容量的环（ReplayHorizon + MaxSubmitWindow + 1 个槽，恰好覆盖任一时刻的
// 合法原帧号区间），Advance 不做清理遍历；超出期限的原帧号既不查也不记（仍按首次迟到折入）。

func TestSubmitInputPromiseIdentityMemoryIsBoundedBetweenTicks(t *testing.T) {
	s, err := NewSequencer(SequencerConfig{Players: []PlayerID{1}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4096; i++ {
		s.Advance()
	}
	for frame := FrameID(1); frame <= 4096; frame++ {
		if _, err := s.SubmitInput(1, frame, []byte{1}); err != nil {
			t.Fatal(err)
		}
	}
	bound := int(ReplayHorizon) + MaxSubmitWindow + 1
	if got := len(s.accepted[1]); got > bound {
		t.Fatalf("one seat before the next tick: accepted=%d bounded_window=%d pending_frames=%d",
			got, bound, len(s.pending))
	}
	// 洪水之后,期限内的重传仍然幂等。
	target, err := s.SubmitInput(1, 4090, []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	if again, err := s.SubmitInput(1, 4090, []byte{1}); err != nil || again != target {
		t.Fatalf("in-horizon replay after the flood: frame=%d want=%d err=%v", again, target, err)
	}
}

// 环用取模定位槽位,所以要证明超期限的提交不会挤掉期限内的身份——否则"提交旧帧号"
// 就成了绕过去重的手段。150 与 21 落在同一个槽(150 = 21 + replayWindowSize)。
func TestSubmitInputPromiseOutOfHorizonSubmissionDoesNotEvictAnIdentity(t *testing.T) {
	s, err := NewSequencer(SequencerConfig{Players: []PlayerID{1}})
	if err != nil {
		t.Fatal(err)
	}
	for s.NextFrame() < 200 {
		s.Advance()
	}
	kept, err := s.SubmitInput(1, 150, []byte("a")) // 期限内(floor=136),折进 200
	if err != nil || kept != 200 {
		t.Fatalf("in-horizon fold: frame=%d err=%v", kept, err)
	}
	s.Advance() // 切 200,next=201,floor=137
	collider := FrameID(150 - (int(ReplayHorizon) + MaxSubmitWindow + 1))
	if _, err := s.SubmitInput(1, collider, []byte("b")); err != nil { // 超期限,同一槽位
		t.Fatal(err)
	}
	replay, err := s.SubmitInput(1, 150, []byte("a"))
	if err != nil {
		t.Fatal(err)
	}
	if replay != kept {
		t.Fatalf("an out-of-horizon submission evicted an in-horizon identity: replay folded into %d, the original went into %d", replay, kept)
	}
}

// 超期限的原帧号既不查也不记，所以它的重传仍会再次折入——这是已声明的边界，同时承载内存上界（不记 = 不增长）。
func TestSubmitInputPromiseOutOfHorizonReplayStillFolds(t *testing.T) {
	s, err := NewSequencer(SequencerConfig{Players: []PlayerID{1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitInput(1, 1, []byte{1}); err != nil {
		t.Fatal(err)
	}
	if got := s.Advance(); len(got.Inputs) != 1 {
		t.Fatalf("frame 1: %+v", got)
	}
	for s.NextFrame() <= ReplayHorizon+1 {
		s.Advance()
	}
	if _, err := s.SubmitInput(1, 1, []byte{1}); err != nil {
		t.Fatal(err)
	}
	if got := s.Advance(); len(got.Inputs) != 1 {
		t.Fatalf("documented out-of-horizon behavior changed: frame %d inputs=%d", got.ID, len(got.Inputs))
	}
	if len(s.accepted[1]) > int(ReplayHorizon)+MaxSubmitWindow+1 {
		t.Fatalf("out-of-horizon fold grew the identity ring: %d", len(s.accepted[1]))
	}
}

// SubmitInput 的入参校验排在身份查找之前：先校验再去重，越窗的垃圾帧号既不索引身份环、也不分配环（32 位平台上按客户端给的数
// 索引会 panic）。任何被记住的 original 在写入时都满足 original <= next+window，所以把查找挪到窗口检查之后不会漏掉命中。

func TestSubmitInputPromiseRejectedFrameNeverTouchesTheIdentityRing(t *testing.T) {
	s, err := NewSequencer(SequencerConfig{Players: []PlayerID{1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitInput(1, 4_000_000_000, []byte{1}); !errors.Is(err, ErrFrameTooEarly) {
		t.Fatalf("a far-future frame must be refused: %v", err)
	}
	if ring := s.accepted[1]; ring != nil {
		t.Fatalf("a refused submission indexed the identity ring with the client's frame id before validating it: ring len=%d", len(ring))
	}
	// 合法提交之后环才出现,去重照常工作。
	target, err := s.SubmitInput(1, 1, []byte{1})
	if err != nil || target != 1 {
		t.Fatalf("legitimate submit: frame=%d err=%v", target, err)
	}
	if s.accepted[1] == nil {
		t.Fatal("a legitimate submit must record its identity")
	}
	if again, err := s.SubmitInput(1, 1, []byte{1}); err != nil || again != target {
		t.Fatalf("replay after the reorder: frame=%d want=%d err=%v", again, target, err)
	}
}

// 槽位下标对任意 uint32 都落在环内，且与 int 的位宽无关（32 位平台上 int(original) 会溢出成负数）。
func TestReplaySlotIndexPromiseStaysInRangeForEveryFrameID(t *testing.T) {
	for _, original := range []FrameID{0, 1, 128, 129, 130, 1 << 16, 4_000_000_000, ^FrameID(0)} {
		got := replaySlotIndex(original)
		if got < 0 || got >= replayWindowSize {
			t.Fatalf("original %d maps outside the ring: index=%d size=%d", original, got, replayWindowSize)
		}
		if want := int(original % FrameID(replayWindowSize)); got != want {
			t.Fatalf("original %d: index=%d want=%d (the index must not depend on the width of int)", original, got, want)
		}
	}
}
