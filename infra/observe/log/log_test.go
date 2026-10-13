// 日志：上下文 / 运行时 / 调用方属性、ELog、文件轮转、关闭后的回落、sink 故障隔离、系统时钟。

package log

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

func TestContextAttrsArePrepended(t *testing.T) {
	var buf bytes.Buffer
	if err := Init(Options{
		Level:       slog.LevelInfo,
		Output:      &buf,
		DisableGoID: true,
		FrameFunc:   func() uint64 { return 42 },
	}); err != nil {
		t.Fatalf("Init error: %v", err)
	}
	defer func() {
		_ = Close()
	}()

	Info("hello", "entity", 1001)

	line := buf.String()
	if strings.Contains(line, "goid=") {
		t.Fatalf("log should not contain lowercase goid key: %s", line)
	}
	framePos := strings.Index(line, "frame=42")
	timePos := strings.Index(line, "server_time_ms=")
	entityPos := strings.Index(line, "entity=1001")
	if framePos < 0 || timePos < 0 || entityPos < 0 {
		t.Fatalf("missing expected attrs: %s", line)
	}
	if framePos > entityPos || timePos > entityPos {
		t.Fatalf("context attrs should be before business attrs: %s", line)
	}
}

func TestRuntimeAttrsAreWrittenBetweenLevelAndMessage(t *testing.T) {
	var buf bytes.Buffer
	if err := Init(Options{
		Level:     slog.LevelInfo,
		Output:    &buf,
		FrameFunc: func() uint64 { return 42 },
	}); err != nil {
		t.Fatalf("Init error: %v", err)
	}
	defer func() {
		_ = Close()
	}()

	_, release := fctx.NewContext(
		fctx.WithFrame(7),
		fctx.WithMeta(fctx.RequestMeta{Source: "nest", Handler: "handlerA"}),
	)
	defer release()

	Info("hello world", "entity", 1001)

	line := buf.String()
	levelPos := strings.Index(line, "level=INFO")
	goIDPos := strings.Index(line, "goId=")
	framePos := strings.Index(line, "frame=7")
	timePos := strings.Index(line, "server_time_ms=")
	sourcePos := strings.Index(line, "source=nest")
	handlerPos := strings.Index(line, "handler=handlerA")
	msgPos := strings.Index(line, `msg="hello world"`)
	entityPos := strings.Index(line, "entity=1001")
	if levelPos < 0 || goIDPos < 0 || framePos < 0 || timePos < 0 || sourcePos < 0 ||
		handlerPos < 0 || msgPos < 0 || entityPos < 0 {
		t.Fatalf("missing expected attrs: %s", line)
	}
	if !(levelPos < goIDPos && goIDPos < framePos && framePos < timePos &&
		timePos < sourcePos && sourcePos < handlerPos && handlerPos < msgPos && msgPos < entityPos) {
		t.Fatalf("unexpected log order: %s", line)
	}
}

func TestCallerAttrsIncludeFileLineAndFunction(t *testing.T) {
	var buf bytes.Buffer
	if err := Init(Options{
		Level:        slog.LevelInfo,
		Output:       &buf,
		Caller:       true,
		DisableGoID:  true,
		DisableFrame: true,
	}); err != nil {
		t.Fatalf("Init error: %v", err)
	}
	defer func() {
		_ = Close()
	}()

	logCallerAttrsTestHelper()

	line := buf.String()
	if !strings.Contains(line, "caller=") {
		t.Fatalf("log should contain caller file and line: %s", line)
	}
	if !strings.Contains(line, "caller_func=github.com/tjbdwanghaibo/roost-core/infra/observe/log.logCallerAttrsTestHelper") {
		t.Fatalf("log should contain caller function: %s", line)
	}
	if strings.Contains(line, " source=") {
		t.Fatalf("caller output should not use source key: %s", line)
	}
}

func logCallerAttrsTestHelper() {
	Info("caller attrs")
}

func TestELogCallerAttrsIncludeUserFunction(t *testing.T) {
	var buf bytes.Buffer
	if err := Init(Options{
		Level:        slog.LevelInfo,
		Output:       &buf,
		Caller:       true,
		DisableGoID:  true,
		DisableFrame: true,
	}); err != nil {
		t.Fatalf("Init error: %v", err)
	}
	defer func() {
		_ = Close()
	}()

	elogCallerAttrsTestHelper()

	line := buf.String()
	if !strings.Contains(line, "caller_func=github.com/tjbdwanghaibo/roost-core/infra/observe/log.elogCallerAttrsTestHelper") {
		t.Fatalf("elog should contain user caller function: %s", line)
	}
	if strings.Contains(line, "elog.go") {
		t.Fatalf("elog caller should not point to wrapper file: %s", line)
	}
}

