package log

// RR-20261005-NC-165：Close 只撤下文件 sink，之后的日志不能消失。
//
// app.run 在返回前 defer flog.Close()，随后生成的 main 用 slog.Error("server exit", "err", err)
// 写下进程为什么退出；停机超时分支里仍在运行的 Serve / Shutdown / Mod 也会继续打日志。旧 Close 只
// 关文件，slog.Default 仍指向写这个已关闭文件的 handler：只配了文件（log.stdout: false）时这些行
// 写进已关闭的文件、错误被 slog 吞掉，进程带着退出码 1 消失，任何地方都没有原因。

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
)

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
