package activity

// RR-20261005-NC-51：AdvanceExpired 对窗口里“已确认”的 Keys 不做与 Opening 相同的
// 键合法 / 归属校验（RR-20261001-09 残余只补了 Opening）。
//
// 旧行为：group-a 窗口的 Keys 里混进 group-b 的键（存量坏记录、人工修复写错组）时，
// group-a 的 sweep 照常读、照常写：把 group-b 的活动按自己的时钟结算掉，并把这个键
// 并入 group-a 的 Delivering。之后 RetireDelivered 按键自身的 GroupID 去更新 group-b
// 的窗口、却回报 true，group-a 的 Delivering 永远留着它，每个 sweep 周期都对它做一轮
// 活动 / 派发读取与窗口写。
//
// 承诺：sweep 只处理键合法且属于本组的确认条目；坏条目跳过、保留给运维（不自动删）、
// 每 tick 计 dropped:sweep.window_key_malformed；它不能挡住本组其他活动的结算，也不能
// 写别的组的活动或窗口。

import (
	"context"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/kit/service/servicemetrics"
	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// plantConfirmedKeys appends keys to group's confirmed Keys the way a stored
// record written by something other than this package would hold them.
func plantConfirmedKeys(t *testing.T, windows versionstore.Store[string, Window], group string, keys ...Key) {
	t.Helper()
	if _, _, err := windows.Update(context.Background(), group, func(current Window, found bool) (Window, bool, error) {
		next := current.clone()
		next.GroupID = group
		next.Keys = append(next.Keys, keys...)
		return next, true, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSweepSkipsConfirmedKeysOfAnotherGroup(t *testing.T) {
	recorder := servicemetrics.NewRecorder()
	var windows versionstore.Store[string, Window]
	s, clock := newActivityService(t, func(c *Config) { windows = c.Windows; c.Metrics = recorder })
	ctx := context.Background()

	foreign := Key{GroupID: "group-b", ActivityID: "foreign", Phase: PhaseClose}
	openActivity(t, s, foreign, 7, 8)
	notify(t, s, foreign, 7) // collecting in its own group
	local := activityKey("local")
	openActivity(t, s, local, 1, 2)
	notify(t, s, local, 1)
	plantConfirmedKeys(t, windows, "group-a", foreign)
	clock.advance(time.Minute) // both grace windows lapse

	done, err := s.AdvanceExpired(ctx, "group-a", 10)
	if err != nil {
		t.Fatalf("a foreign confirmed key failed group-a's sweep: %v", err)
	}
	if len(done) != 1 || done[0].Key != local {
		t.Fatalf("group-a's sweep completed %+v, want only its own %s", done, local)
	}
	if got, _, err := s.LookupActivity(ctx, foreign); err != nil || got.Status != StatusCollecting {
		t.Fatalf("group-a's sweep wrote group-b's activity: %+v err=%v", got, err)
	}
	if _, found, err := s.LookupDispatch(ctx, foreign, 7); err != nil || found {
		t.Fatalf("group-a's sweep created group-b's dispatch: found=%v err=%v", found, err)
	}
	window, _, err := windows.Get(ctx, "group-a")
	if err != nil {
		t.Fatal(err)
	}
	if window.Value.delivering(foreign) {
		t.Fatalf("group-b's key joined group-a's Delivering: %+v", window.Value)
	}
	if !containsKey(window.Value.Keys, foreign) {
		t.Fatalf("the foreign entry was deleted instead of kept for an operator: %+v", window.Value)
	}
	if got := recorder.Count("dropped:sweep.window_key_malformed"); got != 1 {
		t.Fatalf("sweep.window_key_malformed counted %d, want 1 (%s)", got, recorder.Events())
	}

	// group-b's own sweep still finishes it, exactly once.
	if done, err := s.AdvanceExpired(ctx, "group-b", 10); err != nil || len(done) != 1 || done[0].Key != foreign {
		t.Fatalf("group-b's own sweep: %+v err=%v", done, err)
	}
}

func TestSweepSkipsInvalidConfirmedKeys(t *testing.T) {
	recorder := servicemetrics.NewRecorder()
	var windows versionstore.Store[string, Window]
	s, clock := newActivityService(t, func(c *Config) { windows = c.Windows; c.Metrics = recorder })
	ctx := context.Background()

	local := activityKey("local")
	openActivity(t, s, local, 1, 2)
	notify(t, s, local, 1)
	bad := Key{GroupID: "group-a", ActivityID: "", Phase: PhaseClose}
	plantConfirmedKeys(t, windows, "group-a", bad)
	clock.advance(time.Minute)

	for tick := 1; tick <= 2; tick++ {
		if _, err := s.AdvanceExpired(ctx, "group-a", 10); err != nil {
			t.Fatalf("tick %d: an invalid confirmed key failed the sweep: %v", tick, err)
		}
		window, _, err := windows.Get(ctx, "group-a")
		if err != nil {
			t.Fatal(err)
		}
		if !containsKey(window.Value.Keys, bad) || window.Value.delivering(bad) {
			t.Fatalf("tick %d: the invalid entry was acted on (read as an activity, pruned or moved) instead of skipped and kept: %+v", tick, window.Value)
		}
		if got := recorder.Count("dropped:sweep.window_key_malformed"); got != tick {
			t.Fatalf("tick %d: sweep.window_key_malformed counted %d (%s)", tick, got, recorder.Events())
		}
	}
	if got, _, err := s.LookupActivity(ctx, local); err != nil || got.Status != StatusComplete {
		t.Fatalf("the group's own activity did not complete: %+v err=%v", got, err)
	}
}

func containsKey(keys []Key, key Key) bool {
	for _, k := range keys {
		if k == key {
			return true
		}
	}
	return false
}