func elogCallerAttrsTestHelper() {
	NewELog().Title("caller").Info("elog caller attrs")
}

func TestGoIDKey(t *testing.T) {
	var buf bytes.Buffer
	if err := Init(Options{
		Level:        slog.LevelInfo,
		Output:       &buf,
		DisableFrame: true,
	}); err != nil {
		t.Fatalf("Init error: %v", err)
	}
	defer func() {
		_ = Close()
	}()

	Info("hello")

	line := buf.String()
	if !strings.Contains(line, "goId=") {
		t.Fatalf("log should contain goId key: %s", line)
	}
	if strings.Contains(line, "goid=") {
		t.Fatalf("log should not contain lowercase goid key: %s", line)
	}
}

func TestELogKeepsFixedContextBeforeCallAttrs(t *testing.T) {
	var buf bytes.Buffer
	if err := Init(Options{
		Level:       slog.LevelDebug,
		Output:      &buf,
		DisableGoID: true,
		FrameFunc:   func() uint64 { return 7 },
	}); err != nil {
		t.Fatalf("Init error: %v", err)
	}
	defer func() {
		_ = Close()
	}()

	NewELog().
		Title("battle").
		EntityID(1001).
		OwnerID(2002).
		Debug("started", "target", 3003)

	line := buf.String()
	for _, want := range []string{"frame=7", "title=battle", "entityId=1001", "ownerId=2002", "target=3003"} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %s in %s", want, line)
		}
	}
	titlePos := strings.Index(line, "title=battle")
	entityPos := strings.Index(line, "entityId=1001")
	targetPos := strings.Index(line, "target=3003")
	if titlePos > targetPos || entityPos > targetPos {
		t.Fatalf("elog fixed attrs should be before call attrs: %s", line)
	}
}

func TestELogWithDoesNotMutateBase(t *testing.T) {
	var buf bytes.Buffer
	if err := Init(Options{
		Level:       slog.LevelDebug,
		Output:      &buf,
		DisableGoID: true,
		FrameFunc:   func() uint64 { return 9 },
	}); err != nil {
		t.Fatalf("Init error: %v", err)
	}
	defer func() {
		_ = Close()
	}()

	base := NewELog().Title("mission").EntityID(1001)
	base.With("step", 2).Info("derived")
	base.Info("base")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected two log lines, got %d: %q", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], "step=2") {
		t.Fatalf("derived log should contain scoped attr: %s", lines[0])
	}
	if strings.Contains(lines[1], "step=2") {
		t.Fatalf("base log should not inherit derived attr: %s", lines[1])
	}
}

func TestNilELogIsSafe(t *testing.T) {
	var buf bytes.Buffer
	if err := Init(Options{
		Level:       slog.LevelDebug,
		Output:      &buf,
		DisableGoID: true,
		FrameFunc:   func() uint64 { return 11 },
	}); err != nil {
		t.Fatalf("Init error: %v", err)
	}
	defer func() {
		_ = Close()
	}()

	var l *ELog
	l.Info("nil safe", "entityId", 1001)

	line := buf.String()
	if !strings.Contains(line, "nil safe") || !strings.Contains(line, "entityId=1001") {
		t.Fatalf("nil elog should still write through default logger: %s", line)
	}
}

func TestFileLogRotatesByTime(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 26, 12, 30, 0, 0, time.Local)
	if err := Init(Options{
		Level:          slog.LevelInfo,
		File:           true,
		Dir:            dir,
		Filename:       "roost.log",
		RotateInterval: time.Hour,
		NowFunc: func() time.Time {
			return now
		},
		DisableGoID:  true,
		DisableFrame: true,
	}); err != nil {
		t.Fatalf("Init error: %v", err)
	}

	Info("first")
	now = now.Add(time.Hour)
	Info("second")
	if err := Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}

	firstPath := filepath.Join(dir, "roost.2026052612.log")
	secondPath := filepath.Join(dir, "roost.2026052613.log")
	first, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatalf("read first log: %v", err)
	}
	second, err := os.ReadFile(secondPath)
	if err != nil {
		t.Fatalf("read second log: %v", err)
	}
	if !strings.Contains(string(first), "first") || strings.Contains(string(first), "second") {
		t.Fatalf("unexpected first log content: %s", first)
	}
	if !strings.Contains(string(second), "second") || strings.Contains(string(second), "first") {
		t.Fatalf("unexpected second log content: %s", second)
	}
}

