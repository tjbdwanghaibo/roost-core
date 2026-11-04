package ops

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/admin"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/health"
)

// Shutdown 被取消仍保留 server，直到请求排空成功。历史：RR-20261004-NC-04。
type ownershipListener struct {
	net.Listener
	closed chan struct{}
	once   sync.Once
}

func (l *ownershipListener) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() { close(l.closed) })
	return err
}

func TestInterruptedShutdownKeepsServerOwnership(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &ownershipListener{Listener: listener, closed: make(chan struct{})}
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	var active atomic.Bool
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		active.Store(true)
		close(entered)
		<-release
		active.Store(false)
		_, _ = io.WriteString(w, "done")
	})}
	mod := &Server{server: server}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(wrapped) }()
	defer func() { releaseOnce.Do(func() { close(release) }); _ = server.Close() }()
	requestDone := make(chan error, 1)
	go func() {
		response, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + listener.Addr().String())
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		requestDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not enter owned server")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- mod.StopWithContext(ctx) }()
	select {
	case <-wrapped.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown did not begin closing listener")
	}
	cancel() // after Shutdown has begun; the handler is still held at the gate
	first := <-stopped
	if !errors.Is(first, context.Canceled) {
		t.Fatalf("expected interrupted Shutdown, got %v", first)
	}
	t.Logf("first=%v server_retained=%v handler_active=%v", first, mod.server != nil, active.Load())
	if mod.server == nil {
		second := mod.StopWithContext(context.Background())
		t.Errorf("lost in-flight server: retry=%v handler_active=%v", second, active.Load())
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-requestDone; err != nil {
		t.Fatal(err)
	}
	if err := mod.StopWithContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if active.Load() {
		t.Fatal("handler still active after successful shutdown")
	}
	if err := <-serveDone; !errors.Is(err, http.ErrServerClosed) {
		t.Fatal(err)
	}
}

func TestIdleShutdownAndReadinessControls(t *testing.T) {
	mod := &Server{server: &http.Server{}}
	if err := mod.StopWithContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mod.server != nil {
		t.Fatal("completed shutdown did not release server")
	}
	if err := mod.StopWithContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	mod.health = health.NewRegistry()
	mod.health.Register("dependency", health.CheckerFunc(func(context.Context) health.Result {
		return health.Result{Status: health.StatusFail, Err: errors.New("unavailable")}
	}))
	mod.SetReady(true, "ready")
	recorder := httptest.NewRecorder()
	mod.handleReady(recorder, httptest.NewRequest("GET", "/readyz", nil))
	if recorder.Code != 503 {
		t.Fatalf("failed dependency status=%d", recorder.Code)
	}
}

func ownershipBusyServer(t *testing.T) (*Server, func(), <-chan struct{}, *atomic.Bool) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &ownershipListener{Listener: listener, closed: make(chan struct{})}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	releaseHandler := func() { once.Do(func() { close(release) }) }
	active := &atomic.Bool{}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		active.Store(true)
		close(entered)
		<-release
		active.Store(false)
		_, _ = io.WriteString(w, "done")
	})}
	mod := &Server{enabled: true, addr: "127.0.0.1:0", server: server}
	serveDone, requestDone := make(chan error, 1), make(chan error, 1)
	go func() { serveDone <- server.Serve(wrapped) }()
	go func() {
		response, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + listener.Addr().String())
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		requestDone <- err
	}()
	t.Cleanup(func() {
		releaseHandler()
		_ = server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = mod.StopWithContext(ctx)
		select {
		case <-requestDone:
		case <-ctx.Done():
			t.Error("request cleanup timed out")
		}
		select {
		case <-serveDone:
		case <-ctx.Done():
			t.Error("Serve cleanup timed out")
		}
	})
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not enter")
	}
	return mod, releaseHandler, wrapped.closed, active
}

