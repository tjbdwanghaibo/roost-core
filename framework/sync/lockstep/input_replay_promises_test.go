package lockstep

import "testing"

// U-0193 · C8 · RR-20260914-04:同一份原始输入 (player, frame) 只能入帧一次。旧实现只在目标帧的
// pending map 里去重,Advance 切帧后删掉 map,不保留"这份输入已经处理过"的身份;迟到重传把原帧号
// 改成 next 再入帧——已进帧 1 的输入重传后又进帧 2,首次迟到折进帧 2 的重传后又进帧 3。一次性
// 开火 / 技能会被执行两次。承诺:重传幂等(返回当初折入的帧号、不再入帧),同时保留"首次迟到
// 折入下一帧"的策略;身份按每玩家有界窗口保留,只存最大已见帧号会误丢乱序到达的低帧号。

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
