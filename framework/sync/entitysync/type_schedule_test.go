package entitysync

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
)

func cadenceSubject(id int64, kind uint32, value *byte, packs *int) *entity.SubjectSyncState {
	pack := func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
		*packs++
		return entity.CopyFrozenSyncPayload(1, []byte{*value}), nil
	}
	return entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{
		Enabled: true, SubjectID: id, SubjectKind: kind, Namespace: "shared",
		Packer: entity.SubjectSyncPackFunc{Snapshot: pack, Delta: func(p entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) { return pack(p) }},
	})
}

func cadenceManager(t *testing.T) (*Manager, *recordingTransport, *time.Time) {
	t.Helper()
	tr := newRecordingTransport()
	m := newTestManager(t, tr, ManagerConfig{Mode: ModeOnChange, TypeSchedules: map[uint32]TypeSchedule{2: {Mode: ModePeriodic, Interval: time.Second}}})
	now := time.Unix(1800000000, 0)
	m.cadenceClock = func() time.Time { return now }
	return m, tr, &now
}

func TestTypeScheduleMixedTypesMergeWithoutSliding(t *testing.T) {
	m, tr, now := cadenceManager(t)
	var playerValue, terrainValue byte
	playerPacks, terrainPacks := 0, 0
	player := cadenceSubject(101, 1, &playerValue, &playerPacks)
	terrain := cadenceSubject(102, 2, &terrainValue, &terrainPacks)
	for _, s := range []*entity.SubjectSyncState{player, terrain} {
		if err := m.Register(s); err != nil {
			t.Fatal(err)
		}
	}
	open(t, m, 1)
	mustSubscribe(t, m, 1, 101, entity.SyncProfile{})
	mustSubscribe(t, m, 1, 102, entity.SyncProfile{})
	mustFlush(t, m)
	tr.take(1)
	playerPacks, terrainPacks = 0, 0
	terrainValue = 1
	terrain.MarkDirty(1)
	if err := m.CaptureSync(terrain); err != nil {
		t.Fatal(err)
	}
	if terrainPacks != 0 {
		t.Fatal("periodic type was frozen on commit")
	}
	playerValue = 1
	player.MarkDirty(1)
	if err := m.CaptureSync(player); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, m)
	got := oneFrame(t, tr, 1)
	if _, ok := got.updates[101]; !ok {
		t.Fatal("player delayed")
	}
	if _, ok := got.updates[102]; ok {
		t.Fatal("terrain sent early")
	}
	*now = now.Add(999 * time.Millisecond)
	terrainValue = 2
	terrain.MarkDirty(1)
	mustFlush(t, m)
	if len(tr.take(1)) != 0 || terrainPacks != 0 {
		t.Fatal("terrain captured before deadline")
	}
	*now = now.Add(time.Millisecond)
	mustFlush(t, m)
	got = oneFrame(t, tr, 1)
	if u := got.updates[102]; u.Payload.BytesCopy()[0] != 2 || u.Full {
		t.Fatalf("not a merged delta: %+v", u)
	}
	if terrainPacks != 1 || terrain.PendingDirty() {
		t.Fatal("did not consume one merged capture")
	}
	*now = now.Add(10 * time.Second)
	mustFlush(t, m)
	if len(tr.take(1)) != 0 || terrainPacks != 1 {
		t.Fatal("clean terrain polled/encoded")
	}
}

func TestTypeScheduleJoiningSnapshotDoesNotConsumeOldObserverChanges(t *testing.T) {
	m, tr, now := cadenceManager(t)
	var value byte
	packs := 0
	s := cadenceSubject(102, 2, &value, &packs)
	if err := m.Register(s); err != nil {
		t.Fatal(err)
	}
	open(t, m, 1)
	mustSubscribe(t, m, 1, 102, entity.SyncProfile{})
	mustFlush(t, m)
	tr.take(1)
	value = 1
	s.MarkDirty(1)
	*now = now.Add(500 * time.Millisecond)
	open(t, m, 2)
	mustSubscribe(t, m, 2, 102, entity.SyncProfile{})
	mustFlush(t, m)
	join := oneFrame(t, tr, 2).updates[102]
	if join.Payload.BytesCopy()[0] != 1 || !join.Full || !s.PendingDirty() || len(tr.take(1)) != 0 {
		t.Fatal("joining snapshot consumed or sent old observer changes")
	}
	value = 2
	s.MarkDirty(1)
	*now = now.Add(500 * time.Millisecond)
	mustFlush(t, m)
	for _, sid := range []SessionID{1, 2} {
		u := oneFrame(t, tr, sid).updates[102]
		if !u.Full || u.Version <= join.Version || u.Payload.BytesCopy()[0] != 2 {
			t.Fatalf("session %d: %+v", sid, u)
		}
	}
}

