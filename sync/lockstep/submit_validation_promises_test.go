package lockstep

import (
	"errors"
	"testing"
)

// U-0199 · C8:SubmitInput 的入参校验必须排在身份查找之前。
//
// P1(32 位平台上的 panic):环的定位是 `int(original) % replayWindowSize`,而 `original` 是 uint32
// 且完全由客户端控制。`int` 为 32 位的平台(GOARCH=386 / arm)上 int(4_000_000_000) 溢出成
// -294967296,取模得 -24,`ring[-24]` 直接 panic;Room 是单 goroutine 驱动的,会带走整个房间。
// P2(顺序):身份查找连同环的惰性分配排在窗口检查之前,于是一个会被 ErrFrameTooEarly 拒绝的
// 垃圾帧号照样给这个座位分配了 129 槽的环——校验之前就先按客户端给的数去索引了。
//
// 两条同因:先校验再去重。可证明等价:任何被记住的 original 在写入时都满足
// original <= next+window,而 next 只增不减、window 构造后固定,所以越窗的 original 不可能有
// 身份记录,把查找挪到窗口检查之后不会漏掉任何一次命中。

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

// 槽位下标必须对任意 uint32 都落在环内,且与 int 的位宽无关。修前的表达式在 64 位上恰好也
// 落在范围内,所以这条在本机是绿的;32 位平台才会红,而本机跑不了 386 二进制(只验证了
// `GOARCH=386 go build ./lockstep` 能编译,说明该平台并未被排除)。
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
