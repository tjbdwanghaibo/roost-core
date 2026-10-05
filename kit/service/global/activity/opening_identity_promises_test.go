package activity

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/kit/service/servicemetrics"
	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// RR-20261005-NC-42 / RR-20261001-09 残余：持久 opening 是恢复意图，重投必须先
// 校验它，不能把坏计划写到 Activities；sweep 不能执行非法键或跨组计划。
// 坏记录只拒绝/跳过并保留，操作员修正后走正式入口恢复，不自动删生产记录。
func TestOpenActivityRejectsMalformedPersistedIntentBeforeCreate(t *testing.T) {
	for _, shape := range []string{"foreign_key", "collecting", "empty_expected"} {
		t.Run(shape, func(t *testing.T) {
			s, clock := newRR09Service(t)
			ctx := context.Background()
			key := activityKey("persisted-plan")
			intent := Activity{Key: key, Status: StatusPending, ExpectedGameSIDs: []int32{1}, OpenedAtUnix: clock.Now().Unix(), UpdatedAtUnix: clock.Now().Unix()}
			switch shape {
			case "foreign_key":
				intent.Key.ActivityID = "foreign"
			case "collecting":
				intent.Status = StatusCollecting
			case "empty_expected":
				intent.ExpectedGameSIDs = nil
			}
			seedOpening(t, s, OpeningEntry{Key: key, Intent: &intent, AdmittedAtUnix: clock.Now().Unix()})
			before, _, err := s.cfg.Windows.Get(ctx, key.GroupID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.OpenActivity(ctx, key, []int32{1}); !errors.Is(err, ErrConflict) {
				t.Errorf("malformed %s plan was accepted: %v", shape, err)
			}
			if _, found, err := s.cfg.Activities.Get(ctx, key); err != nil || found {
				t.Errorf("malformed %s plan wrote an activity: found=%v err=%v", shape, found, err)
			}
			after, _, err := s.cfg.Windows.Get(ctx, key.GroupID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Errorf("refusal changed opening window: %+v %v", after, err)
			}
			if t.Failed() {
				return
			}
			_, _, err = s.cfg.Windows.Update(ctx, key.GroupID, func(current Window, found bool) (Window, bool, error) {
				next := current.clone()
				next.Opening[0].Intent = &Activity{Key: key, Status: StatusPending, ExpectedGameSIDs: []int32{1}, OpenedAtUnix: clock.Now().Unix(), UpdatedAtUnix: clock.Now().Unix()}
				return next, true, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.OpenActivity(ctx, key, []int32{1})
			if err != nil || got.Key != key || got.Status != StatusPending {
				t.Fatalf("repaired plan did not recover: %+v %v", got, err)
			}
			if _, err := s.OpenActivity(ctx, key, []int32{1}); !errors.Is(err, ErrExists) {
				t.Fatalf("healthy duplicate: %v", err)
			}
		})
	}
}

func TestOpeningSweepRejectsInvalidOrForeignGroupBeforeSideEffects(t *testing.T) {
	for _, shape := range []string{"empty_activity", "slash_phase", "foreign_group"} {
		t.Run(shape, func(t *testing.T) {
			recorder := servicemetrics.NewRecorder()
			s, clock := newRR09Service(t, func(c *Config) { c.Metrics = recorder })
			ctx := context.Background()
			normal := activityKey("healthy")
			openActivity(t, s, normal, 1, 2)
			notify(t, s, normal, 1)
			bad := activityKey("bad")
			switch shape {
			case "empty_activity":
				bad.ActivityID = ""
			case "slash_phase":
				bad.Phase = "close/other"
			case "foreign_group":
				bad.GroupID = "foreign-group"
			}
			intent := Activity{Key: bad, Status: StatusPending, ExpectedGameSIDs: []int32{1}, OpenedAtUnix: clock.Now().Unix(), UpdatedAtUnix: clock.Now().Unix()}
			_, _, err := s.cfg.Windows.Update(ctx, normal.GroupID, func(current Window, found bool) (Window, bool, error) {
				next := current.clone()
				next.Opening = append(next.Opening, OpeningEntry{Key: bad, Intent: &intent, AdmittedAtUnix: clock.Now().Unix()})
				return next, true, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			clock.advance(s.cfg.OpeningGrace + time.Second)
			completed, err := s.AdvanceExpired(ctx, normal.GroupID, 10)
			if err != nil || len(completed) != 1 || completed[0].Key != normal {
				t.Errorf("bad %s plan stalled healthy group: %v %v", shape, completed, err)
			}
			if _, found, err := s.cfg.Activities.Get(ctx, bad); err != nil || found {
				t.Errorf("bad %s plan wrote an activity: found=%v err=%v", shape, found, err)
			}
			if windowOf(t, s, normal.GroupID).openingIndex(bad) < 0 {
				t.Error("malformed plan lost its reserved slot")
			}
			if recorder.Count("dropped:sweep.opening_intent_malformed") != 1 {
				t.Error("malformed plan was not reported")
			}
		})
	}
}

func TestOpeningRecoveryKeepsValidIntentAndLegacyAdmission(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "persisted", true: "legacy"}[legacy], func(t *testing.T) {
			s, clock := newRR09Service(t)
			key := activityKey("valid-recovery")
			entry := OpeningEntry{Key: key, AdmittedAtUnix: clock.Now().Unix()}
			if !legacy {
				entry.Intent = &Activity{Key: key, Status: StatusPending, ExpectedGameSIDs: []int32{1, 2}, OpenedAtUnix: clock.Now().Unix(), UpdatedAtUnix: clock.Now().Unix()}
			}
			seedOpening(t, s, entry)
			got, err := s.OpenActivity(context.Background(), key, []int32{3})
			want := []int32{1, 2}
			if legacy {
				want = []int32{3}
			}
			if err != nil || !reflect.DeepEqual(got.ExpectedGameSIDs, want) {
				t.Fatalf("durable/legacy recovery changed intent: %+v %v", got, err)
			}
			window := windowOf(t, s, key.GroupID)
			if window.openingIndex(key) >= 0 || !window.contains(key) || window.pending() != 1 {
				t.Fatalf("recovery did not confirm its one slot: %+v", window)
			}
		})
	}
}

func TestMalformedForeignOpeningDiagnosticsClearWithItsWindow(t *testing.T) {
	s, clock := newRR09Service(t)
	logs := captureSweepLogs(t, "opening intent is malformed", "opening intent is usable again")
	ctx := context.Background()
	group := "group-a"
	bad := Key{GroupID: "foreign", ActivityID: "bad", Phase: PhaseClose}
	intent := &Activity{Key: bad, Status: StatusPending, ExpectedGameSIDs: []int32{1}}
	if _, _, err := s.cfg.Windows.Create(ctx, group, Window{GroupID: group, Opening: []OpeningEntry{{Key: bad, Intent: intent, AdmittedAtUnix: clock.Now().Unix()}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceExpired(ctx, group, 10); err != nil {
		t.Fatal(err)
	}
	if logs.count("opening intent is malformed") != 1 {
		t.Fatal("foreign key diagnostic was not reported")
	}
	if _, _, err := s.cfg.Windows.Update(ctx, group, func(current Window, found bool) (Window, bool, error) {
		next := current.clone()
		next.Opening = nil
		return next, true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceExpired(ctx, group, 10); err != nil {
		t.Fatal(err)
	}
	if logs.count("opening intent is usable again") != 1 {
		t.Fatal("removed corrupt entry left its diagnostic active")
	}
}

type progressReconcileLedger struct {
	versionstore.Store[RequestKey, ProgressReservation]
	beforeMark func()
}

func (l *progressReconcileLedger) Update(ctx context.Context, key RequestKey, mutate versionstore.Mutate[ProgressReservation]) (versionstore.Versioned[ProgressReservation], bool, error) {
	if l.beforeMark != nil {
		hook := l.beforeMark
		l.beforeMark = nil
		hook()
	}
	return l.Store.Update(ctx, key, mutate)
}

// N06 admin 增量控制：读完 pending 后另一个请求成功写入，新证明不能被旧对账清掉。
func TestProgressReconcileRetainsAConcurrentNewProof(t *testing.T) {
	s, _ := newActivityService(t)
	ctx := context.Background()
	key := activityKey("reconcile-new-proof")
	openActivity(t, s, key, 1)
	healthy := s.cfg.Ledger
	s.cfg.Ledger = failedProgressMark{healthy}
	if _, err := s.ApplyProgress(ctx, key, "p", "old", ProgressDelta{Score: 1}); err == nil {
		t.Fatal("lost-mark fixture did not fail")
	}
	fired := false
	l := &progressReconcileLedger{Store: healthy}
	s.cfg.Ledger = l
	l.beforeMark = func() {
		fired = true
		// 创建第二个 Service 共享存储，避免修改执行中的对象或绕过正式 ApplyProgress。
		cfg := s.cfg
		cfg.Ledger = failedProgressMark{healthy}
		other, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := other.ApplyProgress(ctx, key, "p", "new", ProgressDelta{Score: 2}); err == nil {
			t.Fatal("new lost-mark fixture did not fail")
		}
	}
	got, err := s.ReconcileProgress(ctx, key, "p", "finish old mark")
	if !fired {
		t.Fatal("reconcile interleaving did not run")
	}
	if err != nil || got.Score != 3 || got.Applies != 2 || !reflect.DeepEqual(got.PendingRequestIDs, []string{"new"}) {
		t.Fatalf("old reconcile lost concurrent proof: %+v %v", got, err)
	}
	got, err = s.ReconcileProgress(ctx, key, "p", "finish new mark")
	if err != nil || len(got.PendingRequestIDs) != 0 || got.Score != 3 || got.Applies != 2 {
		t.Fatalf("second reconcile did not converge: %+v %v", got, err)
	}
}
