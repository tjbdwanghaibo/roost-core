// 统计日志：独立 JSONL 文件、关闭时不建文件、provider 注销与 panic、停止不被阻塞的 flush、只读快照不消费窗口、写失败可见。

package statslog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/worker"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

func TestLoggerWritesSeparateJSONLFile(t *testing.T) {
	dir := t.TempDir()
	cfg := viper.New()
	cfg.Set("server_type", "game")
	cfg.Set("sid", 7)
	cfg.Set("stats_log.enabled", true)
	cfg.Set("stats_log.dir", dir)
	cfg.Set("stats_log.filename", "game-7.stats.log")
	cfg.Set("stats_log.interval", "1h")

	mod := New()
	if err := configureTestLogger(mod, cfg); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := connectTestLogger(mod, app.NewRegistry(cfg)); err != nil {
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

func TestLoggerDisabledDoesNotCreateFile(t *testing.T) {
	dir := t.TempDir()
	cfg := viper.New()
	cfg.Set("server_type", "gate")
	cfg.Set("sid", 8)
	cfg.Set("stats_log.enabled", false)
	cfg.Set("stats_log.dir", dir)
	cfg.Set("stats_log.filename", "gate-8.stats.log")
	cfg.Set("stats_log.interval", time.Second)

	mod := New()
	if err := configureTestLogger(mod, cfg); err != nil {
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
	mod := New()
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
	mod := New()
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

	mod := New()
	if err := configureTestLogger(mod, cfg); err != nil {
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
		Short: worker.PoolStats{
			Name:      "nest",
			WorkerNum: 8,
			QueueCap:  1024,
			QueueLen:  4,
			Started:   true,
		},
		IO: worker.PoolStats{
			Name:      "nest.io",
			WorkerNum: 4,
			QueueCap:  1024,
			QueueLen:  1,
			Started:   true,
		},
		Long:    worker.PoolStats{WorkerNum: 2, QueueCap: 16, QueueLen: 3, Started: true},
		Queue:   nest.DispatchQueueStats{LongContinuations: 2},
		Delayed: 3,
		Work: nest.DispatcherWorkStats{
			ProcessedMessages: 100,
			Slow200msMessages: 7,
		},
	}, nest.DispatcherWorkStats{
		ProcessedMessages: 13,
		Slow200msMessages: 2,
	}, 5*time.Second)
	if stats.Short.Workers != 8 || stats.Short.QueueUsage != "4/1024" || !stats.Short.Running {
		t.Fatalf("main stats mismatch: %+v", stats.Short)
	}
	if stats.IO.Workers != 4 || stats.IO.QueueUsage != "1/1024" {
		t.Fatalf("IO stats mismatch: %+v", stats.IO)
	}
	if stats.Long.Workers != 2 || stats.Long.QueueUsage != "3/16" || stats.LongContinuations != 2 {
		t.Fatalf("long business stats mismatch: %+v", stats)
	}
	if stats.DelayedMessages != 3 {
		t.Fatalf("nest stats mismatch: %+v", stats)
	}
	if stats.ProcessedMessages != 13 || stats.Slow200msMessages != 2 || stats.WindowSeconds != 5 {
		t.Fatalf("nest window stats mismatch: %+v", stats)
	}
	if stats.ProcessedMessagesTotal != 100 || stats.Slow200msMessagesTotal != 7 {
		t.Fatalf("nest cumulative stats mismatch: %+v", stats)
	}
}

type snapshotNest struct{ n uint64 }

func (s *snapshotNest) Stats() nest.DispatcherStats {
	return nest.DispatcherStats{Work: nest.DispatcherWorkStats{ProcessedMessages: s.n}}
}
func TestStatsEndpointDoesNotConsumeFileWindow(t *testing.T) {
	cfg := viper.New()
	cfg.Set("stats_log.enabled", true)
	cfg.Set("stats_log.dir", t.TempDir())
	cfg.Set("stats_log.filename", "b7.jsonl")
	m := New()
	if err := configureTestLogger(m, cfg); err != nil {
		t.Fatal(err)
	}
	r := app.NewRegistry(cfg)
	n := &snapshotNest{n: 10}
	if err := r.Register(app.ModName("nest"), n); err != nil {
		t.Fatal(err)
	}
	if err := connectTestLogger(m, r); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	if err := m.FlushOnce(); err != nil {
		t.Fatal(err)
	}
	n.n = 15
	m.CollectStats()
	n.n = 20
	if err := m.FlushOnce(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(m.dir, m.filename))
	if err != nil {
		t.Fatal(err)
	}
	var total uint64
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var record StatsRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		total += record.Nest.ProcessedMessages
	}
	if total != 20 {
		t.Fatalf("file windows contain %d messages, want 20; /statsz consumed part of the window", total)
	}
}

// stats_log 写不进去时运维看得见：WARN 日志加一个无标签计数器 stats_log.write_failures，启动时就发现，不等第一个 interval。同一个
// 错误反复出现只告警一次（计数器每次都加），恢复写入时打一条 INFO。
// 历史：RR-20260928-04。

// lockedBuffer is a slog sink the flush goroutine and the test read and write
// concurrently.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureSlog routes the default logger into a buffer for one test. The
// default logger is process-wide, so these tests do not run in parallel.
func captureSlog(t *testing.T) *lockedBuffer {
	t.Helper()
	out := &lockedBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return out
}

// unwritableStatsDir is a stats_log.dir that MkdirAll cannot create even as
// root: its parent is a regular file.
func unwritableStatsDir(t *testing.T) (dir, blocker string) {
	t.Helper()
	blocker = filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(blocker, "log"), blocker
}

func startStatsLog(t *testing.T, dir string, interval time.Duration) (*Logger, *metrics.Registry) {
	t.Helper()
	cfg := viper.New()
	cfg.Set("server_type", "game")
	cfg.Set("sid", 9)
	cfg.Set("stats_log.enabled", true)
	cfg.Set("stats_log.dir", dir)
	cfg.Set("stats_log.interval", interval)
	mod := New()
	if err := configureTestLogger(mod, cfg); err != nil {
		t.Fatal(err)
	}
	registry := app.NewRegistry(cfg)
	if err := connectTestLogger(mod, registry); err != nil {
		t.Fatal(err)
	}
	reg, ok := app.Lookup[*metrics.Registry](registry, app.ModMetrics)
	if !ok || reg == nil {
		t.Fatal("the app registry publishes no metrics registry")
	}
	if err := mod.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(mod.Stop)
	return mod, reg
}

func writeFailures(reg *metrics.Registry) int64 {
	for _, metric := range reg.Snapshot() {
		if metric.Name == "stats_log.write_failures" {
			return metric.Value
		}
	}
	return 0
}

func TestStatsLogStartReportsAnUnwritableDirectory(t *testing.T) {
	logs := captureSlog(t)
	dir, _ := unwritableStatsDir(t)
	// An hour: nothing periodic runs during the test, so whatever is reported
	// was reported by Start itself.
	_, reg := startStatsLog(t, dir, time.Hour)

	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), dir) {
		t.Errorf("Start with an unwritable stats_log.dir %s logged no WARN naming it; logs:\n%s", dir, logs.String())
	}
	if got := writeFailures(reg); got != 1 {
		t.Errorf("stats_log.write_failures after Start = %d, want 1", got)
	}
}

func TestStatsLogPeriodicFailuresAreCountedWarnedOnceAndRecoveryIsLogged(t *testing.T) {
	logs := captureSlog(t)
	dir, blocker := unwritableStatsDir(t)
	_, reg := startStatsLog(t, dir, 5*time.Millisecond)

	waitFor(t, "stats_log.write_failures >= 3", func() bool { return writeFailures(reg) >= 3 })
	if warns := strings.Count(logs.String(), "level=WARN"); warns != 1 {
		t.Errorf("the same write error repeated %d times produced %d WARN lines, want 1; logs:\n%s",
			writeFailures(reg), warns, logs.String())
	}

	// The operator fixes the directory: the next tick writes, and says so.
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "game-9.stats.log")
	waitFor(t, "a record in "+file, func() bool {
		info, err := os.Stat(file)
		return err == nil && info.Size() > 0
	})
	waitFor(t, "an INFO line saying writing resumed", func() bool {
		return strings.Contains(logs.String(), "level=INFO") && strings.Contains(logs.String(), file)
	})
}

// waitFor polls a condition the flush goroutine establishes on its own clock.
func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}
