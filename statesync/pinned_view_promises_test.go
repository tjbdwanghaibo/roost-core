package statesync

import (
	"errors"
	"testing"
)

// U-0205 · C8 · RR-20260915-02:一个 tick 的视图要在**第一次准备**时就固定,不能等到提交。
// U-0200 在提交时冻结:同 tick 的两个 PreparedFrame 若都在任一 Commit 之前准备好,视图可以不同;
// 运输先交付了第二个(B)、客户端已应用,B.Commit 才按 stale 拒绝——服务端 sent[1] 是 A,客户端持有 B,
// ACK(1) 被接受。tick 2 的投影回到 A 的视图时 delta 是零变化,客户端永远多(或少)一个对象,
// 也不会有 ErrObjectNotFound 来触发 resync。承诺:同 tick 的所有准备共享第一次投影出的视图,
// 无论哪一份被交付、哪一份被提交,客户端拿到的都是同一个视图。

func TestPrepareLatestPromiseOverlappingPreparesShareOneView(t *testing.T) {
	for _, mode := range []string{"extra_object", "missing_object", "explicit_full_control"} {
		t.Run(mode, func(t *testing.T) {
			check := func(e error) {
				t.Helper()
				if e != nil {
					t.Fatal(e)
				}
			}
			firstShow := mode == "missing_object"
			show := firstShow
			r := NewReplicator(ReplicatorConfig{Projector: ProjectorFunc(func(_ SessionInfo, s Snapshot) (Snapshot, error) {
				if !show {
					s.Objects = s.Objects[:1]
				}
				return s, nil
			})})
			defer r.Close()
			check(r.RegisterSession(SessionInfo{ID: 10}))
			s := mustSnapshot(t, 1, []ObjectState{{Ref: ObjectRef{ID: 1, Generation: 1}, Archetype: 1}, {Ref: ObjectRef{ID: 2, Generation: 1}, Archetype: 1}})
			check(r.Publish(s))
			a, e := r.PrepareLatest(10)
			check(e)
			show = !firstShow
			b, e := r.PrepareLatest(10) // 兴趣变了,但同一 tick:必须拿到 a 的视图
			check(e)
			check(a.Commit())
			client, e := ApplyDelta(nil, b.Frame, DefaultLimits()) // 运输交付了 b,客户端先应用
			check(e)
			if e = b.Commit(); e != nil && !errors.Is(e, ErrPreparedFrameStale) {
				t.Fatalf("second commit: %v", e)
			}
			check(r.Acknowledge(10, 1))
			show = firstShow
			check(r.Publish(mustSnapshot(t, 2, s.Objects)))
			if mode == "explicit_full_control" {
				check(r.ForceFull(10))
			}
			p, e := r.PrepareLatest(10)
			check(e)
			client, e = ApplyDelta(&client, p.Frame, DefaultLimits())
			check(e)
			check(p.Commit())
			want := 1
			if firstShow {
				want = 2
			}
			if len(client.Objects) != want {
				t.Fatalf("stale commit followed by successful delta: objects=%d want=%d changes=%d", len(client.Objects), want, len(p.Frame.Objects))
			}
		})
	}
}

// 兜底:万一同 tick 的不同视图还是走到了提交(只能绕过 PrepareLatest 直接构造),被拒的那一刻
// 会话必须进入恢复态——该 tick 的基线已经歧义,不能再当 delta 基线用;下一帧全量。
func TestCommitPromiseRejectedDivergentViewForcesRecovery(t *testing.T) {
	state, err := NewSessionState(SessionInfo{ID: 10})
	if err != nil {
		t.Fatal(err)
	}
	viewA := mustSnapshot(t, 1, []ObjectState{{Ref: ObjectRef{ID: 1, Generation: 1}, Archetype: 1}})
	viewB := mustSnapshot(t, 1, []ObjectState{{Ref: ObjectRef{ID: 1, Generation: 1}, Archetype: 1}, {Ref: ObjectRef{ID: 2, Generation: 1}, Archetype: 1}})
	first, err := state.prepare(1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := state.prepare(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.commitPrepared(viewA, first.sequence, first.generation, true); err != nil {
		t.Fatal(err)
	}
	if err := state.Acknowledge(1, 1); err != nil {
		t.Fatal(err)
	}
	if err := state.commitPrepared(viewB, second.sequence, second.generation, true); !errors.Is(err, ErrPreparedFrameStale) {
		t.Fatalf("divergent view must be refused: %v", err)
	}
	snap := state.Snapshot()
	if !snap.ForceFull {
		t.Fatalf("a refused divergent view left the ambiguous tick usable as a baseline: %+v", snap)
	}
	next, err := state.prepare(2)
	if err != nil {
		t.Fatal(err)
	}
	if !next.fullRefresh || next.base != nil {
		t.Fatalf("after an ambiguous baseline the next frame must be full: fullRefresh=%v base=%v", next.fullRefresh, next.base != nil)
	}
}
