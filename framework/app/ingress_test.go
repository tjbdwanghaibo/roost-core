// 停机第一步关外部输入（App 生命周期方案 docs/maintenance/FEATURE-2026-11-04-app-lifecycle.md）：
// 停机开始之后到达的请求不再进 handler、答“没执行”；已进 handler 的答完才停业务与依赖；
// 等在途不占某个 Mod 的停机份额。

package app

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/internal/operation"
)

// 请求的答复分类（与 servicerpc.Classify 的口径一致：retryable 是确定没执行）。
const (
	answerOK        = "ok"
	answerRetryable = "retryable: not executed"
	answerNoHandler = "no handler"
)

// fakeIngressBus 是测试里的“总线”Mod：Service 在 Init 里往它上面登记 handler（像生成的 Server），deliver 模拟一个
// 外部请求到达。准入与在途计数用 operation.Lifetime；CloseIngress 关准入、在 ctx 内等在途（与 Bus.CloseIngress 同形）。
// 它是共享 Mod、排在最前（像 nats Mod），所以按依赖逆序它最后停。
type fakeIngressBus struct {
	calls   operation.Lifetime
	mu      sync.Mutex
	handler func() string
	stopped atomic.Bool
}

func (b *fakeIngressBus) Name() ModName           { return "fake_bus" }
func (b *fakeIngressBus) Init(*viper.Viper) error { return nil }
func (b *fakeIngressBus) Provide(*Registry) error { return nil }
func (b *fakeIngressBus) Start() error            { return nil }
func (b *fakeIngressBus) Stop()                   { b.stopped.Store(true) }

func (b *fakeIngressBus) CloseIngress(ctx context.Context) error {
	b.calls.Stop()
	return b.calls.Wait(ctx)
}

func (b *fakeIngressBus) handle(handler func() string) {
	b.mu.Lock()
	b.handler = handler
	b.mu.Unlock()
}

func (b *fakeIngressBus) deliver() string {
	if b.stopped.Load() || !b.calls.Begin() {
		return answerRetryable
	}
	defer b.calls.End()
	b.mu.Lock()
	handler := b.handler
	b.mu.Unlock()
	if handler == nil {
		return answerNoHandler
	}
	return handler()
}

// ingressStoreMod 是 handler 用到的存储：登记在总线之后、不依赖它（像排在 nats 之后的 etcd / mongo），所以先于总线停止。
type ingressStoreMod struct {
	name    ModName
	stopped atomic.Bool
	onStop  func()
}

func (m *ingressStoreMod) Name() ModName           { return m.name }
func (m *ingressStoreMod) Init(*viper.Viper) error { return nil }
func (m *ingressStoreMod) Provide(*Registry) error { return nil }
func (m *ingressStoreMod) Start() error            { return nil }
func (m *ingressStoreMod) Stop() {
	m.stopped.Store(true)
	if m.onStop != nil {
		m.onStop() // 存储刚停：这一刻到达的请求若进了 handler，就碰到已停的存储
	}
}

// ingressService 在 Init 里往总线登记 handler，Shutdown 不撤（与生成的 Server 相同）。
type ingressService struct {
	bus        *fakeIngressBus
	handler    func() string
	serving    chan struct{}
	onShutdown func()
}

func (s *ingressService) Name() ServiceName { return "game" }
func (s *ingressService) Init(*Registry) error {
	s.bus.handle(s.handler)
	return nil
}
func (s *ingressService) Serve(ctx context.Context) error {
	close(s.serving)
	<-ctx.Done()
	return nil
}
func (s *ingressService) Shutdown(context.Context) error {
	if s.onShutdown != nil {
		s.onShutdown()
	}
	return nil
}

func runIngressApp(t *testing.T, a *App, svc *ingressService, onServing func(signals chan<- os.Signal)) error {
	t.Helper()
	a.cfg.Set("log.file", false)
	a.cfg.Set("log.dir", t.TempDir())
	a.RootCmd().SetArgs([]string{"game"})
	signals := make(chan os.Signal, 1)
	a.signalSource = func() (<-chan os.Signal, func()) { return signals, func() {} }
	go func() {
		<-svc.serving
		onServing(signals)
	}()
	return a.Execute()
}

