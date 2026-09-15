package statesync

import (
	"errors"
	"testing"
)

// U-0200 · C8 · RR-20260914-10:一个 tick 对一个会话只能有一个视图。`sent` 按 tick 建键,
// 而同一 tick 可以被重新投影并再次提交(兴趣变化),第二次覆盖 sent[tick];ACK 只说"tick 1",
// 分不清客户端拿到的是哪一版——迟到的第一版 ACK 绑到了第二版基线,之后的 delta 变化数为 0,
// 客户端却永远少一个对象。承诺:tick 的视图在第一次提交时冻结,同 tick 的再次准备复用已发送
// 的视图(兴趣变化延到下一 tick 才生效);并发准备的不同视图在提交时按 stale 拒绝。

func TestPrepareLatestPromiseFreezesATicksViewAtFirstCommit(t *testing.T) {
	for _, resend := range []bool{false, true} {
		name := "next_tick_control"
		if resend {
			name = "same_tick_lost_revision"
		}
		t.Run(name, func(t *testing.T) {
			show := false
			r := NewReplicator(ReplicatorConfig{Projector: ProjectorFunc(func(_ SessionInfo, s Snapshot) (Snapshot, error) {
				if !show {
					s.Objects = s.Objects[:1]
				}
				return s, nil
			})})
			defer r.Close()
			check := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			check(r.RegisterSession(SessionInfo{ID: 10}))
			s := mustSnapshot(t, 1, []ObjectState{{Ref: ObjectRef{ID: 1, Generation: 1}, Archetype: 1}, {Ref: ObjectRef{ID: 2, Generation: 1}, Archetype: 99}})
			check(r.Publish(s))
			p, err := r.PrepareLatest(10)
			check(err)
			check(p.Commit())
			client, err := ApplyDelta(nil, p.Frame, DefaultLimits())
			check(err)
			show = true
			if resend {
				// 同 tick 兴趣变化后再准备:被传输接受,然后丢在网络里。
				p2, err := r.PrepareLatest(10)
				check(err)
				check(p2.Commit())
			}
			check(r.Acknowledge(10, 1)) // 第一版的 ACK 迟到
			check(r.Publish(mustSnapshot(t, 2, s.Objects)))
			frame, _, err := r.BuildLatest(10)
			check(err)
			client, err = ApplyDelta(&client, frame, DefaultLimits())
			check(err)
			if len(client.Objects) != 2 {
				t.Fatalf("client objects=%d want=2, delta changes=%d base=%d", len(client.Objects), len(frame.Objects), frame.BaseTick)
			}
		})
	}
}

// 并发准备:两个视图都在提交之前准备好。第一个提交冻结视图,第二个(不同内容)必须按 stale 拒绝,
// 而不是覆盖。同内容的重复提交仍然幂等。
func TestCommitPromiseRejectsADifferentViewOfAnAlreadySentTick(t *testing.T) {
	show := false
	r := NewReplicator(ReplicatorConfig{Projector: ProjectorFunc(func(_ SessionInfo, s Snapshot) (Snapshot, error) {
		if !show {
			s.Objects = s.Objects[:1]
		}
		return s, nil
	})})
	defer r.Close()
	if err := r.RegisterSession(SessionInfo{ID: 10}); err != nil {
		t.Fatal(err)
	}
	s := mustSnapshot(t, 1, []ObjectState{{Ref: ObjectRef{ID: 1, Generation: 1}, Archetype: 1}, {Ref: ObjectRef{ID: 2, Generation: 1}, Archetype: 99}})
	if err := r.Publish(s); err != nil {
		t.Fatal(err)
	}
	first, err := r.PrepareLatest(10)
	if err != nil {
		t.Fatal(err)
	}
	show = true
	second, err := r.PrepareLatest(10) // 不同视图,尚未提交
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := second.Commit(); !errors.Is(err, ErrPreparedFrameStale) {
		t.Fatalf("a different view of an already-sent tick was committed over the first: %v", err)
	}
	// 冻结后再准备同 tick:复用第一版视图,提交幂等。
	again, err := r.PrepareLatest(10)
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Commit(); err != nil {
		t.Fatalf("re-preparing the frozen view must commit idempotently: %v", err)
	}
	if got := len(again.Frame.Objects); got != 1 {
		t.Fatalf("frozen view leaked the newer projection: objects in frame=%d want 1", got)
	}
}
