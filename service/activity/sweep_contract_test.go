package activity

import (
	"context"
	"errors"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/servicemetrics"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
	"testing"
	"time"
)

type failingWindows struct {
	versionstore.Store[string, Window]
	err error
}

func (w failingWindows) Get(context.Context, string) (versionstore.Versioned[Window], bool, error) {
	return versionstore.Versioned[Window]{}, false, w.err
}

func TestSweepFailuresAreCountedNotJustLogged(t *testing.T) {
	recorder := servicemetrics.NewRecorder()
	wire := errors.New("windows store: connection reset")
	service, _ := newActivityService(t, func(cfg *Config) {
		cfg.Windows = failingWindows{Store: cfg.Windows, err: wire}
		cfg.Metrics = recorder
	})
	server := &Server{service: service}
	ctx := context.Background()
	server.sweepGroup(ctx, service, "group-a")
	server.sweepGroup(ctx, service, "group-a")
	if got := recorder.Count("dropped:sweep.advance_failed"); got != 2 {
		t.Fatalf("sweep.advance_failed counted %d times after two failing sweeps, want 2 (events: %s)", got, recorder.Events())
	}

	// 健康的 sweep 不计失败。
	healthy, _ := newActivityService(t, func(cfg *Config) { cfg.Metrics = recorder })
	(&Server{service: healthy}).sweepGroup(ctx, healthy, "group-a")
	if got := recorder.Count("dropped:sweep.advance_failed"); got != 2 {
		t.Fatalf("a healthy sweep changed the failure count to %d", got)
	}
}

func TestTheSweepLoopAdvancesTheConfiguredGroups(t *testing.T) {
	previous := sweepEvery
	sweepEvery = 5 * time.Millisecond
	t.Cleanup(func() { sweepEvery = previous })

	service, c := newActivityService(t, func(cfg *Config) { cfg.SweepGroups = []string{"group-a"} })
	key := activityKey("act-loop")
	openActivity(t, service, key, 1, 2)
	notify(t, service, key, 1) // deadline = now + 30s grace
	c.advance(31 * time.Second)

	server := &Server{service: service}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		activity, found, err := service.LookupActivity(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if found && activity.Status == StatusComplete {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sweep loop never advanced the lapsed activity in a configured group: status=%v", activity.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTheSweepLoopWithoutGroupsSweepsNothingAndStopsCleanly(t *testing.T) {
	previous := sweepEvery
	sweepEvery = 5 * time.Millisecond
	t.Cleanup(func() { sweepEvery = previous })

	service, c := newActivityService(t)
	key := activityKey("act-idle")
	openActivity(t, service, key, 1, 2)
	notify(t, service, key, 1)
	c.advance(31 * time.Second)

	server := &Server{service: service}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if activity, _, _ := service.LookupActivity(context.Background(), key); activity.Status != StatusCollecting {
		t.Fatalf("a process with no configured groups advanced an activity: %v", activity.Status)
	}
}
