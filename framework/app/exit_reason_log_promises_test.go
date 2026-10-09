package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// RR-20261006-07（A13）：app.run 的最终错误要进文件日志。run 在返回前 defer flog.Close()，生成的 main 在 Close 之后才
// slog.Error("server exit", "err", err)，只到 stderr；只看日志文件的运维不知道进程为什么退出。现在 run 在关闭文件日志之前
// 写一行 Error，带上最终错误（含停机期间并入的 RuntimeFailure）。

// Serve / Shutdown 的错误 run 自己已经记过日志；启动阶段（Mod Init / Provide / Start、单实例锁、业务时间检查）
// 失败时直接返回，之前文件里一行都没有。这里用 Mod Init 失败。
var errExitReasonForLog = errors.New("mod init failed: exit reason 7c1e")

type exitReasonMod struct{}

func (exitReasonMod) Name() ModName           { return "exit_reason" }
func (exitReasonMod) Init(*viper.Viper) error { return errExitReasonForLog }
func (exitReasonMod) Provide(*Registry) error { return nil }
func (exitReasonMod) Start() error            { return nil }
func (exitReasonMod) Stop()                   {}

func TestRunWritesTheExitReasonToTheFileLog(t *testing.T) {
	dir := t.TempDir()
	a := New("roost-test", "0.0.0")
	a.Mods(exitReasonMod{})
	a.RegisterServer("game", &errService{})
	a.cfg.Set("log.file", true)
	a.cfg.Set("log.stdout", false)
	a.cfg.Set("log.dir", dir)
	a.cfg.Set("log.rotate_interval", 0)
	a.RootCmd().SetArgs([]string{"game"})

	if err := a.Execute(); !errors.Is(err, errExitReasonForLog) {
		t.Fatalf("Execute err = %v, want the mod init error", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "game-1000.log"))
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(raw), errExitReasonForLog.Error()) {
		t.Fatalf("the exit reason %q is not in the file log:\n%s", errExitReasonForLog, raw)
	}
}
