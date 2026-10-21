package alliance

import (
	"context"
	"encoding/json"
	sender "example.com/slg-alliance-practice/game/handler/syncsender"
	"example.com/slg-alliance-practice/game/scout"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/infra/base/clock"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// 第一组进程证明在途恢复；第二组在本地报告已经 durable、响应可能丢失时再 SIGKILL。
func runArrivalChild(t *testing.T, n *rewardRuntime, mode string) {
	t.Helper()
	ctx := context.Background()
	s := sender.NewAllianceRewardsSender(n.nest)
	root := os.Getenv("ROOST_SLG_REWARD_WAL")
	raw, err := s.Sync_AllianceMapSnapshot(ctx, n.mapID)
	check(t, err)
	marker := "map-ready.json"
	if mode != "arrival-write" {
		marker = "arrival-ready.json"
	}
	expected, err := os.ReadFile(filepath.Join(root, marker))
	check(t, err)
	if raw != string(expected) {
		t.Fatal("arrival cold facts differ BEFORE retry")
	}
	wallet, err := s.Sync_AllianceWallet(ctx, n.id)
	check(t, err)
	var state struct {
		Routes      map[int64]scout.Route
		ReportTotal uint64
		Reports     map[string]struct {
			Target, At  int64
			Destination spatial.Point
		}
		Arrivals map[string]struct {
			Target    int64
			Route     scout.Route
			Delivered bool
		}
	}
	check(t, json.Unmarshal([]byte(raw), &state))
	event, ok := state.Arrivals["march"]
	if !ok || event.Target != 503 || event.Route != state.Routes[503] {
		t.Fatal("durable arrival obligation missing")
	}
	now := event.Route.End
	business := clock.BusinessFunc(func() time.Time { return time.UnixMilli(now) })
	if mode == "arrival-write" {
		if event.Delivered || state.ReportTotal != 0 || len(state.Reports) != 0 {
			t.Fatal("arrival handled before its worker ran")
		}
		now--
		due, err := s.Sync_AllianceDueArrivals(ctx, n.mapID, business, 1)
		check(t, err)
		if len(due) != 0 {
			t.Fatal("arrival discovered early")
		}
		if _, err = s.Sync_AllianceCompleteArrival(ctx, n.mapID, "march", business, false); err == nil {
			t.Fatal("arrival consumed early")
		}
		now++ // 精确 End，首次可处理。
		due, err = s.Sync_AllianceDueArrivals(ctx, n.mapID, business, 1)
		check(t, err)
		if !slices.Equal(due, []string{"march"}) {
			t.Fatal("due event not rediscovered", due)
		}
		if _, err = s.Sync_AllianceDueArrivals(ctx, n.mapID, business, 0); err == nil {
			t.Fatal("unbounded discovery accepted")
		}
		if _, err = s.Sync_AllianceCompleteArrival(ctx, n.mapID, "unknown", business, false); err == nil {
			t.Fatal("forged event accepted")
		}
		if _, err = s.Sync_AllianceCompleteArrival(ctx, n.mapID, "march", business, true); err == nil {
			t.Fatal("arrival rollback accepted")
		}
		after, err := s.Sync_AllianceMapSnapshot(ctx, n.mapID)
		check(t, err)
		if after != raw {
			t.Fatal("arrival rollback or refusal changed facts")
		}
		baseline := n.projector.Stats()
		if baseline.WALUnacked != 0 {
			t.Fatal("old scenario was not drained", baseline)
		}
		// 模拟重复投递；所有结果经正式 Nest Sender，报告与完成位不分两次写。
		var wg sync.WaitGroup
		results := make(chan bool, 8)
		errors := make(chan error, 8)
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				done, err := s.Sync_AllianceCompleteArrival(ctx, n.mapID, due[0], business, false)
				results <- done
				errors <- err
			}()
		}
		wg.Wait()
		close(results)
		close(errors)
		for err := range errors {
			check(t, err)
		}
		count := 0
		for done := range results {
			if done {
				count++
			}
		}
		if count != 1 {
			t.Fatal("arrival applied repeatedly", count)
		}
		raw, err = s.Sync_AllianceMapSnapshot(ctx, n.mapID)
		check(t, err)
		stats := n.projector.Stats()
		if stats.WALUnacked != 1 || stats.Projected != baseline.Projected {
			t.Fatal("expected one unprojected arrival transaction", stats)
		}
	} else if mode == "arrival-recover" || mode == "arrival-verify" {
		now += 60_000 // 延迟重投不得改变报告中的原定到达时间。
		done, err := s.Sync_AllianceCompleteArrival(ctx, n.mapID, "march", business, false)
		check(t, err)
		if done {
			t.Fatal("cold arrival duplicated its effect")
		}
		after, err := s.Sync_AllianceMapSnapshot(ctx, n.mapID)
		check(t, err)
		if after != raw {
			t.Fatal("cold duplicate rewrote arrival")
		}
	} else {
		t.Fatal("unknown arrival mode", mode)
	}
	check(t, json.Unmarshal([]byte(raw), &state))
	report := state.Reports["march"]
	if state.ReportTotal != 1 || len(state.Reports) != 1 || !state.Arrivals["march"].Delivered || report.Target != 503 || report.At != event.Route.End || report.Destination != event.Route.To {
		t.Fatal("arrival effect inconsistent", state)
	}
	due, err := s.Sync_AllianceDueArrivals(ctx, n.mapID, business, 128)
	check(t, err)
	if len(due) != 0 {
		t.Fatal("completed event remains pending", due)
	}
	afterWallet, err := s.Sync_AllianceWallet(ctx, n.id)
	check(t, err)
	if afterWallet != wallet {
		t.Fatal("arrival report changed Player rewards")
	}
	if mode == "arrival-write" {
		temp := filepath.Join(root, "arrival-ready.json.tmp")
		check(t, os.WriteFile(temp, []byte(raw), 0600))
		check(t, os.Rename(temp, filepath.Join(root, "arrival-ready.json")))
		select {} // 父进程看到完整快照标记后杀掉自己的子进程。
	}
	t.Log(fmt.Sprintf("%s: one durable report, no pending event, unchanged wallet", mode))
}
