package statslog

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/worker"

	"github.com/spf13/viper"
)

func TestStatsLogModWritesSeparateJSONLFile(t *testing.T) {
	dir := t.TempDir()
	cfg := viper.New()
	cfg.Set("server_type", "game")
	cfg.Set("sid", 7)
	cfg.Set("stats_log.enabled", true)
	cfg.Set("stats_log.dir", dir)
	cfg.Set("stats_log.filename", "game-7.stats.log")
	cfg.Set("stats_log.interval", "1h")

	mod := NewStatsLogMod()
	if err := mod.Init(cfg); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := mod.Provide(app.NewRegistry(cfg)); err != nil {
		t.Fatalf("Provide: %v", err)
	}
	if err := mod.FlushOnce(); err != nil {
		t.Fatalf("FlushOnce: %v", err)
	}
	mod.Stop()

	raw, err := os.ReadFile(filepath.Join(dir, "game-7.stats.log"))
	if err != nil {
		t.Fatalf("read stats log: %v", err)
	}
	var record StatsRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("stats log should be one JSON object line: %v\n%s", err, string(raw))
	}
	if record.Service != "game" || record.Sid != 7 {
		t.Fatalf("record identity mismatch: %+v", record)
	}
	if record.Runtime.Goroutines <= 0 || record.Runtime.NumCPU <= 0 {
		t.Fatalf("runtime stats missing: %+v", record.Runtime)
	}
	if _, err := time.Parse(time.RFC3339Nano, record.Timestamp); err != nil {
		t.Fatalf("timestamp should be RFC3339Nano text, got %q: %v", record.Timestamp, err)
	}
	if record.TimestampMs <= 0 {
		t.Fatalf("timestamp missing: %+v", record)
	}
	if record.Runtime.HeapAllocBytes == 0 || !strings.Contains(record.Runtime.HeapAlloc, "B") {
		t.Fatalf("heap alloc should expose bytes and human text: %+v", record.Runtime)
	}
}

func TestStatsLogModDisabledDoesNotCreateFile(t *testing.T) {
	dir := t.TempDir()
	cfg := viper.New()
	cfg.Set("server_type", "gate")
	cfg.Set("sid", 8)
	cfg.Set("stats_log.enabled", false)
	cfg.Set("stats_log.dir", dir)
	cfg.Set("stats_log.filename", "gate-8.stats.log")
	cfg.Set("stats_log.interval", time.Second)

	mod := NewStatsLogMod()
	if err := mod.Init(cfg); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := mod.FlushOnce(); err != nil {
		t.Fatalf("FlushOnce disabled should be no-op: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "gate-8.stats.log")); !os.IsNotExist(err) {
		t.Fatalf("disabled stats log should not create file, err=%v", err)
	}
}

func TestStatsLogProviderUnregisterDoesNotRemoveReplacement(t *testing.T) {
	mod := NewStatsLogMod()
	unregisterOld := mod.RegisterProvider("checkpoint", func() (any, error) {
		return "old", nil
	})
	unregisterNew := mod.RegisterProvider("checkpoint", func() (any, error) {
		return "new", nil
	})

	unregisterOld()
	providers := mod.collectProviders()
	if providers["checkpoint"] != "new" {
		t.Fatalf("provider after old unregister = %#v, want replacement to remain", providers)
	}

	unregisterNew()
	if providers := mod.collectProviders(); providers != nil {
		t.Fatalf("providers after new unregister = %#v, want nil", providers)
	}
}

func TestStatsLogProviderPanicIsCaptured(t *testing.T) {
	mod := NewStatsLogMod()
	mod.RegisterProvider("broken", func() (any, error) {
		panic("boom")
	})

	providers := mod.collectProviders()
	value, ok := providers["broken"].(map[string]any)
	if !ok {
		t.Fatalf("provider value = %#v, want error map", providers["broken"])
	}
	if value["error"] == "" {
		t.Fatalf("provider panic should be captured as error, got %#v", value)
	}
}

func TestStatsLogStopWithContextReturnsWhenFlushIsBlocked(t *testing.T) {
	cfg := viper.New()
	cfg.Set("stats_log.enabled", true)
	cfg.Set("stats_log.dir", t.TempDir())
	cfg.Set("stats_log.interval", time.Millisecond)

	mod := NewStatsLogMod()
	if err := mod.Init(cfg); err != nil {
		t.Fatalf("Init: %v", err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	mod.RegisterProvider("blocked", func() (any, error) {
		startedOnce.Do(func() { close(started) })
		<-release
		return "ok", nil
	})
	if err := mod.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stats provider was not called")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := mod.StopWithContext(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("StopWithContext err = %v, want context deadline", err)
	}

	// Let the blocked provider return, then wait for the shutdown the bounded
	// StopWithContext abandoned to actually finish. Left as a deferred
	// close(release), the flush goroutine outlived the test and wrote its
	// record into TempDir while the runner was removing it — "TempDir
	// RemoveAll cleanup: directory not empty" on a slow CI runner. A goroutine
	// the test started is the test's to reap.
	close(release)
	if err := mod.StopWithContext(context.Background()); err != nil {
		t.Fatalf("StopWithContext after releasing the provider: %v", err)
	}
}

func TestFormatNestStatsUsesReadableQueueNames(t *testing.T) {
	stats := formatNestStats(nest.DispatcherStats{
		Main: worker.PoolStats{
			Name:      "nest",
			WorkerNum: 8,
			QueueCap:  1024,
			QueueLen:  4,
			Started:   true,
		},
		Heart: worker.PoolStats{
			Name:      "nest_hb",
			WorkerNum: 4,
			QueueCap:  1024,
			QueueLen:  1,
			Started:   true,
		},
		Cost: worker.PoolStats{
			Name:      "nest_cost",
			WorkerNum: 8,
			QueueCap:  1024,
			QueueLen:  0,
			Started:   true,
		},
		Delayed: 3,
		Work: nest.DispatcherWorkStats{
			ProcessedMessages: 100,
			Slow200msMessages: 7,
		},
	}, nest.DispatcherWorkStats{
		ProcessedMessages: 13,
		Slow200msMessages: 2,
	}, 5*time.Second)
	if stats.Main.Workers != 8 || stats.Main.QueueUsage != "4/1024" || !stats.Main.Running {
		t.Fatalf("main stats mismatch: %+v", stats.Main)
	}
	if stats.Broadcast.Workers != 4 || stats.Broadcast.QueueUsage != "1/1024" {
		t.Fatalf("broadcast stats mismatch: %+v", stats.Broadcast)
	}
	if stats.Cost.Name != "nest_cost" || stats.DelayedMessages != 3 {
		t.Fatalf("nest stats mismatch: %+v", stats)
	}
	if stats.ProcessedMessages != 13 || stats.Slow200msMessages != 2 || stats.WindowSeconds != 5 {
		t.Fatalf("nest window stats mismatch: %+v", stats)
	}
	if stats.ProcessedMessagesTotal != 100 || stats.Slow200msMessagesTotal != 7 {
		t.Fatalf("nest cumulative stats mismatch: %+v", stats)
	}
}
