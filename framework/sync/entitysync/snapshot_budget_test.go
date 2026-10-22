// 快照预算：整实体分帧、预算按窗口轮转且不延误增量、只对实际尝试的会话计费、游标不越过未尝试者、已有对象刷新不占冷额度。

package entitysync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
)

func TestProfileDowngradeReplacesClientFields(t *testing.T) {
	owner, near, far := entity.SyncProfile{Key: "owner"}, entity.SyncProfile{Key: "near"}, entity.SyncProfile{Key: "far"}
	views, err := entity.NewSyncViewSet(entity.SyncView{Profile: owner, Fields: 7}, entity.SyncView{Profile: near, Fields: 3}, entity.SyncView{Profile: far, Fields: 1})
	if err != nil {
		t.Fatal(err)
	}
	pack, err := entity.NewMaskedSyncPacker(views, 1, func(mask uint64) ([]byte, error) {
		fields := map[string]int{}
		for i, name := range []string{"position", "equipment", "gold"} {
			if mask&(1<<i) != 0 {
				fields[name] = i + 1
			}
		}
		return json.Marshal(fields)
	})
	if err != nil {
		t.Fatal(err)
	}
	r := newRecordingTransport()
	m := newTestManager(t, r, ManagerConfig{})
	s := entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{Enabled: true, SubjectID: 1, Packer: pack})
	if err := m.Register(s); err != nil {
		t.Fatal(err)
	}
	open(t, m, 1)
	client := map[string]int{}
	for i, p := range []entity.SyncProfile{owner, near, far} {
		mustSubscribe(t, m, 1, 1, p)
		mustFlush(t, m)
		u := oneFrame(t, r, 1).updates[1]
		if !u.Full || u.Profile != p.Normalize() {
			t.Fatal("profile switch is not a full replacement")
		}
		clear(client)
		if err := json.Unmarshal(u.Payload.BytesCopy(), &client); err != nil {
			t.Fatal(err)
		}
		if len(client) != 3-i || client["position"] != 1 {
			t.Fatalf("stale fields: %+v", client)
		}
	}
}

type boundedRecorder struct {
	*recordingTransport
	limit int
}

func (r boundedRecorder) MaxFrameBytes() int { return r.limit }

func largeSubject(id int64, size int) *entity.SubjectSyncState {
	pack := func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
		return entity.TakeFrozenSyncPayload(1, bytes.Repeat([]byte{byte(id)}, size)), nil
	}
	return entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{Enabled: true, SubjectID: id, Packer: entity.SubjectSyncPackFunc{Snapshot: pack}})
}

func TestWholeEntityPacketsSplitAndRetainIndependentFrameState(t *testing.T) {
	r := boundedRecorder{newRecordingTransport(), 300}
	m := newTestManager(t, r, ManagerConfig{})
	open(t, m, 1)
	for id := int64(1); id <= 3; id++ {
		if err := m.Register(largeSubject(id, 100)); err != nil {
			t.Fatal(err)
		}
		mustSubscribe(t, m, 1, id, entity.SyncProfile{})
	}
	mustFlush(t, m)
	frames := r.take(1)
	if len(frames) != 3 {
		t.Fatalf("frames=%d", len(frames))
	}
	for i, raw := range frames {
		if len(raw) > 300 {
			t.Fatal("transport limit ignored")
		}
		got := decodeFrame(t, raw)
		if len(got.wire.Objects) != 1 || got.wire.Tick != uint32(i+1) || got.wire.BaseTick != uint32(i) {
			t.Fatalf("frame: %+v", got.wire)
		}
		id := int64(i + 1)
		if !bytes.Equal(got.updates[id].Payload.AppendTo(nil), bytes.Repeat([]byte{byte(id)}, 100)) {
			t.Fatal("entity was split or overwritten")
		}
	}
	if m.Stats().SessionsLost != 0 {
		t.Fatal("legal complete packets lost session")
	}
	// 原始工作区的后续复用不能改写已交付帧。
	for range 3 {
		mustFlush(t, m)
	}
	if !bytes.Equal(decodeFrame(t, frames[0]).updates[1].Payload.AppendTo(nil), bytes.Repeat([]byte{1}, 100)) {
		t.Fatal("frame ownership lost")
	}
}

