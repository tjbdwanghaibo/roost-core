// RR-20261004-NC-04：Shutdown 被取消仍保留 server，直到请求排空成功。
package ops

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/health"
)

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
	mod := &OpsMod{server: server}
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
	mod := &OpsMod{server: &http.Server{}}
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
	mod.setReady(true, "ready")
	recorder := httptest.NewRecorder()
	mod.handleReady(recorder, httptest.NewRequest("GET", "/readyz", nil))
	if recorder.Code != 503 {
		t.Fatalf("failed dependency status=%d", recorder.Code)
	}
}

func ownershipBusyServer(t *testing.T) (*OpsMod, func(), <-chan struct{}, *atomic.Bool) {
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
	mod := &OpsMod{enabled: true, addr: "127.0.0.1:0", server: server}
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
	mod := &OpsMod{enabled: true, addr: "127.0.0.1:0"}
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
