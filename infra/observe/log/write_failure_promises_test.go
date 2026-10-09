package log

// RR-20261005-NC-263（N12 观察 O6）：日志 sink 出错时能写的地方照常写，失败可数。
//
//  1. 轮转打开新分片失败（EMFILE、ENOSPC、目录权限）时，旧实现 Write 直接返回错误、整行丢弃，即使当前
//     分片仍可写；而且之后每一行都重试一次 MkdirAll + Open。
//  2. 同时配了控制台和文件时用 io.MultiWriter，它在第一个写失败处停止：控制台（排在前面）出错时，文件
//     也收不到这一行。
//  3. 两种失败都没有任何计数，slog 吞掉 handler 的写错误，运维看不到日志在丢。

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

func counterValue(name string, labels metrics.Labels) int64 {
	var total int64
	for _, m := range metrics.Snapshot() {
		if m.Name != name {
			continue
		}
		match := true
		for k, v := range labels {
			if m.Labels[k] != v {
				match = false
			}
		}
		if match {
			total += m.Value
		}
	}
	return total
}

type steppingClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *steppingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *steppingClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func TestRotationFailureKeepsWritingTheCurrentSlice(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	clock := &steppingClock{now: time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)}
	writer, err := newTimeRotatingFileWriter(timeRotatingFileOptions{Dir: dir, Filename: "app.log", Interval: time.Hour, NowFunc: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	current := filepath.Join(dir, "app.2026100610.log")
	if _, err := writer.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}

	// 下一个分片打不开：目录只读。
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	failuresBefore := counterValue("log.rotate_failures", nil)
	clock.Advance(time.Hour)
	if _, err := writer.Write([]byte("second\n")); err != nil {
		t.Errorf("Write while the next slice cannot be opened = %v, want nil: the current slice is still writable", err)
	}
	if _, err := writer.Write([]byte("third\n")); err != nil {
		t.Errorf("second Write while the next slice cannot be opened = %v, want nil", err)
	}
	data, _ := os.ReadFile(current)
	if !strings.Contains(string(data), "second") || !strings.Contains(string(data), "third") {
		t.Errorf("current slice = %q, want the lines written while rotation failed", data)
	}
	if got := counterValue("log.rotate_failures", nil) - failuresBefore; got != 1 {
		t.Errorf("log.rotate_failures grew by %d over two writes inside the retry interval, want 1 (counted, and not retried on every line)", got)
	}

	// 恢复后，过了重试间隔，下一行进入新分片。
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	clock.Advance(rotateRetryInterval)
	if _, err := writer.Write([]byte("fourth\n")); err != nil {
		t.Fatal(err)
	}
	next, err := os.ReadFile(filepath.Join(dir, "app.2026100611.log"))
	if err != nil || !strings.Contains(string(next), "fourth") {
		t.Fatalf("new slice after recovery = %q, %v; want the line written after the retry interval", next, err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("console is gone") }

func TestAFailingConsoleDoesNotStopTheFileSink(t *testing.T) {
	restoreDefaultLogger(t)
	dir := t.TempDir()
	if err := Init(Options{Level: slog.LevelInfo, Output: failingWriter{}, File: true, Dir: dir, Filename: "app.log", DisableGoID: true, DisableFrame: true}); err != nil {
		t.Fatal(err)
	}
	errorsBefore := counterValue("log.write_errors", metrics.Labels{"sink": "console"})
	slog.Info("reaches the file")
	data, err := os.ReadFile(filepath.Join(dir, "app.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "reaches the file") {
		t.Errorf("file sink = %q, want the line although the console failed", data)
	}
	if got := counterValue("log.write_errors", metrics.Labels{"sink": "console"}) - errorsBefore; got != 1 {
		t.Errorf("log.write_errors{sink=console} grew by %d, want 1", got)
	}
}
