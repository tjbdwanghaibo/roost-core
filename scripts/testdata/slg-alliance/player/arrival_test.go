package alliance

import (
	"context"
	"encoding/json"
	"errors"
	sender "example.com/slg-alliance-practice/game/handler/syncsender"
	"example.com/slg-alliance-practice/game/scout"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
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
	freezeBusinessTime(now)
	t.Cleanup(func() { clock.SetSource(nil) })
	if mode == "arrival-write" {
		if event.Delivered || state.ReportTotal != 0 || len(state.Reports) != 0 {
			t.Fatal("arrival handled before its worker ran")
		}
		now--
		freezeBusinessTime(now)
		due, err := s.Sync_AllianceDueArrivals(ctx, n.mapID, 1)
		check(t, err)
		if len(due) != 0 {
			t.Fatal("arrival discovered early")
		}
		if _, err = s.Sync_AllianceCompleteArrival(ctx, n.mapID, "march", false); err == nil {
			t.Fatal("arrival consumed early")
		}
		now++
		freezeBusinessTime(now) // 精确 End，首次可处理。
		due, err = s.Sync_AllianceDueArrivals(ctx, n.mapID, 1)
		check(t, err)
		if !slices.Equal(due, []string{"march"}) {
			t.Fatal("due event not rediscovered", due)
		}
		if _, err = s.Sync_AllianceDueArrivals(ctx, n.mapID, 0); err == nil {
			t.Fatal("unbounded discovery accepted")
		}
		if _, err = s.Sync_AllianceCompleteArrival(ctx, n.mapID, "unknown", false); err == nil {
			t.Fatal("forged event accepted")
		}
		if _, err = s.Sync_AllianceCompleteArrival(ctx, n.mapID, "march", true); err == nil {
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
		verifyArrivalDrain(t, n)
		verifyArrivalWorker(t, n)
		// 模拟重复投递；所有结果经正式 Nest Sender，报告与完成位不分两次写。
		var wg sync.WaitGroup
		results := make(chan bool, 8)
		errors := make(chan error, 8)
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				done, err := s.Sync_AllianceCompleteArrival(ctx, n.mapID, due[0], false)
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
		if count != 0 {
			t.Fatal("worker-completed arrival applied again", count)
		}
		raw, err = s.Sync_AllianceMapSnapshot(ctx, n.mapID)
		check(t, err)
		stats := n.projector.Stats()
		if stats.WALUnacked != 1 || stats.Projected != baseline.Projected {
			t.Fatal("expected one unprojected arrival transaction", stats)
		}
	} else if mode == "arrival-recover" || mode == "arrival-verify" {
		now += 60_000
		freezeBusinessTime(now) // 延迟重投不得改变报告中的原定到达时间。
		done, err := s.Sync_AllianceCompleteArrival(ctx, n.mapID, "march", false)
		check(t, err)
		if done {
			t.Fatal("cold arrival duplicated its effect")
		}
		after, err := s.Sync_AllianceMapSnapshot(ctx, n.mapID)
		check(t, err)
		if after != raw {
			t.Fatal("cold duplicate rewrote arrival")
		}
		verifyArrivalApp(t, n)
	} else {
		t.Fatal("unknown arrival mode", mode)
	}
	check(t, json.Unmarshal([]byte(raw), &state))
	report := state.Reports["march"]
	if state.ReportTotal != 1 || len(state.Reports) != 1 || !state.Arrivals["march"].Delivered || report.Target != 503 || report.At != event.Route.End || report.Destination != event.Route.To {
		t.Fatal("arrival effect inconsistent", state)
	}
	due, err := s.Sync_AllianceDueArrivals(ctx, n.mapID, 128)
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

func arrivalWorker(n *rewardRuntime) ArrivalWorker {
	s := sender.NewAllianceRewardsSender(n.nest)
	return ArrivalWorker{Limit: 1, Discover: func(ctx context.Context, limit int) ([]string, error) {
		return s.Sync_AllianceDueArrivals(ctx, n.mapID, limit)
	}, Consume: func(ctx context.Context, key string) error {
		_, err := s.Sync_AllianceCompleteArrival(ctx, n.mapID, key, false)
		return err
	}}
}

func verifyArrivalWorker(t *testing.T, n *rewardRuntime) {
	t.Helper()
	worker := arrivalWorker(n)
	originalDiscover := worker.Discover
	lostReply := errors.New("injected lost completion response")
	cycles, applied := 0, 0
	worker.Discover = func(ctx context.Context, limit int) ([]string, error) {
		if cycles == 0 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return originalDiscover(ctx, limit)
	}
	worker.Consume = func(ctx context.Context, key string) error {
		done, err := sender.NewAllianceRewardsSender(n.nest).Sync_AllianceCompleteArrival(ctx, n.mapID, key, false)
		if err != nil {
			return err
		}
		if done {
			applied++
			return lostReply
		}
		return nil
	}
	service := &ArrivalService{VisionService: VisionService{Interval: 20 * time.Millisecond, ReadTimeout: 100 * time.Millisecond}, Worker: worker}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	service.Observe = func(err error) {
		cycles++
		switch cycles {
		case 1:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("read failure=%v", err)
			}
		case 2:
			if !errors.Is(err, lostReply) {
				t.Fatalf("lost reply=%v", err)
			}
		case 3:
			check(t, err)
			cancel()
		default:
			t.Fatal("arrival worker failed to stop")
		}
	}
	check(t, service.Init(nil))
	check(t, service.Serve(ctx))
	check(t, service.Shutdown(context.Background()))
	if cycles != 3 || applied != 1 {
		t.Fatalf("worker cycles=%d applied=%d", cycles, applied)
	}
	// 无效/超量/重复/迟到的发现结果不得进入消费，取消轮次不发请求。
	for _, tc := range []struct {
		limit int
		keys  []string
	}{{1, []string{"a", "b"}}, {1, []string{""}}, {2, []string{"a", "a"}}} {
		bad := ArrivalWorker{Limit: tc.limit, Discover: func(context.Context, int) ([]string, error) { return tc.keys, nil }, Consume: func(context.Context, string) error { t.Fatal("invalid discovery consumed"); return nil }}
		if bad.Tick(context.Background()) == nil {
			t.Fatal("invalid discovery accepted")
		}
	}
	lateCtx, stop := context.WithCancel(context.Background())
	late := ArrivalWorker{Limit: 1, Discover: func(context.Context, int) ([]string, error) { stop(); return []string{"march"}, nil }, Consume: func(context.Context, string) error { t.Fatal("late discovery consumed"); return nil }}
	if !errors.Is(late.Tick(lateCtx), context.Canceled) {
		t.Fatal("late discovery not cancelled")
	}
}

func verifyArrivalApp(t *testing.T, n *rewardRuntime) {
	t.Helper()
	worker := arrivalWorker(n)
	worker.Consume = func(context.Context, string) error { return errors.New("restarted worker found a completed arrival") }
	service := &ArrivalService{VisionService: VisionService{Interval: time.Second, ReadTimeout: time.Second}, Worker: worker}
	var tickErr error
	service.Observe = func(err error) { tickErr = err; service.cancel() }
	path := filepath.Join(os.Getenv("ROOST_SLG_REWARD_WAL"), "arrival-app.yaml")
	check(t, os.WriteFile(path, []byte("log:\n  file: false\n  stdout: false\nsingleton:\n  enabled: false\n"), 0600))
	a := app.New("slg-arrival-practice", "0.0.0").RegisterServer("game", service)
	a.RootCmd().SetArgs([]string{"game", "--config", path, "--sid", os.Getenv("SLG_REWARD_SID")})
	check(t, a.Execute())
	check(t, tickErr)
	select {
	case <-service.done:
	default:
		t.Fatal("App returned before arrival worker drained")
	}
}

func verifyArrivalDrain(t *testing.T, n *rewardRuntime) {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	worker := arrivalWorker(n)
	worker.Consume = func(ctx context.Context, _ string) error { close(entered); <-release; return ctx.Err() }
	service := &ArrivalService{VisionService: VisionService{Interval: time.Second, ReadTimeout: time.Second}, Worker: worker}
	check(t, service.Init(nil))
	done := make(chan error, 1)
	go func() { done <- service.Serve(context.Background()) }()
	var once sync.Once
	defer once.Do(func() { close(release) })
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("arrival consume did not start")
	}
	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err := service.Shutdown(short)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("arrival falsely drained: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("arrival in flight lost: %v", err)
	default:
	}
	once.Do(func() { close(release) })
	wait, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	check(t, service.Shutdown(wait))
	select {
	case err := <-done:
		check(t, err)
	case <-wait.Done():
		t.Fatal("arrival did not drain")
	}
	check(t, service.Shutdown(wait))
	keys, err := arrivalWorker(n).Discover(wait, 1)
	check(t, err)
	if !slices.Equal(keys, []string{"march"}) {
		t.Fatal("cancelled consumption lost durable pending event", keys)
	}
}