func TestWholeEntityTooLargeIsExplicitAndNeverTruncated(t *testing.T) {
	r := boundedRecorder{newRecordingTransport(), 150}
	var cause error
	m := newTestManager(t, r, ManagerConfig{SessionLost: func(_ SessionID, err error) { cause = err }})
	open(t, m, 1)
	if err := m.Register(largeSubject(1, 100)); err != nil {
		t.Fatal(err)
	}
	mustSubscribe(t, m, 1, 1, entity.SyncProfile{})
	mustFlush(t, m)
	if !errors.Is(cause, frame.ErrFrameTooLarge) || len(r.take(1)) != 0 {
		t.Fatalf("cause=%v", cause)
	}
	// 帧没有交给传输：原因归为编码失败的 too_large，接入层据此断开仍活着的连接（不是推送拒收）。
	if got := LossReason(cause); got != SessionLossTooLarge {
		t.Fatalf("loss reason = %q, want %q", got, SessionLossTooLarge)
	}
}

func TestWholeEntitySplitRetryContinuesFrameAndContentBaselines(t *testing.T) {
	r := newRecordingTransport()
	calls := 0
	fail := true
	tr := TransportFunc(func(ctx context.Context, sid SessionID, data []byte) error {
		calls++
		if fail && calls == 2 {
			return ErrRetryLater
		}
		return r.Push(ctx, sid, data)
	})
	m := newTestManager(t, tr, ManagerConfig{Limits: frame.Limits{MaxFrameBytes: 300}})
	open(t, m, 1)
	for id := int64(1); id <= 3; id++ {
		if err := m.Register(largeSubject(id, 100)); err != nil {
			t.Fatal(err)
		}
		mustSubscribe(t, m, 1, id, entity.SyncProfile{})
	}
	if err := m.Flush(context.Background()); !errors.Is(err, ErrRetryLater) {
		t.Fatal(err)
	}
	first := oneFrame(t, r, 1)
	if len(m.session(1).objects) != 1 {
		t.Fatal("undelivered suffix changed references")
	}
	fail = false
	mustFlush(t, m)
	tick := first.wire.Tick
	for _, raw := range r.take(1) {
		f := decodeFrame(t, raw)
		if f.wire.BaseTick != tick {
			t.Fatal("frame clock rollback")
		}
		tick = f.wire.Tick
		for _, u := range f.updates {
			if !u.Full {
				t.Fatal("retry did not restore full view")
			}
		}
	}
	if len(m.session(1).objects) != 3 || m.Counters().SessionsLost != 0 {
		t.Fatal("retry did not recover")
	}
}

func TestSnapshotBudgetRotatesAndDoesNotDelayLiveUpdates(t *testing.T) {
	r := newRecordingTransport()
	m := newTestManager(t, r, ManagerConfig{SnapshotBudget: SnapshotBudget{MaxObjects: 1, PerSessionObjects: 1}})
	packs := 0
	state := testSubject(t, 10, &packs)
	if err := m.Register(state); err != nil {
		t.Fatal(err)
	}
	open(t, m, 1, 2, 3)
	for _, sid := range []SessionID{1, 2, 3} {
		mustSubscribe(t, m, sid, 10, entity.SyncProfile{})
	}
	for n := 1; n <= 3; n++ {
		if n > 1 {
			state.MarkDirty(1)
		}
		mustFlush(t, m)
		for sid := SessionID(1); sid <= 3; sid++ {
			frames := r.take(sid)
			if sid > SessionID(n) {
				if len(frames) != 0 {
					t.Fatal("snapshot escaped budget")
				}
				continue
			}
			if len(frames) != 1 {
				t.Fatalf("live or selected session %d omitted", sid)
			}
			u := decodeFrame(t, frames[0]).updates[10]
			if sid == SessionID(n) && (!u.Full || u.Version != state.Version()) {
				t.Fatal("deferred snapshot used stale version")
			}
			if sid < SessionID(n) && (u.Full || u.BaseVersion+1 != u.Version) {
				t.Fatal("live update was delayed/replaced")
			}
		}
	}
	if m.Counters().SnapshotsDeferred != 3 || m.Stats().Pending != 0 {
		t.Fatalf("stats: %+v", m.Stats())
	}
}

