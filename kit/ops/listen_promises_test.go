package ops

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
)

// RR-20261005-NC-230：ops.enabled 时 Start 返回 nil 必须表示 /healthz、/readyz、/admin 已经在 ops.addr
// 上监听。端口被占用（另一个服务配了同一个 ops 端口、旧进程还没退出）时 Start 必须失败，让启动按
// 正常的启动失败路径收尾。
//
// 旧行为：Start 只把 ListenAndServe 丢进 goroutine，bind 失败只写一条 Error 日志，Start 返回 nil。进程
// 在没有探针端点的情况下继续运行；同机部署（shell / dev-run）的健康检查探到的是占着端口的另一个进程，
// 可能把这个实例判成健康；k8s 则要等 startupProbe 全部失败才重启。
func TestOpsStartFailsWhenTheAddressIsTaken(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	m := newStartableOpsMod(t, taken.Addr().String())
	err = m.Start()
	if err == nil {
		_ = m.StopWithContext(context.Background())
		t.Fatalf("Start on %s (already in use) = nil, want the bind error: the process would run without its probe endpoints", taken.Addr())
	}
	if !strings.Contains(err.Error(), taken.Addr().String()) {
		t.Fatalf("Start error = %v, want it to name ops.addr", err)
	}
	// 失败的 Start 不留下待关闭的 server：修正端口后可以再 Start（同一个 Mod 实例不会被 App 重用，
	// 这里只确认没有半截状态）。
	if err := m.StopWithContext(context.Background()); err != nil {
		t.Fatalf("Stop after a failed Start = %v", err)
	}
}

// newStartableOpsMod 走正式 Init / Provide，ops.enabled 打开、监听 addr。
func newStartableOpsMod(t *testing.T, addr string) *OpsMod {
	t.Helper()
	cfg := viper.New()
	cfg.Set("ops.enabled", true)
	cfg.Set("ops.addr", addr)
	m := NewOpsMod()
	if err := m.Init(cfg); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := m.Provide(app.NewRegistry(viper.New())); err != nil {
		t.Fatalf("Provide: %v", err)
	}
	return m
}
