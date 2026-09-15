package statesync

import (
	"errors"
	"testing"
)

// U-0201 · C8 · RR-20260914-11:ForceFull 之前发出的帧,其 ACK 不能取消 ForceFull。
// 两个 ACK 入口(Acknowledge / HandleControl)在推进 ackTick 时无条件 `forceFull = false`,而
// 恢复意图真正的释放点是 commitPrepared 里"属于当前 generation 的全量帧提交成功"。旧发送的 ACK
// 迟到时把恢复承诺清掉,下一帧又是 delta。承诺:ACK 只推进水位,不碰恢复意图。

func TestForceFullPromiseSurvivesAcksOfEarlierSends(t *testing.T) {
	for _, mode := range []string{"ack_before_force", "raw_ack_after_force", "wire_ack_after_force", "old_wire_after_resync"} {
		t.Run(mode, func(t *testing.T) {
			check := func(e error) {
				t.Helper()
				if e != nil {
					t.Fatal(e)
				}
			}
			r := NewReplicator(ReplicatorConfig{})
			defer r.Close()
			check(r.RegisterSession(SessionInfo{ID: 10}))
			s := mustSnapshot(t, 1, []ObjectState{{Ref: ObjectRef{ID: 1, Generation: 1}, Archetype: 1}})
			check(r.Publish(s))
			p, e := r.PrepareLatest(10)
			check(e)
			check(p.Commit())
			ack, e := EncodeControl(ControlMessage{Type: ControlAck, RoomID: s.RoomID, Epoch: s.Epoch, Tick: 1, Sequence: 1})
			check(e)
			if mode == "ack_before_force" {
				check(r.Acknowledge(10, 1))
			}
			check(r.ForceFull(10))
			switch mode {
			case "raw_ack_after_force":
				check(r.Acknowledge(10, 1))
			case "wire_ack_after_force":
				check(r.HandleControl(10, ack))
			case "old_wire_after_resync":
				resync, e := EncodeControl(ControlMessage{Type: ControlResync, RoomID: s.RoomID, Epoch: s.Epoch, Sequence: 2})
				check(e)
				check(r.HandleControl(10, resync))
				if e = r.HandleControl(10, ack); !errors.Is(e, ErrInvalidControl) {
					t.Fatalf("old control accepted: %v", e)
				}
			}
			check(r.Publish(mustSnapshot(t, 2, s.Objects)))
			f, _, e := r.BuildLatest(10)
			check(e)
			if f.Kind != FrameFull {
				t.Fatalf("ForceFull lost: kind=%d base=%d", f.Kind, f.BaseTick)
			}
			// 释放点仍然是全量帧的提交:提交后恢复意图消失,下一帧回到 delta。
			full, e := r.PrepareLatest(10)
			check(e)
			check(full.Commit())
			check(r.Acknowledge(10, 2))
			check(r.Publish(mustSnapshot(t, 3, s.Objects)))
			next, _, e := r.BuildLatest(10)
			check(e)
			if next.Kind != FrameDelta || next.BaseTick != 2 {
				t.Fatalf("after the full frame committed and was acked, want delta from 2: kind=%d base=%d", next.Kind, next.BaseTick)
			}
		})
	}
}