func TestTypeScheduleRemoveAndDrainBypassDelay(t *testing.T) {
	m, tr, _ := cadenceManager(t)
	var value byte
	packs := 0
	s := cadenceSubject(102, 2, &value, &packs)
	if err := m.Register(s); err != nil {
		t.Fatal(err)
	}
	open(t, m, 1, 2)
	for _, sid := range []SessionID{1, 2} {
		mustSubscribe(t, m, sid, 102, entity.SyncProfile{})
	}
	mustFlush(t, m)
	tr.take(1)
	tr.take(2)
	value = 1
	s.MarkDirty(1)
	if err := m.Unsubscribe(1, 102); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, m)
	if got := oneFrame(t, tr, 1); len(got.wire.Objects) != 1 || got.wire.Objects[0].Operation != frame.ObjectRemove {
		t.Fatal("remove delayed")
	}
	if len(tr.take(2)) != 0 {
		t.Fatal("live delta sent early")
	}
	if err := m.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := oneFrame(t, tr, 2); got.updates[102].Payload.BytesCopy()[0] != 1 {
		t.Fatal("drain lost dirty state")
	}
}

func TestTypeScheduleDurabilityAndRetryRetainDueWork(t *testing.T) {
	m, tr, now := cadenceManager(t)
	watermark := uint64(0)
	m.config.DurableWatermark = func() uint64 { return watermark }
	var value byte
	packs := 0
	s := cadenceSubject(102, 2, &value, &packs)
	if err := m.Register(s); err != nil {
		t.Fatal(err)
	}
	open(t, m, 1)
	mustSubscribe(t, m, 1, 102, entity.SyncProfile{})
	mustFlush(t, m)
	tr.take(1)
	value = 1
	s.MarkDirty(1)
	s.SetLastCommitLSN(7)
	*now = now.Add(time.Second)
	mustFlush(t, m)
	if len(tr.take(1)) != 0 || !s.PendingDirty() {
		t.Fatal("durability gate bypassed")
	}
	watermark = 7
	tr.retry = ErrRetryLater
	if err := m.Flush(context.Background()); !errors.Is(err, ErrRetryLater) {
		t.Fatal(err)
	}
	if !s.PendingDirty() {
		t.Fatal("retry consumed pending change")
	}
	tr.retry = nil
	mustFlush(t, m)
	if got := oneFrame(t, tr, 1); got.updates[102].Payload.BytesCopy()[0] != 1 {
		t.Fatal("retry failed")
	}
}

func TestTypeScheduleConfigurationIsCopiedAndValidated(t *testing.T) {
	for _, rule := range []TypeSchedule{{Mode: 99}, {Mode: ModePeriodic}, {Mode: ModePeriodic, Interval: -1}, {Mode: ModeOnChange, Interval: time.Second}} {
		if _, err := NewManager(ManagerConfig{Transport: newRecordingTransport(), TypeSchedules: map[uint32]TypeSchedule{2: rule}}); err == nil {
			t.Fatalf("accepted %+v", rule)
		}
	}
	rules := map[uint32]TypeSchedule{2: {Mode: ModePeriodic, Interval: time.Second}}
	m := newTestManager(t, newRecordingTransport(), ManagerConfig{TypeSchedules: rules})
	rules[2] = TypeSchedule{Mode: ModeOnChange}
	if m.config.TypeSchedules[2].Interval != time.Second {
		t.Fatal("caller mutated configuration")
	}
}