// Close 只撤下文件 sink，之后的日志不消失：只配了文件时 Close 之后的日志回落到 stderr，配了控制台时保留控制台（进程退出原因与停机期间
// 的日志都还能看到）。
// 历史：RR-20261005-NC-165。

func restoreDefaultLogger(t *testing.T) {
	t.Helper()
	previous := slog.Default()
	t.Cleanup(func() {
		_ = Close()
		slog.SetDefault(previous)
		loggerMu.Lock()
		defaultLogger = previous
		loggerMu.Unlock()
	})
}

func TestLogsAfterCloseOfAFileOnlySinkReachStderr(t *testing.T) {
	restoreDefaultLogger(t)
	dir := t.TempDir()
	if err := Init(Options{Level: slog.LevelInfo, File: true, Dir: dir, Filename: "app.log", DisableGoID: true, DisableFrame: true}); err != nil {
		t.Fatal(err)
	}
	slog.Info("before close")

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := os.Stderr
	os.Stderr = writer
	t.Cleanup(func() { os.Stderr = stderr })

	if err := Close(); err != nil {
		t.Fatal(err)
	}
	slog.Error("server exit", "err", "mod init failed")
	Warn("late shutdown line")
	os.Stderr = stderr
	_ = writer.Close()
	captured, _ := io.ReadAll(reader)

	file, err := os.ReadFile(dir + "/app.log")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(file), "before close") {
		t.Fatalf("fixture: the file sink missed the line written before Close: %q", file)
	}
	for _, want := range []string{"server exit", "mod init failed", "late shutdown line"} {
		if !strings.Contains(string(captured), want) {
			t.Fatalf("after Close of a file-only sink %q went nowhere (stderr=%q)", want, captured)
		}
	}
}

func TestLogsAfterCloseKeepTheConsoleWriter(t *testing.T) {
	restoreDefaultLogger(t)
	var console bytes.Buffer
	if err := Init(Options{Level: slog.LevelInfo, Output: &console, File: true, Dir: t.TempDir(), Filename: "app.log", DisableGoID: true, DisableFrame: true}); err != nil {
		t.Fatal(err)
	}
	if err := Close(); err != nil {
		t.Fatal(err)
	}
	slog.Error("server exit")
	if !strings.Contains(console.String(), "server exit") {
		t.Fatalf("the console writer lost the line written after Close: %q", console.String())
	}
}

// 轮转间隔必须为正；关闭后的写入报 os.ErrClosed。
func TestRotatingWriterRefusesNonPositiveIntervalsAndWritesAfterClose(t *testing.T) {
	if _, err := newTimeRotatingFileWriter(timeRotatingFileOptions{Dir: t.TempDir(), Filename: "app.log"}); err == nil {
		t.Fatal("interval 0 accepted")
	}
	writer, err := newTimeRotatingFileWriter(timeRotatingFileOptions{Dir: t.TempDir(), Filename: "app.log", Interval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := writer.Write([]byte("late\n")); !errors.Is(err, os.ErrClosed) || n != 0 {
		t.Fatalf("Write after Close = (%d, %v), want os.ErrClosed", n, err)
	}
}

func TestServerTimeUsesSystemClockInsideBusinessContext(t *testing.T) {
	ctx, release := fctx.NewContext()
	defer release()
	ctx.NowMilli = time.Now().Add(24 * time.Hour).UnixMilli()
	before := time.Now().UnixMilli()
	attrs := (contextHandler{}).contextAttrs(0)
	after := time.Now().UnixMilli()
	for _, a := range attrs {
		if a.Key == "server_time_ms" {
			if n := a.Value.Int64(); n < before || n > after {
				t.Fatalf("server_time_ms=%d follows business offset; system window=[%d,%d]", n, before, after)
			}
			return
		}
	}
	t.Fatal("missing server time")
}

// 日志 sink 出错时能写的地方照常写，失败可数：轮转打开新分片失败时继续写当前分片（且不每行重试打开）；控制台出错不阻止文件 sink 写入；
// 两种失败都计数。
// 历史：RR-20261005-NC-263。

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