func TestSnapshotByteBudgetProgressAndDeferredLeave(t *testing.T) {
	r := newRecordingTransport()
	m := newTestManager(t, r, ManagerConfig{SnapshotBudget: SnapshotBudget{MaxBytes: 1}})
	open(t, m, 1)
	for id := int64(1); id <= 3; id++ {
		if err := m.Register(largeSubject(id, 100)); err != nil {
			t.Fatal(err)
		}
		mustSubscribe(t, m, 1, id, entity.SyncProfile{})
	}
	mustFlush(t, m)
	oneFrame(t, r, 1)
	if err := m.Unsubscribe(1, 2); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, m)
	f := oneFrame(t, r, 1)
	if len(f.updates) != 1 || f.updates[3].SubjectID != 3 {
		t.Fatal("deferred leave became a ghost create")
	}
	if len(m.session(1).objects) != 2 {
		t.Fatal("soft byte budget did not make progress")
	}
}

func TestSnapshotObjectBudgetSkipsUnscheduledCapture(t *testing.T) {
	r := newRecordingTransport()
	m := newTestManager(t, r, ManagerConfig{SnapshotBudget: SnapshotBudget{MaxObjects: 1}})
	open(t, m, 1)
	packs := 0
	for id := int64(1); id <= 4; id++ {
		if err := m.Register(testSubject(t, id, &packs)); err != nil {
			t.Fatal(err)
		}
		mustSubscribe(t, m, 1, id, entity.SyncProfile{})
	}
	for tick := 1; tick <= 4; tick++ {
		mustFlush(t, m)
		if packs != tick {
			t.Fatalf("tick %d packed %d: deferred snapshots recaptured", tick, packs)
		}
		oneFrame(t, r, 1)
	}
	if m.Stats().Pending != 0 || m.Counters().SnapshotsCaptured != 4 {
		t.Fatal("snapshots did not converge")
	}
}

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

// 游标从中间开始并轮转越过末尾时，RetryLater 后游标只停在按计划顺序实际尝试的前缀，不越过从未尝试的会话；计费只落在实际尝试
// Push 的会话上。
// 历史：RR-20260926-05。
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

// 同一会话跨两个类别时 Push 顺序按会话首次出现：计划为 恢复 s2 → 新入场 s1 → 新入场 s2，Push 为 s2、s1；s2 RetryLater 后
// s1 从未尝试，新入场游标不能越过它。
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

// 持续 RetryLater 下的会话公平：冷预算按窗口受限、传输每个窗口只接下第一帧、最低 ID 会话每个窗口还持续新增冷对象时，其余每个
// 会话的冷创建仍在有界窗口数内交付——游标跟随真实尝试、不越过未尝试者，失败的尝试照常计费。periodic 与 on_change 各跑一遍
// （on_change 把窗口起点前移一个 Interval 来确定性轮转，不 sleep）。
func TestEverySessionIsDeliveredUnderPersistentRetryLater(t *testing.T) {
	const (
		sessions   = 6
		maxObjects = 2
		// 每个窗口只成功一帧；最低 ID 会话每窗口新增一个对象，与其余会话竞争同一份额度。界取会话数的 4 倍。
		windowBound = 4 * sessions
	)
	for _, mode := range []SyncMode{ModePeriodic, ModeOnChange} {
		t.Run(mode.String(), func(t *testing.T) {
			recorder := newRecordingTransport()
			pushes := 0
			transport := TransportFunc(func(ctx context.Context, sid SessionID, raw []byte) error {
				pushes++
				if pushes > 1 {
					return ErrRetryLater // 本窗口的第二帧起传输不可用
				}
				return recorder.Push(ctx, sid, raw)
			})
			m := newTestManager(t, transport, ManagerConfig{Mode: mode, Interval: time.Hour, SnapshotBudget: SnapshotBudget{MaxObjects: maxObjects}})
			ids := make([]SessionID, 0, sessions)
			for sid := SessionID(1); sid <= sessions; sid++ {
				ids = append(ids, sid)
			}
			open(t, m, ids...)
			packs := 0
			for _, sid := range ids {
				if err := m.Register(testSubject(t, int64(sid)*100, &packs)); err != nil {
					t.Fatal(err)
				}
				mustSubscribe(t, m, sid, int64(sid)*100, entity.SyncProfile{})
			}

			deliveredAt := make(map[SessionID]int)
			nextArrival := int64(10_000)
			retries := 0
			for window := 1; window <= windowBound && len(deliveredAt) < sessions; window++ {
				// 最低 ID 的会话持续有新的小对象入场。
				nextArrival++
				if err := m.Register(testSubject(t, nextArrival, &packs)); err != nil {
					t.Fatal(err)
				}
				mustSubscribe(t, m, 1, nextArrival, entity.SyncProfile{})
				if mode == ModeOnChange && !m.budgetWindow.IsZero() {
					m.budgetWindow = m.budgetWindow.Add(-m.config.Interval) // 进入下一个窗口
				}
				pushes = 0
				if err := m.Flush(t.Context()); err != nil {
					if !errors.Is(err, ErrRetryLater) {
						t.Fatalf("window %d: %v", window, err)
					}
					retries++
				}
				for _, sid := range ids {
					recorder.take(sid)
					if _, done := deliveredAt[sid]; !done && m.sessionHoldsSubject(sid, int64(sid)*100) {
						deliveredAt[sid] = window
					}
				}
			}
			t.Logf("%s: first delivery window per session=%v, windows ending in RetryLater=%d", mode, deliveredAt, retries)
			if retries == 0 {
				t.Fatal("premise: no window ended in RetryLater")
			}
			var missing []string
			for _, sid := range ids {
				if _, done := deliveredAt[sid]; !done {
					missing = append(missing, fmt.Sprint(sid))
				}
			}
			if len(missing) > 0 {
				t.Fatalf("sessions %v never received their cold create within %d windows of persistent RetryLater (delivered=%v)", missing, windowBound, deliveredAt)
			}
		})
	}
}