func TestShutdownDeadlineRetainsServerUntilRetryDrains(t *testing.T) {
	mod, release, closed, active := ownershipBusyServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := mod.StopWithContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown=%v", err)
	}
	select {
	case <-closed:
	default:
		t.Fatal("Shutdown never closed listener")
	}
	if !active.Load() || mod.server == nil {
		t.Fatal("deadline lost in-flight server")
	}
	if err := mod.Start(); err == nil {
		t.Fatal("Start replaced server still awaiting drain")
	}
	release()
	if err := mod.StopWithContext(context.Background()); err != nil || active.Load() {
		t.Fatalf("retry=%v active=%v", err, active.Load())
	}
	if err := mod.Start(); err != nil {
		t.Fatalf("fresh server after complete drain=%v", err)
	}
	if err := mod.StopWithContext(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentShutdownCallersKeepTheirContexts(t *testing.T) {
	mod, release, closed, active := ownershipBusyServer(t)
	first := make(chan error, 1)
	go func() { first <- mod.StopWithContext(context.Background()) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown did not begin")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	second := make(chan error, 1)
	go func() { second <- mod.StopWithContext(ctx) }()
	select {
	case err := <-second:
		if !errors.Is(err, context.Canceled) || !active.Load() {
			t.Fatalf("independent cancellation=%v active=%v", err, active.Load())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled Stop blocked behind the draining caller")
	}
	if err := mod.Start(); err == nil {
		t.Fatal("Start replaced a draining server")
	}
	release()
	if err := <-first; err != nil || active.Load() {
		t.Fatalf("first shutdown=%v active=%v", err, active.Load())
	}
	if err := mod.StopWithContext(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentOpsStartStopUsesCapturedServer(t *testing.T) {
	mod := &Server{enabled: true, addr: "127.0.0.1:0"}
	var group sync.WaitGroup
	for range 16 {
		group.Go(func() { _ = mod.Start() })
		group.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := mod.StopWithContext(ctx); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	if err := mod.StopWithContext(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// 停机第一步（App 经 app.IngressCloser 关输入，App 生命周期方案缺口 7）：之后的 /admin/execute 不执行、答 503，
// 已在执行的命令先跑完 CloseIngress 才返回；健康 / 就绪照常。没有这一步时 ops 是共享 Mod、停得晚，停机期间的运维命令
// （例如联盟的 alliance.transfer）照常执行，落在已停的 Nest / Remote 上。
func TestCloseIngressRefusesAdminCommandsAndWaitsForTheRunningOne(t *testing.T) {
	m := &Server{adminEnabled: true, adminToken: "b7", commands: admin.NewRegistry()}
	entered, release := make(chan struct{}), make(chan struct{})
	var runs atomic.Int32
	if err := m.commands.Register(admin.CommandDef{Name: "transfer", Handler: func(context.Context, admin.Command) (admin.Result, error) {
		if runs.Add(1) == 1 {
			close(entered)
			<-release
		}
		return admin.Result{OK: true}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	execute := func() int {
		r := httptest.NewRequest(http.MethodPost, "/admin/execute", strings.NewReader(`{"name":"transfer"}`))
		r.Header.Set("Authorization", "Bearer b7")
		w := httptest.NewRecorder()
		m.handleAdminExecute(w, r)
		return w.Code
	}
	running := make(chan int, 1)
	go func() { running <- execute() }()
	<-entered
	closer, ok := any(m).(interface{ CloseIngress(context.Context) error })
	if !ok {
		close(release)
		t.Fatal("ops Server has no CloseIngress: admin commands keep running while the App stops the Mods they use")
	}
	closed := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		closed <- closer.CloseIngress(ctx)
	}()
	select {
	case err := <-closed:
		t.Fatalf("CloseIngress returned %v while a command was still running", err)
	case <-time.After(50 * time.Millisecond):
	}
	if code := execute(); code != http.StatusServiceUnavailable {
		t.Errorf("a command after CloseIngress answered %d, want 503 (not executed)", code)
	}
	close(release)
	if err := <-closed; err != nil {
		t.Fatalf("CloseIngress: %v", err)
	}
	if code := <-running; code != http.StatusOK {
		t.Errorf("the running command answered %d, want 200", code)
	}
	if n := runs.Load(); n != 1 {
		t.Errorf("command ran %d times, want only the one accepted before CloseIngress", n)
	}
	w := httptest.NewRecorder()
	m.handleHealth(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Errorf("/healthz after CloseIngress = %d, want 200", w.Code)
	}
}