// 停机开始之后（Service.Shutdown 时、存储 Mod 停止时）到达的请求必须答“没执行、可重试”，不能再进 handler：
// handler 在 Service.Init 里注册、Service.Shutdown 不撤，存储先于总线停止，没有统一的第一步时，存储停止那一刻到达的
// 请求进 handler、碰到已停的存储（缺口 2；模块消息同理，缺口 5）。
func TestRequestsAfterShutdownBeginsNeverReachTheHandler(t *testing.T) {
	bus := &fakeIngressBus{}
	store := &ingressStoreMod{name: "store"}
	var atShutdown, atStoreStop string
	svc := &ingressService{bus: bus, serving: make(chan struct{})}
	svc.handler = func() string {
		if store.stopped.Load() {
			return "internal: store stopped"
		}
		return answerOK
	}
	svc.onShutdown = func() { atShutdown = bus.deliver() }
	store.onStop = func() { atStoreStop = bus.deliver() }

	a := New("roost-test", "0.0.0")
	a.Mods(bus, store)
	a.RegisterServer("game", svc)
	var before string
	err := runIngressApp(t, a, svc, func(signals chan<- os.Signal) {
		before = bus.deliver()
		signals <- os.Interrupt
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if before != answerOK {
		t.Fatalf("a request while serving was answered %q, want %q", before, answerOK)
	}
	if atShutdown != answerRetryable {
		t.Errorf("a request arriving during Service.Shutdown was answered %q; want %q: shutdown must close the inputs first", atShutdown, answerRetryable)
	}
	if atStoreStop != answerRetryable {
		t.Errorf("a request arriving while the store Mod stopped was answered %q; want %q: it reached the handler after shutdown began", atStoreStop, answerRetryable)
	}
}

// withdrawingOwnerMod 是 4947c62a 起生成的 owner Mod 的停法：自己的 StopWithContext 里撤 handler、等在途。
type withdrawingOwnerMod struct {
	bus     *fakeIngressBus
	stopErr atomic.Pointer[error]
}

func (m *withdrawingOwnerMod) Name() ModName           { return "owner" }
func (m *withdrawingOwnerMod) Init(*viper.Viper) error { return nil }
func (m *withdrawingOwnerMod) Provide(*Registry) error { return nil }
func (m *withdrawingOwnerMod) Start() error            { return nil }
func (m *withdrawingOwnerMod) Stop()                   {}
func (m *withdrawingOwnerMod) StopWithContext(ctx context.Context) error {
	err := m.bus.CloseIngress(ctx)
	m.stopErr.Store(&err)
	return err
}

// 停机开始时在途的请求要在停机预算内答完，然后全部 Mod 照常停下：等在途的时间不能占某一个 Mod（owner Mod）的
// 停机份额——总预算 2s 够等 1.2s 的在途再停完全部 Mod，但 owner Mod 只分到约 1s，在自己的份额里等就超时，
// App 随即不再停后面的 Mod（缺口 3）。
func TestWaitingForInFlightRequestsIsNotChargedToOneModsShare(t *testing.T) {
	bus := &fakeIngressBus{}
	store := &declaringIngressStoreMod{ingressStoreMod: ingressStoreMod{name: "store"}, declared: 500 * time.Millisecond}
	owner := &withdrawingOwnerMod{bus: bus}
	entered := make(chan struct{})
	svc := &ingressService{bus: bus, serving: make(chan struct{})}
	svc.handler = func() string {
		close(entered)
		time.Sleep(1200 * time.Millisecond)
		if store.stopped.Load() {
			return "internal: store stopped"
		}
		return answerOK
	}

	a := New("roost-test", "0.0.0")
	a.Mods(bus, store)
	a.RegisterServer("game", svc, owner)
	a.cfg.Set("shutdown.total_timeout", "2s")
	inFlight := make(chan string, 1)
	err := runIngressApp(t, a, svc, func(signals chan<- os.Signal) {
		go func() { inFlight <- bus.deliver() }()
		<-entered
		signals <- os.Interrupt
	})
	if err != nil {
		t.Errorf("Execute: %v; want every Mod stopped within shutdown.total_timeout", err)
	}
	if got := <-inFlight; got != answerOK {
		t.Errorf("the request in flight when the signal arrived was answered %q, want %q", got, answerOK)
	}
	if !store.stopped.Load() || !bus.stopped.Load() {
		t.Errorf("store stopped=%v bus stopped=%v; want both: waiting for the in-flight request ran out the owner Mod's share and the App kept the rest",
			store.stopped.Load(), bus.stopped.Load())
	}
	if p := owner.stopErr.Load(); p != nil && *p != nil && !errors.Is(*p, context.DeadlineExceeded) {
		t.Errorf("owner Mod stop: %v", *p)
	}
}

type declaringIngressStoreMod struct {
	ingressStoreMod
	declared time.Duration
}

func (m *declaringIngressStoreMod) StopBudget() time.Duration { return m.declared }

// 输入没在停机预算内排空（在途 handler 不返回）：handler 仍在用依赖，App 不调 Service.Shutdown、不停任何 Mod，
// 按停机不完整返回（与 Service.Shutdown 不完整的处理相同）。
func TestInputsThatDoNotDrainKeepTheServiceAndTheMods(t *testing.T) {
	bus := &fakeIngressBus{}
	store := &ingressStoreMod{name: "store"}
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(release) })
	svc := &ingressService{bus: bus, serving: make(chan struct{})}
	svc.handler = func() string { close(entered); <-release; return answerOK }
	var shutdownCalled atomic.Bool
	svc.onShutdown = func() { shutdownCalled.Store(true) }
	a := New("roost-test", "0.0.0")
	a.Mods(bus, store)
	a.RegisterServer("game", svc)
	a.cfg.Set("shutdown.total_timeout", "200ms")
	err := runIngressApp(t, a, svc, func(signals chan<- os.Signal) {
		go bus.deliver()
		<-entered
		signals <- os.Interrupt
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Execute = %v, want the shutdown deadline", err)
	}
	if shutdownCalled.Load() || store.stopped.Load() || bus.stopped.Load() {
		t.Fatalf("Service.Shutdown=%v store stopped=%v bus stopped=%v while a handler was still running; want everything kept",
			shutdownCalled.Load(), store.stopped.Load(), bus.stopped.Load())
	}
}

// 运行中才建立的输入（Service.Init 里订阅的消费者）经 AddIngress 登记，在停机第一步、Service.Shutdown 之前关闭；
// 第一步开始之后再登记被拒绝（App 不会再关它）。
func TestAddIngressIsClosedBeforeServiceShutdown(t *testing.T) {
	bus := &fakeIngressBus{}
	var order []string
	var mu sync.Mutex
	note := func(step string) { mu.Lock(); order = append(order, step); mu.Unlock() }
	svc := &ingressService{bus: bus, serving: make(chan struct{})}
	var registry *Registry
	svc.handler = func() string { return answerOK }
	var lateErr error
	svc.onShutdown = func() {
		note("service shutdown")
		lateErr = AddIngress(registry, "late", IngressFunc(func(context.Context) error { return nil }))
	}
	a := New("roost-test", "0.0.0")
	a.Mods(bus, &registryProbeMod{onProvide: func(r *Registry) error {
		registry = r
		return AddIngress(r, "consumer", IngressFunc(func(context.Context) error { note("consumer closed"); return nil }))
	}})
	a.RegisterServer("game", svc)
	if err := runIngressApp(t, a, svc, func(signals chan<- os.Signal) { signals <- os.Interrupt }); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(order) != 2 || order[0] != "consumer closed" || order[1] != "service shutdown" {
		t.Fatalf("order = %v, want the registered input closed before Service.Shutdown", order)
	}
	if !errors.Is(lateErr, errIngressClosed) {
		t.Fatalf("AddIngress after the inputs closed = %v, want errIngressClosed", lateErr)
	}
	if err := AddIngress(registry, "consumer", IngressFunc(func(context.Context) error { return nil })); err == nil {
		t.Fatal("AddIngress accepted a duplicate name")
	}
}

type registryProbeMod struct {
	onProvide func(*Registry) error
}

func (m *registryProbeMod) Name() ModName             { return "registry_probe" }
func (m *registryProbeMod) Init(*viper.Viper) error   { return nil }
func (m *registryProbeMod) Provide(r *Registry) error { return m.onProvide(r) }
func (m *registryProbeMod) Start() error              { return nil }
func (m *registryProbeMod) Stop()                     {}