func TestSnapshotWindowDoesNotDriftOrAccumulate(t *testing.T) {
	m := newTestManager(t, newRecordingTransport(), ManagerConfig{Mode: ModeOnChange, Interval: 10 * time.Millisecond,
		SnapshotBudget: SnapshotBudget{MaxObjects: 2, PerSessionObjects: 1}})
	start := time.Unix(100, 0)
	m.refreshSnapshotWindowAt(start)
	m.windowAllowance.objects, m.windowSessions[1] = 2, 1
	m.refreshSnapshotWindowAt(start.Add(9 * time.Millisecond))
	if m.windowAllowance.objects != 2 || m.windowSessions[1] != 1 {
		t.Fatal("budget refilled before its boundary")
	}
	m.refreshSnapshotWindowAt(start.Add(17 * time.Millisecond))
	if !m.budgetWindow.Equal(start.Add(10*time.Millisecond)) || m.windowAllowance.objects != 0 || len(m.windowSessions) != 0 {
		t.Fatal("late wake shifted next boundary or retained usage")
	}
	m.windowAllowance.objects = 2
	m.refreshSnapshotWindowAt(start.Add(20 * time.Millisecond))
	if m.windowAllowance.objects != 0 {
		t.Fatal("late wake delayed next refill")
	}
	m.refreshSnapshotWindowAt(start.Add(time.Hour + time.Millisecond))
	if !m.budgetWindow.Equal(start.Add(time.Hour)) || m.windowAllowance != (snapshotAllowance{}) {
		t.Fatal("idle windows accumulated credit")
	}
}

