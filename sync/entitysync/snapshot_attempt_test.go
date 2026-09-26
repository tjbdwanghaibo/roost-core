package entitysync

import (
	"context"
	"errors"
	"slices"
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

// RR-20260926-05 复核：游标从中间开始并轮转越过末尾时，计划顺序（如 3→4→1）与按会话 ID
// 的顺序不同。RetryLater 后游标只能停在按计划顺序实际尝试的前缀，不能越过从未尝试的会话 4；
// 计费仍只落在实际尝试 Push 的会话上。
func TestSnapshotRetryWithRotatedCursorDoesNotSkipUnattempted(t *testing.T) {
	cases := []struct {
		name         string
		mode         SyncMode
		maxObjects   int
		cursor, fail SessionID
	}{
		{"on_change_plan_3_4_1", ModeOnChange, 3, 2, 3},
		{"periodic_plan_4_1", ModePeriodic, 2, 3, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := newRecordingTransport()
			fail := tc.fail
			var attempted, firstAttempts []SessionID
			tr := TransportFunc(func(ctx context.Context, sid SessionID, raw []byte) error {
				attempted = append(attempted, sid)
				if sid == fail {
					return ErrRetryLater
				}
				return recorder.Push(ctx, sid, raw)
			})
			m := newTestManager(t, tr, ManagerConfig{Mode: tc.mode, Interval: time.Hour, SnapshotBudget: SnapshotBudget{MaxObjects: tc.maxObjects}})
			open(t, m, 1, 2, 3, 4)
			m.snapshotCursor[snapshotArrival] = tc.cursor // 上一窗口已轮转到中间
			packs := 0
			for sid := SessionID(1); sid <= 4; sid++ {
				if err := m.Register(testSubject(t, int64(sid)*10, &packs)); err != nil {
					t.Fatal(err)
				}
				mustSubscribe(t, m, sid, int64(sid)*10, entity.SyncProfile{})
			}
			if err := m.Flush(t.Context()); !errors.Is(err, ErrRetryLater) {
				t.Fatalf("err=%v", err)
			}
			t.Logf("retry flush: attempted=%v windowSessions=%v allowance=%+v cursor=%v", attempted, m.windowSessions, m.windowAllowance, m.snapshotCursor)
			if tc.mode == ModeOnChange {
				charged := make([]SessionID, 0, len(m.windowSessions))
				for sid := range m.windowSessions {
					charged = append(charged, sid)
				}
				slices.Sort(charged)
				tried := slices.Clone(attempted)
				slices.Sort(tried)
				if !slices.Equal(charged, tried) || m.windowAllowance.objects != len(attempted) {
					t.Fatalf("charged %v (allowance %+v), attempted %v", charged, m.windowAllowance, attempted)
				}
			}
			for sid := SessionID(1); sid <= 4; sid++ {
				recorder.take(sid)
			}
			fail, firstAttempts, attempted = 0, attempted, nil
			mustFlush(t, m) // on_change 仍在同一窗口；periodic 是下一轮
			delivered := []SessionID{}
			for sid := SessionID(1); sid <= 4; sid++ {
				if len(recorder.take(sid)) > 0 {
					delivered = append(delivered, sid)
				}
			}
			t.Logf("second flush delivered=%v cursor=%v", delivered, m.snapshotCursor)
			if _, ok := m.session(4).objects[40]; !ok {
				t.Fatalf("planned session 4 was skipped by the cursor: first attempts=%v, second flush delivered=%v", firstAttempts, delivered)
			}
		})
	}
}

// RR-20260926-05 复核：同一会话跨两个类别时，Push 顺序按会话首次出现，无法与每个类别的
// 计划顺序都一致。计划为 恢复 s2 → 新入场 s1 → 新入场 s2，Push 为 s2、s1；s2 RetryLater 后
// s1 从未尝试，新入场游标不能因为排在它后面的 s2 已尝试就越过它。
func TestSnapshotCursorStopsBeforeUnattemptedAcrossClasses(t *testing.T) {
	recorder := newRecordingTransport()
	fail := SessionID(0)
	var attempted []SessionID
	tr := TransportFunc(func(ctx context.Context, sid SessionID, raw []byte) error {
		attempted = append(attempted, sid)
		if sid == fail {
			return ErrRetryLater
		}
		return recorder.Push(ctx, sid, raw)
	})
	m := newTestManager(t, tr, ManagerConfig{Mode: ModeOnChange, Interval: time.Hour, SnapshotBudget: SnapshotBudget{MaxObjects: 3}})
	open(t, m, 1, 2, 3)
	packs := 0
	sub := func(sid SessionID, id int64) {
		if err := m.Register(testSubject(t, id, &packs)); err != nil {
			t.Fatal(err)
		}
		mustSubscribe(t, m, sid, id, entity.SyncProfile{})
	}
	sub(2, 21)
	mustFlush(t, m)
	if err := m.HoldSession(2); err != nil {
		t.Fatal(err)
	}
	if err := m.ReadySession(2); err != nil { // 21 成为 s2 的恢复请求
		t.Fatal(err)
	}
	sub(1, 11)
	sub(2, 22)
	sub(3, 31)
	m.refreshSnapshotWindowAt(m.budgetWindow.Add(time.Hour))
	m.snapshotCursor, m.snapshotNextClass = [2]SessionID{}, snapshotRecovery
	for sid := SessionID(1); sid <= 3; sid++ {
		recorder.take(sid)
	}
	attempted, fail = nil, 2
	if err := m.Flush(t.Context()); !errors.Is(err, ErrRetryLater) {
		t.Fatalf("err=%v", err)
	}
	t.Logf("retry flush: attempted=%v windowSessions=%v cursor=%v next=%v", attempted, m.windowSessions, m.snapshotCursor, m.snapshotNextClass)
	if !slices.Equal(attempted, []SessionID{2}) {
		t.Fatalf("admission order %v, want only the first planned session [2]", attempted)
	}
	fail = 0
	mustFlush(t, m) // 同一窗口，剩余 1 个额度
	if _, ok := m.session(1).objects[11]; !ok {
		t.Fatalf("arrival cursor passed never-attempted session 1: cursor=%v objects s1=%v s2=%v s3=%v", m.snapshotCursor, m.session(1).objects, m.session(2).objects, m.session(3).objects)
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
