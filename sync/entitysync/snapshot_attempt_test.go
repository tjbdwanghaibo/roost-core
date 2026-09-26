package entitysync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/frame"
)

// RR 复现：scheduleSnapshots 在任何 Push 前对全部会话预扣冷快照额度；
// 第一个会话 Push 即 RetryLater 后，同一 on_change 窗口的第二次 Flush 无额度可用。
func TestSnapshotRetryOnlyChargesAttemptedSessions(t *testing.T) {
	r := newRecordingTransport()
	m := newTestManager(t, r, ManagerConfig{Mode: ModeOnChange, Interval: time.Hour, SnapshotBudget: SnapshotBudget{MaxObjects: 3}})
	open(t, m, 1, 2, 3)
	packs := 0
	for sid := SessionID(1); sid <= 3; sid++ {
		id := int64(sid) * 10
		if err := m.Register(testSubject(t, id, &packs)); err != nil {
			t.Fatal(err)
		}
		mustSubscribe(t, m, sid, id, entity.SyncProfile{})
	}
	r.setRetry(ErrRetryLater)
	err := m.Flush(t.Context())
	r.setRetry(nil)
	if !errors.Is(err, ErrRetryLater) {
		t.Fatalf("err=%v", err)
	}
	if m.windowAllowance.objects != 1 || len(m.windowSessions) != 1 || m.windowSessions[1] != 1 || m.snapshotCursor[snapshotArrival] != 1 {
		t.Fatalf("unattempted sessions charged: %+v %v cursor=%v", m.windowAllowance, m.windowSessions, m.snapshotCursor)
	}
	delivered := len(r.take(1)) + len(r.take(2)) + len(r.take(3))
	t.Logf("after RetryLater: windowAllowance=%+v windowSessions=%v snapshotCursor=%v delivered=%d", m.windowAllowance, m.windowSessions, m.snapshotCursor, delivered)
	mustFlush(t, m) // 同一窗口（Interval=1h，未轮转）
	got := len(r.take(1)) + len(r.take(2)) + len(r.take(3))
	s := m.Stats()
	t.Logf("same-window second Flush: frames=%d CreatesAdmitted=%d SnapshotsDeferred=%d PendingSnapshots=%d WaitingSnapshotSubjects=%d", got, s.CreatesAdmitted, s.SnapshotsDeferred, s.PendingSnapshots, s.WaitingSnapshotSubjects)
	if got != 2 {
		t.Fatalf("RetryLater on the first Push consumed the whole window budget for never-attempted sessions")
	}
}

func TestSnapshotBudgetDoesNotChargeUnattemptedSessions(t *testing.T) {
	for _, scenario := range []string{"cancel_before_push", "cancel_after_first", "retry_second", "stale_second", "encode_failure"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			recorder := newRecordingTransport()
			var m *Manager
			calls := 0
			transport := TransportFunc(func(ctx context.Context, sid SessionID, payload []byte) error {
				calls++
				if scenario == "cancel_after_first" {
					cancel()
					return ctx.Err()
				}
				if scenario == "retry_second" && sid == 2 {
					return ErrRetryLater
				}
				if scenario == "stale_second" && sid == 1 {
					m.CloseSession(2)
				}
				return recorder.Push(ctx, sid, payload)
			})
			config := ManagerConfig{Mode: ModeOnChange, Interval: time.Hour, SnapshotBudget: SnapshotBudget{MaxObjects: 3}}
			if scenario == "cancel_before_push" {
				config.DurableWatermark = func() uint64 { cancel(); return ^uint64(0) }
			}
			if scenario == "encode_failure" {
				config.Limits.MaxComponentBytes = 1
			}
			m = newTestManager(t, transport, config)
			open(t, m, 1, 2, 3)
			packs := 0
			for sid := SessionID(1); sid <= 3; sid++ {
				id := int64(sid) * 10
				if err := m.Register(testSubject(t, id, &packs)); err != nil {
					t.Fatal(err)
				}
				mustSubscribe(t, m, sid, id, entity.SyncProfile{})
			}
			err := m.Flush(ctx)
			want, wantErr := 0, error(nil)
			switch scenario {
			case "cancel_before_push":
				wantErr = context.Canceled
			case "cancel_after_first":
				want, wantErr = 1, context.Canceled
			case "retry_second":
				want, wantErr = 2, ErrRetryLater
			case "stale_second":
				want = 2
			}
			if !errors.Is(err, wantErr) {
				t.Fatalf("error=%v want=%v", err, wantErr)
			}
			if calls != want || m.windowAllowance.objects != want || len(m.windowSessions) != want {
				t.Fatalf("calls=%d allowance=%+v sessions=%v want=%d", calls, m.windowAllowance, m.windowSessions, want)
			}
			if scenario == "stale_second" && m.windowSessions[2] != 0 {
				t.Fatal("stale lifetime charged")
			}
		})
	}
}

func TestSnapshotAttemptBudgetPreservesSuccessfulFramePrefix(t *testing.T) {
	recorder := newRecordingTransport()
	calls := 0
	retry := true
	tr := TransportFunc(func(ctx context.Context, sid SessionID, raw []byte) error {
		calls++
		if retry && calls == 2 {
			return ErrRetryLater
		}
		return recorder.Push(ctx, sid, raw)
	})
	m := newTestManager(t, tr, ManagerConfig{Mode: ModeOnChange, Interval: time.Hour, SnapshotBudget: SnapshotBudget{MaxObjects: 4}, Limits: frame.Limits{MaxFrameBytes: 300}})
	open(t, m, 1, 2)
	for id := int64(1); id <= 4; id++ {
		if err := m.Register(largeSubject(id, 100)); err != nil {
			t.Fatal(err)
		}
		sid := SessionID(1)
		if id == 4 {
			sid = 2
		}
		mustSubscribe(t, m, sid, id, entity.SyncProfile{})
	}
	if err := m.Flush(t.Context()); !errors.Is(err, ErrRetryLater) {
		t.Fatal(err)
	}
	first := oneFrame(t, recorder, 1)
	if m.windowAllowance.objects != 3 || m.windowSessions[2] != 0 || len(m.session(1).objects) != 1 {
		t.Fatalf("allowance=%+v sessions=%v objects=%v", m.windowAllowance, m.windowSessions, m.session(1).objects)
	}
	retry = false
	mustFlush(t, m)
	if len(recorder.take(2)) != 1 {
		t.Fatal("never-attempted session lost remaining budget")
	}
	for _, raw := range recorder.take(1) {
		next := decodeFrame(t, raw)
		if next.wire.BaseTick != first.wire.Tick {
			t.Fatal("successful prefix clock rolled back")
		}
		first = next
	}
	m.refreshSnapshotWindowAt(m.budgetWindow.Add(time.Hour))
	mustFlush(t, m)
	if len(m.session(1).objects) != 3 || len(m.session(2).objects) != 1 || m.Stats().PendingSnapshots != 0 {
		t.Fatal("retry failed to converge")
	}
}
