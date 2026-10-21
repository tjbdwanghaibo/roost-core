package alliance

import (
	"context"
	"errors"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/base/clock"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 故意使用直到 release 才响应的 loader，证明 Shutdown 超时不假装 Serve 已结束。
func verifyVisionServiceLifecycle(t *testing.T) {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	service := &VisionService{Interval: time.Second, ReadTimeout: time.Second, Refresh: func(ctx context.Context, _ uint64) error {
		close(entered)
		<-release
		return ctx.Err()
	}}
	check(t, service.Init(nil))
	done := make(chan error, 1)
	go func() { done <- service.Serve(context.Background()) }()
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("service did not start")
	}
	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err := service.Shutdown(short)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown claimed drained: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("inflight read lost: %v", err)
	default:
	}
	close(release)
	released = true
	wait, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	check(t, service.Shutdown(wait))
	select {
	case err := <-done:
		check(t, err)
	case <-wait.Done():
		t.Fatal("service did not drain")
	}
	check(t, service.Shutdown(wait))
	// 初始化失败、已初始化但尚未 Serve 的路径也必须可关闭。
	partial := &VisionService{}
	if partial.Init(nil) == nil {
		t.Fatal("missing callback accepted")
	}
	check(t, partial.Shutdown(wait))
	idle := &VisionService{Interval: time.Second, ReadTimeout: time.Second, Refresh: func(context.Context, uint64) error { t.Fatal("stopped service refreshed"); return nil }}
	check(t, idle.Init(nil))
	check(t, idle.Shutdown(wait))
	check(t, idle.Serve(wait))
}

// 在已装配的真实 Nest/WAL/TCP 消费者中运行正式 App game 入口；
// App 管 Service 生命周期，外部测试拥有的基础设施只在 App 返回后清理。
func verifyVisionApp(t *testing.T, view *durableVision) {
	t.Helper()
	service := &VisionService{Interval: time.Second, ReadTimeout: time.Second, Refresh: func(ctx context.Context, _ uint64) error { return view.refresh(ctx) }}
	var readErr error
	service.Observe = func(err error) { readErr = err; service.cancel() }
	path := filepath.Join(os.Getenv("ROOST_SLG_REWARD_WAL"), "vision-app.yaml")
	check(t, os.WriteFile(path, []byte("log:\n  file: false\n  stdout: false\nsingleton:\n  enabled: false\n"), 0600))
	application := app.New("slg-vision-practice", "0.0.0").RegisterServer("game", service)
	application.RootCmd().SetArgs([]string{"game", "--config", path, "--sid", os.Getenv("SLG_REWARD_SID")})
	check(t, application.Execute())
	check(t, readErr)
	select {
	case <-service.done:
	default:
		t.Fatal("App returned before vision drained")
	}
	view.expect()
}

// 只给独立测试子进程换真实时间来源；不可变闭包通过 clock 的原子入口发布。
func freezeBusinessTime(ms int64) { clock.SetSource(func() time.Time { return time.UnixMilli(ms) }) }
