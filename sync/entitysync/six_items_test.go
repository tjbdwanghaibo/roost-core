package entitysync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/frame"
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
