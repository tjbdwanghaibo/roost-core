package activity

// B9（维护者决定，2026-10-06）：读取已存窗口条目只走一个入口。
//
// 背景：窗口条目校验先后在相邻的循环里漏了四次——RR-20260914-02 → RR-20261001-09 → 其残余 →
// NC-42 → NC-51，每次都是在某一个读窗口条目的位置补判断。NC-51 之后仍有两处没补（N06 S3 留项）：
//
//   - Delivering 列表：DeliveringActivities 原样交出别的组的键或不合法的键，sweep 对它们读活动、
//     读派发，再由 RetireDelivered 按键自身的 GroupID 去写别的组的窗口；本组的这条永远不会被移走。
//   - PendingActivities（RPC，对 game 公开）原样交出 Keys / Opening 里的坏键。
//   - RetireDelivered 对“键根本不在 Delivering”也回报 true（观察 4）。
//
// 承诺：Keys / Opening / Delivering 的读者都经同一个入口；坏条目（windowKeyProblem 非空）跳过、
// 保留给运维、按列表计数；RetireDelivered 只在本次确实把键移出 Delivering 时回报 true。

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/kit/service/servicemetrics"
	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// plantDelivering appends keys to group's Delivering list the way a stored
// record written by something other than this package would hold them.
func plantDelivering(t *testing.T, windows versionstore.Store[string, Window], group string, keys ...Key) {
	t.Helper()
	if _, _, err := windows.Update(context.Background(), group, func(current Window, found bool) (Window, bool, error) {
		next := current.clone()
		next.GroupID = group
		next.Delivering = append(next.Delivering, keys...)
		return next, true, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// ackAll acknowledges every dispatch of a complete activity.
func ackAll(t *testing.T, s *Service, key Key, sids ...int32) {
	t.Helper()
	for _, sid := range sids {
		d, found, err := s.LookupDispatch(context.Background(), key, sid)
		if err != nil || !found {
			t.Fatalf("dispatch %s/%d: found=%v err=%v", key, sid, found, err)
		}
		if _, err := s.AckDispatch(context.Background(), key, sid, d.Token); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDeliveringSkipsMalformedEntriesAndKeepsThem(t *testing.T) {
	recorder := servicemetrics.NewRecorder()
	var windows versionstore.Store[string, Window]
	s, _ := newActivityService(t, func(c *Config) { windows = c.Windows; c.Metrics = recorder })
	ctx := context.Background()

	// group-b's own activity: complete, every dispatch acked, still listed in
	// group-b's Delivering (group-b's sweep has not run yet).
	foreign := Key{GroupID: "group-b", ActivityID: "foreign", Phase: PhaseClose}
	openActivity(t, s, foreign, 7)
	notify(t, s, foreign, 7)
	ackAll(t, s, foreign, 7)
	// group-a's own activity, complete and still delivering.
	local := activityKey("local")
	openActivity(t, s, local, 1)
	notify(t, s, local, 1)
	invalid := Key{GroupID: "group-a", ActivityID: "", Phase: PhaseClose}
	plantDelivering(t, windows, "group-a", foreign, invalid)

	keys, err := s.DeliveringActivities(ctx, "group-a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != local {
		t.Fatalf("group-a's delivering list = %v, want only its own %s", keys, local)
	}
	if got := recorder.Count("dropped:sweep.delivering_key_malformed"); got != 2 {
		t.Fatalf("sweep.delivering_key_malformed counted %d, want 2 (%s)", got, recorder.Events())
	}

	before, _, err := windows.Get(ctx, "group-b")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{service: s}
	server.sweepGroup(ctx, s, "group-a")
	after, _, err := windows.Get(ctx, "group-b")
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != before.Version || !after.Value.delivering(foreign) {
		t.Fatalf("group-a's sweep wrote group-b's window: before %+v after %+v", before, after)
	}
	window, _, err := windows.Get(ctx, "group-a")
	if err != nil {
		t.Fatal(err)
	}
	if !window.Value.delivering(foreign) || !window.Value.delivering(invalid) {
		t.Fatalf("a malformed delivering entry was removed instead of kept for an operator: %+v", window.Value)
	}

	// group-b's own sweep still retires it.
	server.sweepGroup(ctx, s, "group-b")
	if keys, _ := s.DeliveringActivities(ctx, "group-b", 10); len(keys) != 0 {
		t.Fatalf("group-b's own sweep did not retire its acked activity: %v", keys)
	}
}

func TestPendingActivitiesSkipsMalformedEntries(t *testing.T) {
	var windows versionstore.Store[string, Window]
	s, _ := newActivityService(t, func(c *Config) { windows = c.Windows })
	ctx := context.Background()

	local := activityKey("local")
	openActivity(t, s, local, 1, 2)
	foreign := Key{GroupID: "group-b", ActivityID: "foreign", Phase: PhaseClose}
	plantConfirmedKeys(t, windows, "group-a", foreign, Key{GroupID: "group-a", Phase: PhaseClose})
	seedOpening(t, s, OpeningEntry{Key: Key{GroupID: "group-a", ActivityID: "bad/id", Phase: PhaseClose}})

	keys, err := s.PendingActivities(ctx, "group-a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != local {
		t.Fatalf("PendingActivities(group-a) = %v, want only %s: a game must not be handed another group's or an invalid key", keys, local)
	}
}

func TestRetireDeliveredReportsOnlyWhatItRemoved(t *testing.T) {
	s, _ := newActivityService(t)
	ctx := context.Background()

	k := activityKey("acked")
	openActivity(t, s, k, 1)
	notify(t, s, k, 1)
	ackAll(t, s, k, 1)

	retired, err := s.RetireDelivered(ctx, k)
	if err != nil || !retired {
		t.Fatalf("first retire = %v, %v; want true", retired, err)
	}
	retired, err = s.RetireDelivered(ctx, k)
	if err != nil || retired {
		t.Fatalf("retiring a key that is no longer listed = %v, %v; want false", retired, err)
	}
	never := activityKey("never-opened")
	if retired, err := s.RetireDelivered(ctx, never); err != nil || retired {
		t.Fatalf("retiring a key that was never listed = %v, %v; want false", retired, err)
	}
}

// The operator side of B9: the malformed entries the sweep skips can be
// listed and removed by the owning process, one at a time, with a note — and
// only malformed ones.
func TestOperatorRemovesOnlyMalformedWindowEntries(t *testing.T) {
	recorder := servicemetrics.NewRecorder()
	var windows versionstore.Store[string, Window]
	s, clock := newActivityService(t, func(c *Config) { windows = c.Windows; c.Metrics = recorder })
	ctx := context.Background()
	logs := captureSweepLogs(t, "delivering window key is malformed", "delivering window key is usable again")

	local := activityKey("local")
	openActivity(t, s, local, 1, 2)
	done := activityKey("done")
	openActivity(t, s, done, 1)
	notify(t, s, done, 1) // complete, delivering
	foreign := Key{GroupID: "group-b", ActivityID: "foreign", Phase: PhaseClose}
	invalid := Key{GroupID: "group-a", ActivityID: "", Phase: PhaseClose}
	badPlan := activityKey("bad-plan")
	plantConfirmedKeys(t, windows, "group-a", foreign, foreign)
	plantDelivering(t, windows, "group-a", invalid)
	seedOpening(t, s, OpeningEntry{Key: badPlan, AdmittedAtUnix: clock.Now().Unix(), Intent: malformedIntent(badPlan)})

	listed, err := s.MalformedWindowEntries(ctx, "group-a")
	if err != nil {
		t.Fatal(err)
	}
	want := map[malformedID]bool{{WindowKeys, foreign}: true, {WindowDelivering, invalid}: true, {WindowOpening, badPlan}: true}
	got := map[malformedID]bool{}
	for _, entry := range listed {
		if entry.Reason == "" {
			t.Errorf("entry %+v has no reason", entry)
		}
		got[malformedID{entry.List, entry.Key}] = true
	}
	if len(got) != len(want) {
		t.Fatalf("MalformedWindowEntries = %+v, want exactly %v", listed, want)
	}
	for id := range want {
		if !got[id] {
			t.Fatalf("MalformedWindowEntries = %+v, missing %+v", listed, id)
		}
	}
	if _, err := s.DeliveringActivities(ctx, "group-a", 10); err != nil { // the sweep's tick sees it
		t.Fatal(err)
	}

	// Refusals: no note, unknown list, healthy entries, absent entries.
	if _, err := s.RemoveMalformedWindowEntry(ctx, "group-a", WindowKeys, foreign, " "); !errors.Is(err, ErrAdminNoteRequired) {
		t.Fatalf("no note: %v, want ErrAdminNoteRequired", err)
	}
	if _, err := s.RemoveMalformedWindowEntry(ctx, "group-a", "pending", foreign, "x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown list: %v, want ErrInvalid", err)
	}
	for _, tc := range []struct {
		list WindowList
		key  Key
	}{{WindowKeys, local}, {WindowDelivering, done}} {
		if _, err := s.RemoveMalformedWindowEntry(ctx, "group-a", tc.list, tc.key, "cleanup"); !errors.Is(err, ErrStatus) {
			t.Fatalf("removing the healthy %s entry %s: %v, want ErrStatus", tc.list, tc.key, err)
		}
	}
	if _, err := s.RemoveMalformedWindowEntry(ctx, "group-a", WindowOpening, foreign, "cleanup"); !errors.Is(err, ErrMissing) {
		t.Fatalf("an entry not in that list: %v, want ErrMissing", err)
	}
	if _, err := s.RemoveMalformedWindowEntry(ctx, "group-z", WindowKeys, foreign, "cleanup"); !errors.Is(err, ErrMissing) {
		t.Fatalf("a group with no window: %v, want ErrMissing", err)
	}
	if w := windowOf(t, s, "group-a"); !containsKey(w.Keys, local) || !w.delivering(done) || w.AdminNote != "" {
		t.Fatalf("a refused repair changed the window: %+v", w)
	}

	// The repairs.
	for _, tc := range []struct {
		list WindowList
		key  Key
		want int
	}{{WindowKeys, foreign, 2}, {WindowDelivering, invalid, 1}, {WindowOpening, badPlan, 1}} {
		removed, err := s.RemoveMalformedWindowEntry(ctx, "group-a", tc.list, tc.key, "INC-42: written by a broken migration")
		if err != nil || removed != tc.want {
			t.Fatalf("remove %s %s = %d, %v; want %d", tc.list, tc.key, removed, err, tc.want)
		}
	}
	w := windowOf(t, s, "group-a")
	if containsKey(w.Keys, foreign) || w.delivering(invalid) || w.openingIndex(badPlan) >= 0 {
		t.Fatalf("a repaired entry is still stored: %+v", w)
	}
	if !containsKey(w.Keys, local) || !w.delivering(done) {
		t.Fatalf("a repair removed a healthy entry: %+v", w)
	}
	if w.AdminNote != "INC-42: written by a broken migration" || w.AdminActionAtUnix != clock.Now().Unix() {
		t.Fatalf("the repair note was not recorded on the window: %+v", w)
	}
	if listed, err := s.MalformedWindowEntries(ctx, "group-a"); err != nil || len(listed) != 0 {
		t.Fatalf("after the repairs MalformedWindowEntries = %+v, %v", listed, err)
	}
	if got := recorder.Count("accepted:admin.remove_malformed_window_entry"); got != 3 {
		t.Fatalf("repairs counted %d, want 3 (%s)", got, recorder.Events())
	}

	// The next tick sees the list clean: no further count, one "gone" line.
	if _, err := s.DeliveringActivities(ctx, "group-a", 10); err != nil {
		t.Fatal(err)
	}
	if got := recorder.Count("dropped:sweep.delivering_key_malformed"); got != 1 {
		t.Fatalf("sweep.delivering_key_malformed counted %d after the repair, want only the one before it", got)
	}
	if logs.count("delivering window key is malformed") != 1 || logs.count("delivering window key is usable again") != 1 {
		t.Fatalf("delivering logs: appeared %d, cleared %d; want one each",
			logs.count("delivering window key is malformed"), logs.count("delivering window key is usable again"))
	}
}
