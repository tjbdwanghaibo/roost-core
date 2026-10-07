package Game

import (
	"context"
	"testing"
	"time"

	gamescene "example.com/planet/game/scene"
)

// RR-20260930-18：Service.Shutdown 把 App 给的停机 ctx 传给 Scene.Close，所以停止“卸载后重载”
// （stopUnloadResync）和关闭 replication manager 都受 shutdown.total_timeout 约束。旧行为是
// Shutdown 忽略参数、用 context.Background() 关场景：一次卡住的 stop 让停机越过 App 的时限，
// 直到部署侧的 SIGKILL（compose stop_grace_period / systemd TimeoutStopSec）。
//
// 场景由既有 harness 建（没有实体运行时，所以 stopUnloads 为 nil），这里放一个记下 ctx 的替身：
// Close 一定经过它，它看到的就是 Close 收到的 ctx。
func TestShutdownClosesTheSceneUnderTheAppsStopDeadline(t *testing.T) {
	scene, err := newScene(newSceneRecorder(), nil, gamescene.DefaultConfig())
	if err != nil {
		t.Fatalf("new scene: %v", err)
	}
	if err := scene.Start(context.Background()); err != nil {
		t.Fatalf("start scene: %v", err)
	}
	var seen context.Context
	scene.stopUnloads = func(ctx context.Context) error {
		seen = ctx
		return nil
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	service := &Service{scene: scene}
	if err := service.Shutdown(stopCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if seen == nil {
		t.Fatal("Shutdown did not close the scene (the reload stop was never called)")
	}
	if _, ok := seen.Deadline(); !ok {
		t.Fatalf("Scene.Close ran under a context with no deadline; Shutdown must pass the App's stop context on, not context.Background()")
	}
	if service.scene != nil {
		t.Fatal("Shutdown left the scene set after closing it")
	}
}

// RR-20260930-23 转写（App 单实例锁方案 §7.3）：原承诺是“租约间断之后不留下脱离场景的连接”。按玩家
// 的租约删除之后，同一 sid 的间断只会以进程 fail-stop 的形式出现（失锁、DataEngine / Remote fatal：
// RuntimeFailure → Nest 围栏 → Service.Shutdown），承诺变成：停机时本进程服务中的每个玩家的连接都被
// 断开，客户端重连到接替的进程，而不是挂在一个不再处理请求的进程上。
func TestShutdownClosesTheConnectionsOfEveryServedPlayer(t *testing.T) {
	owners, fencer, _ := ownersUnderTest(t)
	for _, playerID := range []int64{42, 43} {
		if err := owners.Serve(context.Background(), playerID, ownSID); err != nil {
			t.Fatalf("serve %d: %v", playerID, err)
		}
		fencer.connect(playerID)
	}
	service := &Service{owners: owners}
	if err := service.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	closed := map[int64]bool{}
	for _, playerID := range fencer.closedPlayers() {
		closed[playerID] = true
	}
	if !closed[42] || !closed[43] {
		t.Fatalf("Shutdown left served players connected: closed %v, want 42 and 43", fencer.closedPlayers())
	}
	if fencer.ActiveSessions(42) != 0 || fencer.ActiveSessions(43) != 0 {
		t.Fatal("a served player still has a connection after Shutdown")
	}
}
