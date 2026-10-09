package remoteentity

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	coreremote "github.com/tjbdwanghaibo/roost-core/framework/remoteentity"
)

// RR-20261005-NC-234（N14 观察 O4）：remote_entity Mod 停止失败（ctx 到期、finalizer / 复制 handler 没排空）
// 时不能记 “stopped”。旧行为：不论 asm.Stop 的结果，都记 Info “remote_entity mod: stopped”，与 App 随后的
// “mod stop failed … context deadline exceeded” 互相矛盾，排查停机不完整时误导运维。
func TestRemoteEntityModDoesNotLogStoppedWhenStopFails(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	mod := &RemoteEntityMod{asm: &coreremote.Assembly{}, cfg: coreremote.DefaultConfig()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 停机预算已经用完
	if err := mod.StopWithContext(ctx); err == nil {
		t.Fatal("Stop with an expired ctx = nil")
	}
	if strings.Contains(logs.String(), `msg="remote_entity mod: stopped"`) {
		t.Fatalf("a failed stop logged success:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "remote_entity mod: stop incomplete") {
		t.Fatalf("a failed stop left no record:\n%s", logs.String())
	}

	logs.Reset()
	if err := mod.StopWithContext(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !strings.Contains(logs.String(), `msg="remote_entity mod: stopped"`) {
		t.Fatalf("a completed stop did not log it:\n%s", logs.String())
	}
}