// 现有对象的全量视图替换不能因新对象恢复额度耗尽而一直停留在旧视图。
func TestExistingObjectRefreshDoesNotWaitForColdSnapshotBudget(t *testing.T) {
	for name, budget := range map[string]SnapshotBudget{
		"objects": {MaxObjects: 1}, "bytes": {MaxBytes: 1}, "session": {PerSessionObjects: 1},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRecordingTransport()
			m := newTestManager(t, r, ManagerConfig{Mode: ModeOnChange, Interval: time.Hour, SnapshotBudget: budget})
			open(t, m, 1)
			for id := int64(1); id <= 2; id++ {
				packs := 0
				if err := m.Register(testSubject(t, id, &packs)); err != nil {
					t.Fatal(err)
				}
			}
			mustSubscribe(t, m, 1, 1, entity.SyncProfile{Key: "old"})
			mustFlush(t, m)
			oneFrame(t, r, 1)
			ref := m.session(1).objects[1]
			mustSubscribe(t, m, 1, 2, entity.SyncProfile{})
			mustSubscribe(t, m, 1, 1, entity.SyncProfile{Key: "new"})
			mustFlush(t, m)
			frames := r.take(1)
			if len(frames) != 1 {
				t.Fatalf("existing object refresh waited for cold snapshot budget: frames=%d", len(frames))
			}
			f := decodeFrame(t, frames[0])
			if len(f.updates) != 1 || !f.updates[1].Full || f.updates[1].Profile.Key != "new" || f.objects[1] != frame.ObjectUpdate {
				t.Fatalf("wrong refresh: %+v", f)
			}
			if m.session(1).objects[1] != ref || m.Counters().CreatesAdmitted != 1 {
				t.Fatal("refresh recreated object or cold object escaped budget")
			}
			if m.Stats().PendingSnapshots != 1 {
				t.Fatal("cold baseline lost")
			}
			m.budgetWindow = time.Now().Add(-2 * time.Hour)
			mustFlush(t, m)
			if m.Counters().CreatesAdmitted != 2 || m.Stats().Pending != 0 {
				t.Fatal("cold baseline did not resume")
			}
		})
	}
}

func TestExistingObjectRefreshLeavesColdQuotaAndHoldRestoresIt(t *testing.T) {
	r := newRecordingTransport()
	m := newTestManager(t, r, ManagerConfig{SnapshotBudget: SnapshotBudget{MaxObjects: 1, PerSessionObjects: 1}})
	open(t, m, 1)
	for id := int64(1); id <= 2; id++ {
		packs := 0
		if err := m.Register(testSubject(t, id, &packs)); err != nil {
			t.Fatal(err)
		}
	}
	mustSubscribe(t, m, 1, 1, entity.SyncProfile{})
	mustFlush(t, m)
	oneFrame(t, r, 1)
	mustSubscribe(t, m, 1, 1, entity.SyncProfile{Key: "refresh"})
	mustSubscribe(t, m, 1, 2, entity.SyncProfile{})
	mustFlush(t, m)
	if got := m.Counters(); got.CreatesAdmitted != 2 || got.UpdatesAdmitted != 1 {
		t.Fatalf("refresh consumed cold quota: %+v", got)
	}
	if err := m.HoldSession(1); err != nil {
		t.Fatal(err)
	}
	if err := m.ReadySession(1); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, m)
	if got := m.Counters().CreatesAdmitted; got != 3 {
		t.Fatalf("reset references escaped cold quota: %d", got)
	}
	if m.Stats().PendingSnapshots != 1 {
		t.Fatal("reset should still have one cold baseline")
	}
	mustFlush(t, m)
	if m.Counters().CreatesAdmitted != 4 || m.Stats().Pending != 0 {
		t.Fatal("reset did not complete")
	}
}

// 验证的是客户端真正收到的 create：数量预选与字节准入不能互相推翻顺序。
func TestSnapshotArrivalAndRecoveryAlternate(t *testing.T) {
	for name, budget := range map[string]SnapshotBudget{
		"objects": {MaxObjects: 1}, "bytes": {MaxBytes: 1}, "session": {PerSessionObjects: 1},
	} {
		for _, mode := range []SyncMode{ModePeriodic, ModeOnChange} {
			for _, recoveryFirst := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/mode%d/lowRecovery%t", name, mode, recoveryFirst), func(t *testing.T) {
					r := newRecordingTransport()
					m := newTestManager(t, r, ManagerConfig{Mode: mode, Interval: time.Hour, SnapshotBudget: budget})
					open(t, m, 1)
					old, added := []int64{1, 2}, []int64{100, 101}
					if !recoveryFirst {
						old, added = added, old
					}
					packs := 0
					for _, id := range append(append([]int64{}, old...), added...) {
						if err := m.Register(testSubject(t, id, &packs)); err != nil {
							t.Fatal(err)
						}
					}
					flush := func() {
						m.budgetWindow = time.Now().Add(-2 * time.Hour)
						mustFlush(t, m)
					}
					for _, id := range old {
						mustSubscribe(t, m, 1, id, entity.SyncProfile{})
						flush()
						oneFrame(t, r, 1)
					}
					if err := m.HoldSession(1); err != nil {
						t.Fatal(err)
					}
					// 改 Profile 不能把恢复转换为新入场。
					mustSubscribe(t, m, 1, old[0], entity.SyncProfile{Key: "changed"})
					for _, id := range added {
						mustSubscribe(t, m, 1, id, entity.SyncProfile{})
					}
					mustFlush(t, m)
					if len(r.take(1)) != 0 {
						t.Fatal("held session received frame")
					}
					if err := m.ReadySession(1); err != nil {
						t.Fatal(err)
					}
					var previous bool
					for i := range 4 {
						flush()
						f := oneFrame(t, r, 1)
						if len(f.updates) != 1 {
							t.Fatalf("budget must admit one create: %+v", f.updates)
						}
						for id := range f.updates {
							recovery := id == old[0] || id == old[1]
							if i > 0 && recovery == previous {
								t.Fatalf("one class starved: tick=%d recovery=%t", i, recovery)
							}
							previous = recovery
						}
						if mode == ModeOnChange {
							mustFlush(t, m)
							if len(r.take(1)) != 0 {
								t.Fatal("immediate flush spent the same window twice")
							}
						}
					}
					if m.Stats().Pending != 0 || len(m.session(1).objects) != 4 {
						t.Fatal("baselines failed to converge")
					}
				})
			}
		}
	}
}

