// RR-20261005-NC-80：正式 Ops 装配（Init → Provide → Start，真实 listener）里，管理命令已执行
// 但结果不可编码时返回 500，不能回 200 空体让运维以为成功。同一链路上不配合 context 的命令
// 占住请求时，Stop 超时保留 server，命令返回后重试排空成功（RR-20261004-NC-04 的组合控制）。
package ops

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/admin"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

const promiseAdminToken = "promise-admin-token"

func startAssembledOps(t *testing.T, register func(*admin.Registry)) (*OpsMod, string) {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()
	cfg := viper.New()
	cfg.Set("ops.enabled", true)
	cfg.Set("ops.addr", addr)
	cfg.Set("ops.admin_enabled", true)
	cfg.Set("ops.admin_token", promiseAdminToken)
	mod := NewOpsMod()
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	registry := app.NewRegistry(cfg)
	commands, _ := app.Lookup[*admin.Registry](registry, mods.ModAdmin)
	register(commands)
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mod.StopWithContext(context.Background()) })
	for deadline := time.Now().Add(3 * time.Second); ; {
		connection, err := net.Dial("tcp", addr)
		if err == nil {
			_ = connection.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ops listener did not come up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return mod, "http://" + addr
}

func executeAdmin(base, name string) (int, []byte, error) {
	request, _ := http.NewRequest(http.MethodPost, base+"/admin/execute", bytes.NewReader([]byte(`{"name":"`+name+`"}`)))
	request.Header.Set("X-Admin-Token", promiseAdminToken)
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	return response.StatusCode, body, err
}

func TestAdminResultThatCannotBeEncodedIsNotReportedAsSuccess(t *testing.T) {
	var executed atomic.Int32
	_, base := startAssembledOps(t, func(commands *admin.Registry) {
		_ = commands.Register(admin.CommandDef{Name: "promise.ratio", Handler: func(context.Context, admin.Command) (admin.Result, error) {
			executed.Add(1)
			return admin.Result{Data: map[string]any{"ratio": math.NaN()}}, nil
		}})
	})
	status, body, err := executeAdmin(base, "promise.ratio")
	if err != nil {
		t.Fatal(err)
	}
	if executed.Load() != 1 || status != http.StatusInternalServerError {
		t.Fatalf("executed=%d status=%d body=%q, want one execution answered 500", executed.Load(), status, body)
	}
}

func TestNonCooperativeAdminCommandStopRetryDrains(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	mod, base := startAssembledOps(t, func(commands *admin.Registry) {
		_ = commands.Register(admin.CommandDef{Name: "promise.block", Handler: func(context.Context, admin.Command) (admin.Result, error) {
			close(entered)
			<-release // ignores its context on purpose
			return admin.Result{Message: "done"}, nil
		}})
	})
	answered := make(chan int, 1)
	go func() {
		status, _, err := executeAdmin(base, "promise.block")
		if err != nil {
			status = -1
		}
		answered <- status
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := mod.StopWithContext(ctx); !errors.Is(err, context.DeadlineExceeded) || mod.ListenAddr() == "" {
		t.Fatalf("first stop = %v, server kept = %v", err, mod.ListenAddr() != "")
	}
	close(release)
	if status := <-answered; status != http.StatusOK {
		t.Fatalf("in-flight command answered %d", status)
	}
	if err := mod.StopWithContext(context.Background()); err != nil || mod.ListenAddr() != "" {
		t.Fatalf("retry stop = %v, server kept = %v", err, mod.ListenAddr() != "")
	}
	if _, _, err := executeAdmin(base, "promise.block"); err == nil {
		t.Fatal("listener still accepts after a drained stop")
	}
}
