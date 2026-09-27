package statslog

// RR-20260928-04：stats_log 写不进去时必须让运维看得见——WARN 日志加一个无标签计数器
// stats_log.write_failures，而且启动时就要发现，不能等第一个 interval（默认 1 分钟）。
// 旧行为：Start 不碰文件，周期循环里 `_ = m.FlushOnce()` 把 MkdirAll / OpenFile 的错误吞掉；
// 生成的容器（read_only 根文件系统、WORKDIR /app，相对的 stats_log.dir: log 指向只读的
// /app/log）运行 75 秒后 /app/log 不存在，日志里也没有任何一行提示。
// 同一个错误反复出现只告警一次（计数器每次都加），恢复写入时打一条 INFO。

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	"github.com/tjbdwanghaibo/roost-core/metrics"
)

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

func startStatsLog(t *testing.T, dir string, interval time.Duration) (*StatsLogMod, *metrics.Registry) {
	t.Helper()
	cfg := viper.New()
	cfg.Set("server_type", "game")
	cfg.Set("sid", 9)
	cfg.Set("stats_log.enabled", true)
	cfg.Set("stats_log.dir", dir)
	cfg.Set("stats_log.interval", interval)
	mod := NewStatsLogMod()
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	registry := app.NewRegistry(cfg)
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	reg, ok := app.Lookup[*metrics.Registry](registry, mods.ModMetrics)
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
