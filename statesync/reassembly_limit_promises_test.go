package statesync

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// U-0202 · C8 · RR-20260914-12:单片与多片重组必须遵守同一个 MaxFrameBytes。ChunkCount==1 的
// 快路径直接返回 payload,绕过多片路径的累计大小检查:同一份 400 字节载荷,分两片被
// ErrFrameTooLarge 拒绝,单片却 done=true 放行。承诺:两条路径对帧大小的判据一致。
func TestReassemblerPromiseSingleChunkHonoursMaxFrameBytes(t *testing.T) {
	for _, size := range []int{300, 1200} {
		t.Run(map[int]string{300: "two_chunks", 1200: "single_chunk"}[size], func(t *testing.T) {
			limits := DefaultLimits()
			limits.MaxFrameBytes = 256
			frame := DeltaFrame{SnapshotMeta: testMeta(1), Kind: FrameFull}
			packets, err := FragmentFrame(frame, 1, bytes.Repeat([]byte{1}, 400), size, DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			r := NewReassembler(limits, time.Second)
			var last error
			var done bool
			var out []byte
			for _, packet := range packets {
				out, done, _, last = r.PushFor(10, packet, time.Now())
				if last != nil {
					break
				}
			}
			if !errors.Is(last, ErrFrameTooLarge) {
				t.Fatalf("datagram=%d chunks=%d accepted=%v bytes=%d err=%v", size, len(packets), done, len(out), last)
			}
			if r.Len() != 0 {
				t.Fatalf("a refused frame left %d assemblies in flight", r.Len())
			}
		})
	}
	// 对照:恰好等于上限的单片放行;上限之内的多片放行。
	limits := DefaultLimits()
	limits.MaxFrameBytes = 400
	frame := DeltaFrame{SnapshotMeta: testMeta(1), Kind: FrameFull}
	for _, size := range []int{300, 1200} {
		packets, err := FragmentFrame(frame, 1, bytes.Repeat([]byte{1}, 400), size, DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		r := NewReassembler(limits, time.Second)
		var out []byte
		var done bool
		for _, packet := range packets {
			var err error
			if out, done, _, err = r.PushFor(10, packet, time.Now()); err != nil {
				t.Fatalf("datagram=%d: a frame exactly at the limit was refused: %v", size, err)
			}
		}
		if !done || len(out) != 400 {
			t.Fatalf("datagram=%d: done=%v bytes=%d", size, done, len(out))
		}
	}
}