// 使用真实提交门与 Manager 循环；虚拟单调时间避免真实 Sleep 的调度误差。
func TestTypeScheduleProducerLoopAndStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := newRecordingTransport()
		m := newTestManager(t, tr, ManagerConfig{Mode: ModeOnChange, Interval: 50 * time.Millisecond, TypeSchedules: map[uint32]TypeSchedule{2: {Mode: ModePeriodic, Interval: time.Second}}})
		var value byte
		var packs atomic.Int32
		pack := func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
			packs.Add(1)
			return entity.CopyFrozenSyncPayload(1, []byte{value}), nil
		}
		s := entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{Enabled: true, SubjectID: 102, SubjectKind: 2, Namespace: "terrain", Packer: entity.SubjectSyncPackFunc{Snapshot: pack, Delta: func(p entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) { return pack(p) }}})
		e := newBatchEntity(s)
		if err := m.Register(s); err != nil {
			t.Fatal(err)
		}
		open(t, m, 1)
		mustSubscribe(t, m, 1, 102, entity.SyncProfile{})
		mustFlush(t, m)
		tr.take(1)
		packs.Store(0)
		m.BindSyncProducer()
		if err := m.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		commitBatchChange(t, m, e, func() { value = 1 })
		synctest.Wait()
		if packs.Load() != 0 || len(tr.take(1)) != 0 {
			t.Fatal("periodic commit froze/sent data")
		}
		time.Sleep(999 * time.Millisecond)
		synctest.Wait()
		if packs.Load() != 0 || len(tr.take(1)) != 0 {
			t.Fatal("periodic producer sent early")
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if got := oneFrame(t, tr, 1); got.updates[102].Payload.BytesCopy()[0] != 1 {
			t.Fatal("timer did not deliver")
		}
		commitBatchChange(t, m, e, func() { value = 2 })
		synctest.Wait()
		if err := m.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := oneFrame(t, tr, 1); got.updates[102].Payload.BytesCopy()[0] != 2 {
			t.Fatal("stop did not force pending capture")
		}
	})
}

func TestTypeScheduleHeldSessionRecoversWithoutWaitingForDeadline(t *testing.T) {
	m, tr, now := cadenceManager(t)
	var value byte
	packs := 0
	s := cadenceSubject(102, 2, &value, &packs)
	if err := m.Register(s); err != nil {
		t.Fatal(err)
	}
	open(t, m, 1)
	mustSubscribe(t, m, 1, 102, entity.SyncProfile{})
	mustFlush(t, m)
	tr.take(1)
	if err := m.HoldSession(1); err != nil {
		t.Fatal(err)
	}
	value = 1
	s.MarkDirty(1)
	mustFlush(t, m)
	if len(tr.take(1)) != 0 {
		t.Fatal("held session received data")
	}
	if err := m.ReadySession(1); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, m)
	if u := oneFrame(t, tr, 1).updates[102]; !u.Full || u.Payload.BytesCopy()[0] != 1 {
		t.Fatal("ready snapshot waited for periodic deadline")
	}
	value = 2
	s.MarkDirty(1)
	*now = now.Add(time.Second)
	mustFlush(t, m)
	if u := oneFrame(t, tr, 1).updates[102]; u.Payload.BytesCopy()[0] != 2 {
		t.Fatal("post-recovery change was lost")
	}
}

// 周期窗口内唯一需要快照的会话被 Hold（入场后等客户端 Ready）时，本轮没有快照接收方：
// 不能每次 Flush 都重新打包快照、推进版本，也不能因此把老观察者到期的增量降成全量。
func TestTypeScheduleHeldSnapshotDoesNotRepackWhileWaiting(t *testing.T) {
	m, tr, now := cadenceManager(t)
	var value byte
	packs := 0
	s := cadenceSubject(102, 2, &value, &packs)
	if err := m.Register(s); err != nil {
		t.Fatal(err)
	}
	open(t, m, 1)
	mustSubscribe(t, m, 1, 102, entity.SyncProfile{})
	mustFlush(t, m)
	tr.take(1)
	if err := m.OpenHeldSession(2); err != nil {
		t.Fatal(err)
	}
	mustSubscribe(t, m, 2, 102, entity.SyncProfile{})
	packs = 0
	baseVersion := s.Version()
	value = 1
	s.MarkDirty(1)
	for range 5 {
		*now = now.Add(100 * time.Millisecond)
		mustFlush(t, m)
	}
	if packs != 0 || s.Version() != baseVersion || len(tr.take(1)) != 0 || len(tr.take(2)) != 0 {
		t.Fatalf("held-only snapshot while waiting: packs=%d version=%d (base %d); want 0 packs and no version change", packs, s.Version(), baseVersion)
	}
	*now = now.Add(500 * time.Millisecond)
	mustFlush(t, m)
	if u := oneFrame(t, tr, 1).updates[102]; u.Full || u.Payload.BytesCopy()[0] != 1 {
		t.Fatalf("due update for the live observer: %+v; want a delta carrying the change", u)
	}
	if err := m.ReadySession(2); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, m)
	if u := oneFrame(t, tr, 2).updates[102]; !u.Full || u.Payload.BytesCopy()[0] != 1 {
		t.Fatalf("ready session snapshot: %+v", u)
	}
}