func TestSnapshotIdleClassBorrowsBudgetAndRotatesSessions(t *testing.T) {
	r := newRecordingTransport()
	m := newTestManager(t, r, ManagerConfig{SnapshotBudget: SnapshotBudget{MaxObjects: 3, PerSessionObjects: 2}})
	open(t, m, 1, 2)
	packs := 0
	for id := int64(1); id <= 4; id++ {
		if err := m.Register(testSubject(t, id, &packs)); err != nil {
			t.Fatal(err)
		}
		for _, sid := range []SessionID{1, 2} {
			mustSubscribe(t, m, sid, id, entity.SyncProfile{})
		}
	}
	for _, want := range [][2]int{{2, 1}, {2, 1}, {0, 2}} {
		mustFlush(t, m)
		for sid := SessionID(1); sid <= 2; sid++ {
			got := 0
			for _, raw := range r.take(sid) {
				got += len(decodeFrame(t, raw).updates)
			}
			if got != want[sid-1] {
				t.Fatalf("session %d got %d want %d", sid, got, want[sid-1])
			}
		}
	}
	if m.Stats().Pending != 0 {
		t.Fatal("idle class reserved unused budget")
	}
}

func TestSnapshotClassesRotateAcrossSessions(t *testing.T) {
	r := newRecordingTransport()
	m := newTestManager(t, r, ManagerConfig{SnapshotBudget: SnapshotBudget{MaxObjects: 1}})
	open(t, m, 1, 2, 3, 4)
	packs := 0
	if err := m.Register(testSubject(t, 1, &packs)); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []SessionID{1, 2} {
		mustSubscribe(t, m, sid, 1, entity.SyncProfile{})
		mustFlush(t, m)
		oneFrame(t, r, sid)
	}
	for _, sid := range []SessionID{1, 2} {
		if err := m.HoldSession(sid); err != nil {
			t.Fatal(err)
		}
		if err := m.ReadySession(sid); err != nil {
			t.Fatal(err)
		}
	}
	for _, sid := range []SessionID{3, 4} {
		mustSubscribe(t, m, sid, 1, entity.SyncProfile{})
	}
	seen := make(map[SessionID]bool)
	var previous bool
	for tick := range 4 {
		mustFlush(t, m)
		admitted := 0
		for sid := SessionID(1); sid <= 4; sid++ {
			for _, raw := range r.take(sid) {
				admitted++
				if seen[sid] || len(decodeFrame(t, raw).updates) != 1 {
					t.Fatalf("session %d failed to make independent progress", sid)
				}
				seen[sid] = true
				recovery := sid <= 2
				if tick > 0 && recovery == previous {
					t.Fatal("session order overrode class fairness")
				}
				previous = recovery
			}
		}
		if admitted != 1 {
			t.Fatalf("global budget: admitted %d", admitted)
		}
	}
	if len(seen) != 4 || m.Stats().Pending != 0 {
		t.Fatal("class session rotation did not converge")
	}
}
