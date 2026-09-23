package lockstep

import "testing"

// U-0197 · C8 · RR-20260914-08:去重身份表必须在**准入时**就有界,不能靠下一次 Tick 回收。
// U-0193 加的 accepted[player][original] 对所有过去的原帧号照单全收,只有 Advance 按
// ReplayHorizon 清理:两次 Tick 之间提交大量不同的旧帧号,pending 仍只有一帧,accepted 却随
// 旧帧号数量线性增长,下一次 Advance 还要遍历它们。承诺:每个座位的身份结构是固定容量的
// 环(ReplayHorizon + MaxSubmitWindow + 1 个槽,恰好覆盖任一时刻的合法原帧号区间),准入即
// 有界,Advance 不再做清理遍历;超出期限的原帧号既不查也不记(仍按首次迟到折入,是 U-0193
// 已声明的边界)。

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

// 超期限的原帧号既不查也不记,所以它的重传仍会再次折入——这是 U-0193 已声明的边界,
// 现在还承载着内存上界(不记 = 不增长),钉在这里防止无声改变。
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
