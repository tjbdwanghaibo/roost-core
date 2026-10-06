package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/health"
)

// 单实例锁回归（docs/feature/APP-SINGLETON-LOCK-2026-10-05.md §8.1）。
//
// 同一进程里不能并发跑两个 App.run（flog / metrics / fctx / clock 都是进程级全局量），所以
// “另一个实例”一律用预置了别人值的假 store 模拟，每个用例只跑一个 App.run。时间全部走可控的
// fakeClock：锁的节拍、等待与窗口只读这个时钟，用例显式推进，不用 sleep 制造时序。

const (
	testSingletonPrefix = "roost:test:singleton"
	testSingletonKey    = testSingletonPrefix + ":game:1000"
	testSingletonTTL    = 15 * time.Second
	testSingletonRenew  = 3 * time.Second
	testSingletonGuard  = 5 * time.Second
	testSingletonWait   = 30 * time.Second
	testOtherHolder     = "other-token|other-host|4242|1"
	testWaitLimit       = 5 * time.Second
)

// fakeClock 是可控的单调时钟。Timer 登记的定时器只在 Advance 推进到期时触发。
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	timers  []*fakeTimer
	changed chan struct{} // 定时器集合变化时关闭并替换，供 timerReady 等待
}

type fakeTimer struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1_000_000, 0), changed: make(chan struct{})}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Timer(d time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &fakeTimer{at: c.now.Add(d), ch: make(chan time.Time, 1)}
	if d <= 0 {
		timer.ch <- c.now
		return timer.ch, func() {}
	}
	c.timers = append(c.timers, timer)
	c.notifyLocked()
	return timer.ch, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		for i, pending := range c.timers {
			if pending == timer {
				c.timers = append(c.timers[:i], c.timers[i+1:]...)
				c.notifyLocked()
				return
			}
		}
	}
}

func (c *fakeClock) notifyLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// Advance 把时钟推进 d，触发所有到期的定时器。
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.timers[:0]
	for _, timer := range c.timers {
		if timer.at.After(c.now) {
			kept = append(kept, timer)
			continue
		}
		timer.ch <- c.now
	}
	c.timers = kept
	c.notifyLocked()
}

// nextDeadline 返回最早的待触发定时器的到期时间。
func (c *fakeClock) nextDeadline() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.timers) == 0 {
		return time.Time{}, false
	}
	earliest := c.timers[0].at
	for _, timer := range c.timers[1:] {
		if timer.at.Before(earliest) {
			earliest = timer.at
		}
	}
	return earliest, true
}

// AdvanceToNext 推进到最早的定时器到期，返回推进的时长。
func (c *fakeClock) AdvanceToNext(t *testing.T) time.Duration {
	t.Helper()
	at, ok := c.nextDeadline()
	if !ok {
		t.Fatal("fake clock: no pending timer to advance to")
	}
	d := at.Sub(c.Now())
	c.Advance(d)
	return d
}

// timerReady 返回一个在至少有 n 个待触发定时器时关闭的通道。
func (c *fakeClock) timerReady(n int) <-chan struct{} {
	ready := make(chan struct{})
	go func() {
		for {
			c.mu.Lock()
			count, changed := len(c.timers), c.changed
			c.mu.Unlock()
			if count >= n {
				close(ready)
				return
			}
			<-changed
		}
	}()
	return ready
}

func (c *fakeClock) pendingTimers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// fakeSingletonStore 模拟 Redis 单键 CAS：值按 fakeClock 过期。onCAS 可以替换一次调用的结果：
// apply 执行真实的比较并写入，钩子可以不调用它（没落地的报错）、调用后返回错误（落地但丢回复），
// 或在调用前后推进时钟（处理慢 / 回复迟到）。
type fakeSingletonStore struct {
	clock *fakeClock

	mu       sync.Mutex
	entries  map[string]fakeEntry
	casCalls []fakeCASCall
	deletes  []fakeCASCall
	gets     [][]string
	getErr   error
	closed   bool
	onCAS    func(call fakeCASCall, apply func() (bool, []byte)) (bool, []byte, error)
	// 业务时间高水位键（<prefix>:business_time）的调用单独记账：不进 casCalls、不走 onCAS，
	// 单实例锁的用例只看锁键；markErr 非 nil 时高水位的 CAS 报这个错。
	markCalls []fakeCASCall
	markErr   error
}

type fakeEntry struct {
	value   []byte
	expires time.Time // 零值表示不过期
}

type fakeCASCall struct {
	key      string
	expected []byte
	next     []byte
	ttl      time.Duration
	at       time.Time
	// timeout 是调用方 ctx 在进入调用时剩下的时长（没有截止时间为 0）；钩子用它模拟“一直没有回复，
	// 直到单次超时到期”。
	timeout time.Duration
}

func newFakeSingletonStore(clock *fakeClock) *fakeSingletonStore {
	return &fakeSingletonStore{clock: clock, entries: make(map[string]fakeEntry)}
}

func (s *fakeSingletonStore) set(key, value string, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := fakeEntry{value: []byte(value)}
	if ttl > 0 {
		entry.expires = s.clock.Now().Add(ttl)
	}
	s.entries[key] = entry
}

func (s *fakeSingletonStore) remove(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, key)
}

func (s *fakeSingletonStore) valueLocked(key string) []byte {
	entry, ok := s.entries[key]
	if !ok {
		return nil
	}
	if !entry.expires.IsZero() && !s.clock.Now().Before(entry.expires) {
		delete(s.entries, key)
		return nil
	}
	return entry.value
}

func (s *fakeSingletonStore) value(key string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.valueLocked(key)...)
}

func (s *fakeSingletonStore) setOnCAS(hook func(call fakeCASCall, apply func() (bool, []byte)) (bool, []byte, error)) {
	s.mu.Lock()
	s.onCAS = hook
	s.mu.Unlock()
}

func (s *fakeSingletonStore) calls() []fakeCASCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]fakeCASCall(nil), s.casCalls...)
}

func (s *fakeSingletonStore) releaseCalls() []fakeCASCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]fakeCASCall(nil), s.deletes...)
}

func (s *fakeSingletonStore) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *fakeSingletonStore) CompareAndSet(ctx context.Context, key string, expected, next []byte, ttl time.Duration) (bool, []byte, error) {
	s.mu.Lock()
	call := fakeCASCall{key: key, expected: append([]byte(nil), expected...), next: append([]byte(nil), next...), ttl: ttl, at: s.clock.Now()}
	if deadline, ok := ctx.Deadline(); ok {
		call.timeout = time.Until(deadline)
	}
	if expected == nil {
		call.expected = nil
	}
	isMark := strings.HasSuffix(key, businessTimeKeySuffix)
	var hook func(call fakeCASCall, apply func() (bool, []byte)) (bool, []byte, error)
	if isMark {
		s.markCalls = append(s.markCalls, call)
		if s.markErr != nil {
			err := s.markErr
			s.mu.Unlock()
			return false, nil, err
		}
	} else {
		s.casCalls = append(s.casCalls, call)
		hook = s.onCAS
	}
	s.mu.Unlock()
	apply := func() (bool, []byte) {
		s.mu.Lock()
		defer s.mu.Unlock()
		current := s.valueLocked(key)
		if expected == nil {
			if current != nil {
				return false, append([]byte(nil), current...)
			}
		} else if current == nil || !bytes.Equal(current, expected) {
			return false, append([]byte(nil), current...)
		}
		entry := fakeEntry{value: append([]byte(nil), next...)}
		if ttl > 0 { // 与 redis.CompareAndSet 一致：ttl 为 0 是不过期的 SET
			entry.expires = s.clock.Now().Add(ttl)
		}
		s.entries[key] = entry
		return true, append([]byte(nil), next...)
	}
	if hook != nil {
		return hook(call, apply)
	}
	applied, current := apply()
	return applied, current, nil
}

func (s *fakeSingletonStore) CompareAndDelete(_ context.Context, key string, expected []byte) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes = append(s.deletes, fakeCASCall{key: key, expected: append([]byte(nil), expected...), at: s.clock.Now()})
	current := s.valueLocked(key)
	if current == nil || !bytes.Equal(current, expected) {
		return false, nil
	}
	delete(s.entries, key)
	return true, nil
}

func (s *fakeSingletonStore) Get(_ context.Context, keys []string) ([][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets = append(s.gets, append([]string(nil), keys...))
	if s.closed {
		return nil, errFakeStoreClosed // 与真实客户端一致：关闭之后的调用报错
	}
	if s.getErr != nil {
		return nil, s.getErr
	}
	out := make([][]byte, len(keys))
	for i, key := range keys {
		if value := s.valueLocked(key); value != nil {
			out[i] = append([]byte(nil), value...)
		}
	}
	return out, nil
}

var errFakeStoreClosed = errors.New("fake singleton store: client is closed")

func (s *fakeSingletonStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// singletonProbeMod 记录生命周期调用；onStop 在 Stop 时执行（检查那一刻锁的状态）。
type singletonProbeMod struct {
	name     ModName
	inits    atomic.Int32
	starts   atomic.Int32
	stops    atomic.Int32
	startErr error
	onStart  func()
	onStop   func()
	registry *Registry
}

func (m *singletonProbeMod) Name() ModName { return m.name }
func (m *singletonProbeMod) Init(*viper.Viper) error {
	m.inits.Add(1)
	return nil
}
func (m *singletonProbeMod) Provide(r *Registry) error {
	m.registry = r
	return nil
}
func (m *singletonProbeMod) Start() error {
	m.starts.Add(1)
	if m.onStart != nil {
		m.onStart()
	}
	return m.startErr
}
func (m *singletonProbeMod) Stop() {
	m.stops.Add(1)
	if m.onStop != nil {
		m.onStop()
	}
}

// singletonProbeService 在 Serve 开始时关闭 served，阻塞到 ctx 取消。
type singletonProbeService struct {
	served     chan struct{}
	serveOnce  sync.Once
	initErr    error
	onInit     func(*Registry) error
	onShutdown func(context.Context) error
	registry   *Registry
}

func newSingletonProbeService() *singletonProbeService {
	return &singletonProbeService{served: make(chan struct{})}
}

func (s *singletonProbeService) Name() ServiceName { return "game" }
func (s *singletonProbeService) Init(r *Registry) error {
	s.registry = r
	if s.onInit != nil {
		if err := s.onInit(r); err != nil {
			return err
		}
	}
	return s.initErr
}
func (s *singletonProbeService) Serve(ctx context.Context) error {
	s.serveOnce.Do(func() { close(s.served) })
	<-ctx.Done()
	return nil
}
func (s *singletonProbeService) Shutdown(ctx context.Context) error {
	if s.onShutdown != nil {
		return s.onShutdown(ctx)
	}
	return nil
}

type singletonHarness struct {
	app      *App
	clock    *fakeClock
	store    *fakeSingletonStore
	svc      *singletonProbeService
	signals  chan os.Signal
	opens    atomic.Int32
	t        *testing.T
	finished chan struct{}
}

func newSingletonHarness(t *testing.T, sharedMods []Mod, serviceMods []Mod) *singletonHarness {
	t.Helper()
	clock := newFakeClock()
	h := &singletonHarness{
		clock:   clock,
		store:   newFakeSingletonStore(clock),
		svc:     newSingletonProbeService(),
		signals: make(chan os.Signal, 1),
		t:       t,
	}
	a := New("roost-test", "0.0.0")
	if len(sharedMods) > 0 {
		a.Mods(sharedMods...)
	}
	a.RegisterServer("game", h.svc, serviceMods...)
	a.cfg.Set("log.file", false)
	a.cfg.Set("log.stdout", false)
	a.cfg.Set("log.dir", t.TempDir())
	a.cfg.Set("singleton.enabled", true)
	a.cfg.Set("singleton.key_prefix", testSingletonPrefix)
	a.cfg.Set("singleton.ttl", testSingletonTTL)
	a.cfg.Set("singleton.renew_interval", testSingletonRenew)
	a.cfg.Set("singleton.guard", testSingletonGuard)
	a.cfg.Set("singleton.startup_wait", testSingletonWait)
	a.singletonClock = clock
	a.Singleton(func(*viper.Viper) (SingletonStore, error) {
		h.opens.Add(1)
		return h.store, nil
	})
	a.signalSource = func() (<-chan os.Signal, func()) { return h.signals, func() {} }
	a.RootCmd().SetArgs([]string{"game"})
	h.app = a
	return h
}

func (h *singletonHarness) start() <-chan error {
	result := make(chan error, 1)
	h.finished = make(chan struct{})
	go func() {
		defer close(h.finished)
		result <- h.app.Execute()
	}()
	// 用例提前失败时也要让 run 退出：下一个用例的 run 会改同一批进程级全局量。
	h.t.Cleanup(func() {
		select {
		case <-h.finished:
			return
		default:
		}
		select {
		case h.signals <- os.Interrupt:
		default:
		}
		select {
		case <-h.finished:
		case <-time.After(testWaitLimit):
			h.t.Log("run did not exit during cleanup")
		}
	})
	return result
}

// awaitServed 等 Serve 开始；run 先返回则报告它的错误。
func (h *singletonHarness) awaitServed(t *testing.T, result <-chan error) {
	t.Helper()
	select {
	case <-h.svc.served:
	case err := <-result:
		t.Fatalf("run returned before serving: %v", err)
	case <-time.After(testWaitLimit):
		t.Fatal("service did not start serving")
	}
}

// awaitWaiting 等锁的 goroutine 停在定时器上（启动等待或续期节拍）。期间 Serve 开始或 run 返回都算失败。
func (h *singletonHarness) awaitWaiting(t *testing.T, result <-chan error, expectServing bool) {
	t.Helper()
	served := h.svc.served
	if expectServing {
		served = nil
	}
	select {
	case <-h.clock.timerReady(1):
	case <-served:
		t.Fatal("service started serving while the singleton key belonged to another process")
	case err := <-result:
		t.Fatalf("run returned while it should be waiting on the singleton timer: %v", err)
	case <-time.After(testWaitLimit):
		t.Fatal("singleton lock never waited on its timer")
	}
}

// stop 发信号让 run 正常停机并返回它的结果。
func (h *singletonHarness) stop(t *testing.T, result <-chan error) error {
	t.Helper()
	h.signals <- os.Interrupt
	return awaitRunResult(t, result)
}

func awaitRunResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(testWaitLimit):
		t.Fatal("run did not return")
		return nil
	}
}

func (h *singletonHarness) runtimeFailure(t *testing.T) *RuntimeFailure {
	t.Helper()
	if h.svc.registry == nil {
		t.Fatal("service was not initialized")
	}
	return MustLookup[*RuntimeFailure](h.svc.registry, ModRuntimeFailure)
}

func (h *singletonHarness) status(t *testing.T) singletonStatus {
	t.Helper()
	if h.app.singleton == nil {
		t.Fatal("App did not install a singleton lock")
	}
	return h.app.singleton.snapshot()
}

func (h *singletonHarness) healthOf(t *testing.T) health.Result {
	t.Helper()
	if h.svc.registry == nil {
		t.Fatal("service was not initialized")
	}
	snapshot := MustLookup[*health.Registry](h.svc.registry, ModHealth).Snapshot(context.Background())
	for _, result := range snapshot.Results {
		if result.Name == "singleton" {
			return result
		}
	}
	t.Fatalf("no singleton health check registered: %+v", snapshot)
	return health.Result{}
}

func mustBeMine(t *testing.T, value []byte) {
	t.Helper()
	if len(value) == 0 || string(value) == testOtherHolder {
		t.Fatalf("singleton key holds %q, want this process's value", value)
	}
	if parts := strings.Split(string(value), "|"); len(parts) != 4 || len(parts[0]) != 16 {
		t.Fatalf("singleton value %q is not token|hostname|pid|started_unix_ms", value)
	}
}

// #1：键是别人的值时，run 在任何 Mod Init 之前等待；对方 Release 后拿到锁并完成启动。
func TestSingletonWaitsForTheHolderBeforeAnyModInit(t *testing.T) {
	shared := &singletonProbeMod{name: "probe_shared"}
	h := newSingletonHarness(t, []Mod{shared}, nil)
	h.store.set(testSingletonKey, testOtherHolder, 0)
	result := h.start()

	h.awaitWaiting(t, result, false)
	if n := shared.inits.Load(); n != 0 {
		t.Fatalf("mod Init ran %d times while another process held the key", n)
	}
	h.store.remove(testSingletonKey) // 对方正常停机 Release
	h.clock.AdvanceToNext(t)
	h.awaitServed(t, result)
	mustBeMine(t, h.store.value(testSingletonKey))
	if shared.inits.Load() != 1 || shared.starts.Load() != 1 {
		t.Fatalf("mod init=%d start=%d after acquiring", shared.inits.Load(), shared.starts.Load())
	}
	if err := h.stop(t, result); err != nil {
		t.Fatalf("run: %v", err)
	}
}

// #2：别人一直在续期：到 startup_wait 失败，错误带持有者的值；没有 Mod Init，键没被改动。
func TestSingletonGivesUpAtStartupWaitWithoutTakingTheKey(t *testing.T) {
	shared := &singletonProbeMod{name: "probe_shared"}
	h := newSingletonHarness(t, []Mod{shared}, nil)
	h.svc.initErr = errors.New("service must not start while another process holds the key")
	h.store.set(testSingletonKey, testOtherHolder, 0)
	started := h.clock.Now()
	result := h.start()

	var err error
	for done := false; !done; {
		select {
		case err = <-result:
			done = true
		case <-h.clock.timerReady(1):
			h.clock.AdvanceToNext(t)
		case <-time.After(testWaitLimit):
			t.Fatal("run neither waited nor returned")
		}
	}
	if !errors.Is(err, ErrSingletonHeld) || !strings.Contains(err.Error(), testOtherHolder) {
		t.Fatalf("run error = %v, want ErrSingletonHeld naming the holder", err)
	}
	if errors.Is(err, ErrSingletonStoreUnavailable) {
		t.Fatalf("a live holder was reported as an unavailable store: %v", err)
	}
	if waited := h.clock.Now().Sub(started); waited < testSingletonWait || waited >= testSingletonWait+testSingletonRenew {
		t.Fatalf("gave up after %s, want within [startup_wait, startup_wait+renew_interval)", waited)
	}
	if n := shared.inits.Load(); n != 0 {
		t.Fatalf("mod Init ran %d times", n)
	}
	if got := h.store.value(testSingletonKey); string(got) != testOtherHolder {
		t.Fatalf("holder's key changed to %q", got)
	}
	for _, call := range h.store.calls() {
		if call.expected != nil {
			t.Fatalf("startup wait issued a CAS against %q; it must only try to create the key", call.expected)
		}
	}
	if len(h.store.releaseCalls()) != 0 {
		t.Fatal("a process that never held the lock released it")
	}
	if !h.store.isClosed() {
		t.Fatal("store was not closed")
	}
}

// #3：别人的值在可控时钟走过 TTL 后过期：等待后接手。
func TestSingletonTakesOverAfterTheHolderExpires(t *testing.T) {
	h := newSingletonHarness(t, nil, nil)
	h.store.set(testSingletonKey, testOtherHolder, testSingletonTTL) // 对方已经卡住、不再续期
	// 拿锁之后的启动可能很慢（DataEngine 重放）：Service.Init 挡住，期间续期节拍照常登记。
	initGate := make(chan struct{})
	var openGate sync.Once
	h.svc.onInit = func(*Registry) error { <-initGate; return nil }
	started := h.clock.Now()
	result := h.start()
	t.Cleanup(func() { openGate.Do(func() { close(initGate) }) })
	// 只推进启动等待的定时器，键成了自己的就停：拿锁之后登记的是续期节拍，若一直推进到 Serve 开始，
	// 启动稍慢时时钟会被续期节拍推过 startup_wait，误报“没接手”。刚推进完 Acquire 的定时器、CAS 还没
	// 落地时可能多推进一个续期节拍，无害。
	for {
		if value := h.store.value(testSingletonKey); len(value) > 0 && string(value) != testOtherHolder {
			break
		}
		select {
		case <-h.clock.timerReady(1):
			if h.clock.Now().Sub(started) > testSingletonWait {
				t.Fatal("never took over the expired key")
			}
			h.clock.AdvanceToNext(t)
		case err := <-result:
			t.Fatalf("run returned before taking over: %v", err)
		case <-time.After(testWaitLimit):
			t.Fatal("run neither waited nor took over the key")
		}
	}
	openGate.Do(func() { close(initGate) })
	h.awaitServed(t, result)
	var acquiredAt time.Time
	for _, call := range h.store.calls() {
		if call.expected == nil {
			acquiredAt = call.at // 最后一次 Acquire 就是成功的那次
		}
	}
	if waited := acquiredAt.Sub(started); waited < testSingletonTTL || waited >= testSingletonTTL+testSingletonRenew {
		t.Fatalf("took over after %s, want within [ttl, ttl+renew_interval)", waited)
	}
	mustBeMine(t, h.store.value(testSingletonKey))
	if err := h.stop(t, result); err != nil {
		t.Fatalf("run: %v", err)
	}
}

// #3（第 4 步）：Acquire 一直报错到上限：错误是 store unavailable，不是“被持有”。
func TestSingletonReportsAnUnavailableStoreAtStartupWait(t *testing.T) {
	shared := &singletonProbeMod{name: "probe_shared"}
	h := newSingletonHarness(t, []Mod{shared}, nil)
	h.svc.initErr = errors.New("service must not start without the lock")
	storeDown := errors.New("dial tcp: connection refused")
	h.store.setOnCAS(func(fakeCASCall, func() (bool, []byte)) (bool, []byte, error) {
		return false, nil, storeDown
	})
	result := h.start()
	var err error
	for done := false; !done; {
		select {
		case err = <-result:
			done = true
		case <-h.clock.timerReady(1):
			h.clock.AdvanceToNext(t)
		case <-time.After(testWaitLimit):
			t.Fatal("run neither waited nor returned")
		}
	}
	if !errors.Is(err, ErrSingletonStoreUnavailable) || !errors.Is(err, storeDown) || errors.Is(err, ErrSingletonHeld) {
		t.Fatalf("run error = %v, want ErrSingletonStoreUnavailable wrapping the store error", err)
	}
	if n := shared.inits.Load(); n != 0 {
		t.Fatalf("mod Init ran %d times", n)
	}
}

// #4：Acquire 落地但回复丢失：重试时 current 等于自己的值 → 认领，并以随后那次 Renew 的 asked 起算窗口。
func TestSingletonClaimsItsOwnValueAfterALostAcquireReply(t *testing.T) {
	h := newSingletonHarness(t, nil, nil)
	var calls atomic.Int32
	h.store.setOnCAS(func(call fakeCASCall, apply func() (bool, []byte)) (bool, []byte, error) {
		switch calls.Add(1) {
		case 1: // 落地，但回复丢了
			apply()
			return false, nil, context.DeadlineExceeded
		case 2: // 第二次 Acquire 处理用了 1s；随后的认领 Renew 的 asked 因此晚于这次的 asked
			applied, current := apply()
			h.clock.Advance(time.Second)
			return applied, current, nil
		}
		applied, current := apply()
		return applied, current, nil
	})
	result := h.start()
	h.awaitWaiting(t, result, false)
	h.clock.AdvanceToNext(t)
	h.awaitServed(t, result)

	got := h.store.calls()
	if len(got) < 3 || got[0].expected != nil || got[1].expected != nil || !bytes.Equal(got[2].expected, got[0].next) {
		t.Fatalf("CAS sequence = %+v, want acquire, acquire, renew of the claimed value", got)
	}
	renewAsked := got[2].at
	if !renewAsked.After(got[1].at) {
		t.Fatalf("claim renew asked at %v, not after the second acquire %v", renewAsked, got[1].at)
	}
	if status := h.status(t); status.state != singletonHeld || !status.validUntil.Equal(renewAsked.Add(testSingletonTTL)) {
		t.Fatalf("status = %+v, want held with validUntil = renew asked + ttl (%v)", status, renewAsked.Add(testSingletonTTL))
	}
	h.awaitWaiting(t, result, true) // 续期 goroutine 异步登记它的第一个节拍
	if next, ok := h.clock.nextDeadline(); !ok || !next.Equal(renewAsked.Add(testSingletonRenew)) {
		t.Fatalf("next renewal beat = %v (pending=%v), want renew asked + renew_interval", next, ok)
	}
	if err := h.stop(t, result); err != nil {
		t.Fatalf("run: %v", err)
	}
}

// #4：丢回复之后 current 是别人的值 → 继续等，不认领；等待期间的信号让 run 直接退出。
func TestSingletonKeepsWaitingWhenALostReplyHidesAnotherHolder(t *testing.T) {
	shared := &singletonProbeMod{name: "probe_shared"}
	h := newSingletonHarness(t, []Mod{shared}, nil)
	var calls atomic.Int32
	h.store.setOnCAS(func(call fakeCASCall, apply func() (bool, []byte)) (bool, []byte, error) {
		if calls.Add(1) == 1 {
			h.store.set(testSingletonKey, testOtherHolder, 0) // 没落地；与此同时别人拿到了键
			return false, nil, context.DeadlineExceeded
		}
		applied, current := apply()
		return applied, current, nil
	})
	result := h.start()
	h.awaitWaiting(t, result, false)
	h.clock.AdvanceToNext(t)
	h.awaitWaiting(t, result, false)
	for _, call := range h.store.calls() {
		if call.expected != nil {
			t.Fatalf("issued a CAS against %q while another process held the key", call.expected)
		}
	}
	if shared.inits.Load() != 0 {
		t.Fatal("mod Init ran while another process held the key")
	}
	h.signals <- os.Interrupt
	if err := awaitRunResult(t, result); err != nil {
		t.Fatalf("signal while waiting: run error = %v, want a clean exit", err)
	}
	if shared.inits.Load() != 0 || string(h.store.value(testSingletonKey)) != testOtherHolder {
		t.Fatal("signal during the wait started mods or touched the holder's key")
	}
	if len(h.store.releaseCalls()) != 0 {
		t.Fatal("a process that never held the lock released it")
	}
}

// #5：Renew 答 NotHeld：RuntimeFailure 恰好一次，OnFail 回调先于 Done；Lost 吸收、不再续期、不 Release。
func TestSingletonNotHeldFailsOnceAndFencesBeforeShutdown(t *testing.T) {
	stoppedWhileOthers := atomic.Bool{}
	shared := &singletonProbeMod{name: "probe_shared"}
	h := newSingletonHarness(t, []Mod{shared}, nil)
	shared.onStop = func() { stoppedWhileOthers.Store(true) }
	var hookCalls atomic.Int32
	var doneQueuedAtHook atomic.Bool
	var hookRanBeforeShutdown atomic.Bool
	h.svc.onInit = func(r *Registry) error {
		failure := MustLookup[*RuntimeFailure](r, ModRuntimeFailure)
		failure.OnFail(func(error) {
			hookCalls.Add(1)
			doneQueuedAtHook.Store(len(failure.Done()) > 0)
		})
		return nil
	}
	h.svc.onShutdown = func(context.Context) error {
		hookRanBeforeShutdown.Store(hookCalls.Load() == 1)
		return nil
	}
	result := h.start()
	h.awaitServed(t, result)
	if got := h.healthOf(t); got.Status != health.StatusOK {
		t.Fatalf("held lock health = %+v", got)
	}
	h.awaitWaiting(t, result, true)
	h.store.set(testSingletonKey, testOtherHolder, 0) // 卡住期间键过期并被新进程拿走
	h.clock.AdvanceToNext(t)

	err := awaitRunResult(t, result)
	if !errors.Is(err, ErrSingletonLost) || !errors.Is(err, ErrSingletonNotHeld) {
		t.Fatalf("run error = %v, want ErrSingletonLost wrapping ErrSingletonNotHeld", err)
	}
	if n := hookCalls.Load(); n != 1 {
		t.Fatalf("OnFail hook ran %d times, want exactly once", n)
	}
	if doneQueuedAtHook.Load() {
		t.Fatal("Done was delivered before the OnFail hook ran")
	}
	if !hookRanBeforeShutdown.Load() {
		t.Fatal("Service.Shutdown ran before the OnFail hook (Nest would not be fenced yet)")
	}
	if !stoppedWhileOthers.Load() {
		t.Fatal("mods were not stopped after the lock was lost")
	}
	if status := h.status(t); status.state != singletonLost || !bytes.Equal(status.holder, []byte(testOtherHolder)) {
		t.Fatalf("status after NotHeld = %+v, want lost with the new holder", status)
	}
	if got := h.healthOf(t); got.Status != health.StatusFail {
		t.Fatalf("lost lock health = %+v", got)
	}
	calls := len(h.store.calls())
	h.clock.Advance(10 * testSingletonRenew)
	if len(h.store.calls()) != calls || h.clock.pendingTimers() != 0 {
		t.Fatal("renewal continued after Lost")
	}
	if len(h.store.releaseCalls()) != 0 || string(h.store.value(testSingletonKey)) != testOtherHolder {
		t.Fatal("a lost lock was released")
	}
}

// #6：连续 Unknown：窗口内不判 Lost；Lost 在 now ≥ validUntil − guard 之后的第一个节拍判定，且不晚于 validUntil。
func TestSingletonUnknownRenewalsLoseOnlyAtTheEndOfTheWindow(t *testing.T) {
	h := newSingletonHarness(t, nil, nil)
	var lostAt atomic.Pointer[time.Time]
	h.svc.onInit = func(r *Registry) error {
		MustLookup[*RuntimeFailure](r, ModRuntimeFailure).OnFail(func(error) {
			now := h.clock.Now()
			lostAt.Store(&now)
		})
		return nil
	}
	result := h.start()
	h.awaitServed(t, result)
	validUntil := h.status(t).validUntil
	if validUntil.IsZero() {
		t.Fatal("no validity window after acquiring")
	}
	storeDown := errors.New("i/o timeout")
	h.store.setOnCAS(func(fakeCASCall, func() (bool, []byte)) (bool, []byte, error) {
		return false, nil, storeDown
	})
	var runErr error
	for lost := false; !lost; {
		h.awaitWaiting(t, result, true)
		h.clock.AdvanceToNext(t)
		select {
		case runErr = <-result:
			lost = true
			continue
		case <-h.clock.timerReady(1):
		case <-time.After(testWaitLimit):
			t.Fatal("renewal neither rescheduled nor lost the lock")
		}
		now := h.clock.Now()
		if !now.Before(validUntil.Add(-testSingletonGuard)) {
			t.Fatalf("an unknown renewal at %v (window ends %v, guard %v) did not lose the lock", now, validUntil, testSingletonGuard)
		}
		if status := h.status(t); status.state != singletonUnknown {
			t.Fatalf("status inside the window = %+v, want unknown", status)
		}
		if got := h.healthOf(t); got.Status != health.StatusDegraded {
			t.Fatalf("unknown renewal health = %+v, want degraded", got)
		}
	}
	if lostAt.Load() == nil {
		t.Fatalf("run returned (%v) without the OnFail hook running", runErr)
	}
	at := *lostAt.Load()
	if at.Before(validUntil.Add(-testSingletonGuard)) || at.After(validUntil) {
		t.Fatalf("lost at %v, want within [validUntil-guard, validUntil] = [%v, %v]", at, validUntil.Add(-testSingletonGuard), validUntil)
	}
	if at.Sub(validUntil.Add(-testSingletonGuard)) >= testSingletonRenew {
		t.Fatalf("lost at %v, more than one beat after the window started closing", at)
	}
	if err := runErr; !errors.Is(err, ErrSingletonLost) || !errors.Is(err, storeDown) {
		t.Fatalf("run error = %v, want ErrSingletonLost wrapping the store error", err)
	}
}

// #6 / §3.3 关系 1：续期一直 Unknown 时，Lost 必须不晚于 validUntil 判定——键最早在 validUntil 过期，
// 之后新进程就可能拿到锁。前几拍快速报错（连接被拒）、结论落在窗口内，于是进入
// [validUntil−guard, validUntil) 的那一拍才发起；这一拍若是超时，它的单次超时不能越过 validUntil，
// 否则 Lost 判定晚于键过期：只有 renew_interval ≤ guard 不够，结论最晚会落在 validUntil−guard+2×renew_interval。
func TestSingletonUnknownRenewalTimeoutLosesNoLaterThanTheWindowEnd(t *testing.T) {
	const ttl, guard = 14 * time.Second, 3 * time.Second
	h := newSingletonHarness(t, nil, nil)
	// 满足 ValidateServiceConfig 的三条关系：renew 3s ≤ guard 3s；2×3s ≤ 14s−3s；startup_wait 30s ≥ 14s+2×3s。
	h.app.cfg.Set("singleton.ttl", ttl)
	h.app.cfg.Set("singleton.guard", guard)
	var lostAt atomic.Pointer[time.Time]
	h.svc.onInit = func(r *Registry) error {
		MustLookup[*RuntimeFailure](r, ModRuntimeFailure).OnFail(func(error) {
			now := h.clock.Now()
			lostAt.Store(&now)
		})
		return nil
	}
	result := h.start()
	h.awaitServed(t, result)
	validUntil := h.status(t).validUntil
	if want := h.store.calls()[0].at.Add(ttl); !validUntil.Equal(want) {
		t.Fatalf("test setup: validUntil = %v, want acquire asked + ttl (%v)", validUntil, want)
	}
	refused := errors.New("dial tcp: connection refused")
	h.store.setOnCAS(func(call fakeCASCall, _ func() (bool, []byte)) (bool, []byte, error) {
		if call.at.Before(validUntil.Add(-guard)) {
			return false, nil, refused // 立即失败，结论仍在窗口内 → Unknown
		}
		// 进入窗口末段的这一拍：请求发出后一直没有回复，直到调用方给的单次超时到期。
		h.clock.Advance(call.timeout)
		return false, nil, context.DeadlineExceeded
	})
	var runErr error
	for done := false; !done; {
		select {
		case runErr = <-result:
			done = true
		case <-h.clock.timerReady(1):
			h.clock.AdvanceToNext(t)
		case <-time.After(testWaitLimit):
			t.Fatal("renewal neither rescheduled nor lost the lock")
		}
	}
	if !errors.Is(runErr, ErrSingletonLost) || !errors.Is(runErr, context.DeadlineExceeded) {
		t.Fatalf("run error = %v, want ErrSingletonLost wrapping the renewal timeout", runErr)
	}
	at := lostAt.Load()
	if at == nil {
		t.Fatal("the lock was lost without the OnFail hook running")
	}
	if at.After(validUntil) {
		t.Fatalf("lost at validUntil+%v: the key may already belong to a new process while this one still serves", at.Sub(validUntil))
	}
}

// #6：进程卡住、窗口按时间已过，但恢复后（asked 在恢复之后）的那次 Renew Applied → 继续持有。
func TestSingletonAnAppliedRenewalAfterAStallKeepsTheLock(t *testing.T) {
	h := newSingletonHarness(t, nil, nil)
	result := h.start()
	h.awaitServed(t, result)
	validUntil := h.status(t).validUntil
	h.awaitWaiting(t, result, true)
	h.clock.Advance(testSingletonTTL - time.Second) // 节拍晚了 11s 才触发：窗口按时间已经过了
	h.awaitWaiting(t, result, true)
	now := h.clock.Now()
	if !now.After(validUntil.Add(-testSingletonGuard)) {
		t.Fatalf("test setup: %v is not past the window", now)
	}
	if status := h.status(t); status.state != singletonHeld || !status.validUntil.Equal(now.Add(testSingletonTTL)) {
		t.Fatalf("status after the post-stall renewal = %+v, want held until %v", status, now.Add(testSingletonTTL))
	}
	if err := h.runtimeFailure(t).Err(); err != nil {
		t.Fatalf("runtime failure after an applied renewal: %v", err)
	}
	if err := h.stop(t, result); err != nil {
		t.Fatalf("run: %v", err)
	}
}

// #7：窗口从 asked 起算：CAS 处理慢时，validUntil 不晚于 asked + ttl。
func TestSingletonWindowStartsWhenTheRenewalWasAsked(t *testing.T) {
	h := newSingletonHarness(t, nil, nil)
	result := h.start()
	h.awaitServed(t, result)
	h.awaitWaiting(t, result, true)
	h.store.setOnCAS(func(call fakeCASCall, apply func() (bool, []byte)) (bool, []byte, error) {
		h.clock.Advance(2 * time.Second) // Redis 2s 后才处理
		applied, current := apply()
		return applied, current, nil
	})
	h.clock.AdvanceToNext(t)
	h.awaitWaiting(t, result, true)
	calls := h.store.calls()
	asked := calls[len(calls)-1].at
	if status := h.status(t); status.state != singletonHeld || status.validUntil.After(asked.Add(testSingletonTTL)) {
		t.Fatalf("status = %+v, want validUntil <= asked(%v) + ttl", status, asked)
	}
	if err := h.stop(t, result); err != nil {
		t.Fatalf("run: %v", err)
	}
}

// #7b：迟到的 Applied 不作数：回复在 asked + ttl − guard 之后才到 → 不进入 Held，立即再续一次，以那次为准。
func TestSingletonALateAppliedRenewalDoesNotCount(t *testing.T) {
	h := newSingletonHarness(t, nil, nil)
	result := h.start()
	h.awaitServed(t, result)
	before := h.status(t)
	h.awaitWaiting(t, result, true)
	var calls atomic.Int32
	var statusAtRetry atomic.Pointer[singletonStatus]
	h.store.setOnCAS(func(call fakeCASCall, apply func() (bool, []byte)) (bool, []byte, error) {
		if calls.Add(1) == 1 { // SIGSTOP：Redis 处理了，回复在 SIGCONT 之后才读到
			applied, current := apply()
			h.clock.Advance(testSingletonTTL + time.Second)
			return applied, current, nil
		}
		status := h.app.singleton.snapshot()
		statusAtRetry.Store(&status)
		h.store.set(testSingletonKey, testOtherHolder, 0) // 停的期间键过期、被新进程拿走
		applied, current := apply()
		return applied, current, nil
	})
	h.clock.AdvanceToNext(t)
	err := awaitRunResult(t, result)
	if !errors.Is(err, ErrSingletonLost) || !errors.Is(err, ErrSingletonNotHeld) {
		t.Fatalf("run error = %v, want Lost by NotHeld on the immediate retry", err)
	}
	got := h.store.calls()
	if len(got) < 3 {
		t.Fatalf("CAS calls = %d, want the late renewal and an immediate retry", len(got))
	}
	late, retry := got[len(got)-2], got[len(got)-1]
	if !retry.at.Equal(late.at.Add(testSingletonTTL + time.Second)) {
		t.Fatalf("retry asked at %v, want immediately after the late reply (%v)", retry.at, late.at.Add(testSingletonTTL+time.Second))
	}
	status := statusAtRetry.Load()
	if status == nil || status.state == singletonHeld || !status.validUntil.Equal(before.validUntil) {
		t.Fatalf("status when retrying = %+v, want not held and the window unchanged (%v)", status, before.validUntil)
	}
}

// #8：正常停机：所有 Mod 停完之后才 Release，Release 只删自己的值。
func TestSingletonReleasesOnlyAfterEveryModStopped(t *testing.T) {
	shared := &singletonProbeMod{name: "probe_shared"}
	service := &singletonProbeMod{name: "probe_service"}
	h := newSingletonHarness(t, []Mod{shared}, []Mod{service})
	var heldAtSharedStop, heldAtServiceStop atomic.Bool
	shared.onStop = func() { heldAtSharedStop.Store(len(h.store.value(testSingletonKey)) > 0) }
	service.onStop = func() { heldAtServiceStop.Store(len(h.store.value(testSingletonKey)) > 0) }
	result := h.start()
	h.awaitServed(t, result)
	mine := h.store.value(testSingletonKey)
	mustBeMine(t, mine)
	if err := h.stop(t, result); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !heldAtSharedStop.Load() || !heldAtServiceStop.Load() {
		t.Fatal("lock was released before every mod stopped")
	}
	releases := h.store.releaseCalls()
	if len(releases) != 1 || !bytes.Equal(releases[0].expected, mine) {
		t.Fatalf("releases = %+v, want one CompareAndDelete of this process's value", releases)
	}
	if h.store.value(testSingletonKey) != nil {
		t.Fatal("key still present after a clean shutdown")
	}
	if !h.store.isClosed() || h.clock.pendingTimers() != 0 {
		t.Fatal("store not closed or renewal still scheduled")
	}
}

// C5（维护者决定 2026-10-06）：停机中的进程仍算活。Service.Shutdown 与各 Mod Stop 期间查 Live 都得到
// 本 sid（键在、值是自己的），直到全部 Mod 停完、Release 之后键才不在。没有“停机中”的中间值。
func TestSingletonLiveCountsAStoppingProcessUntilRelease(t *testing.T) {
	shared := &singletonProbeMod{name: "probe_shared"}
	service := &singletonProbeMod{name: "probe_service"}
	h := newSingletonHarness(t, []Mod{shared}, []Mod{service})
	type observation struct {
		sids  []int32
		err   error
		value []byte
	}
	var mu sync.Mutex
	seen := map[string]observation{}
	observe := func(phase string) {
		live := MustLookup[SingletonLiveness](h.svc.registry, ModSingleton)
		sids, err := live.Live(context.Background(), "game", []int32{1000, 1001})
		mu.Lock()
		defer mu.Unlock()
		seen[phase] = observation{sids: sids, err: err, value: h.store.value(testSingletonKey)}
	}
	h.svc.onShutdown = func(context.Context) error { observe("service shutdown"); return nil }
	service.onStop = func() { observe("service mod stop") }
	shared.onStop = func() { observe("shared mod stop") }
	result := h.start()
	h.awaitServed(t, result)
	mine := h.store.value(testSingletonKey)
	mustBeMine(t, mine)
	if err := h.stop(t, result); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, phase := range []string{"service shutdown", "service mod stop", "shared mod stop"} {
		got, ok := seen[phase]
		if !ok {
			t.Fatalf("Live was not observed during %s", phase)
		}
		if got.err != nil || len(got.sids) != 1 || got.sids[0] != 1000 {
			t.Fatalf("Live during %s = %v, %v; a stopping process holds the lock and counts as live", phase, got.sids, got.err)
		}
		if !bytes.Equal(got.value, mine) {
			t.Fatalf("during %s the key held %q, want this process's unchanged value %q", phase, got.value, mine)
		}
	}
	if h.store.value(testSingletonKey) != nil {
		t.Fatal("key still present after a clean shutdown: Live would keep counting a stopped process")
	}
}

// hangingSingletonMod 的 StopWithContext 一直挂到用例结束。
type hangingSingletonMod struct {
	name    ModName
	release chan struct{}
}

func (m *hangingSingletonMod) Name() ModName           { return m.name }
func (m *hangingSingletonMod) Init(*viper.Viper) error { return nil }
func (m *hangingSingletonMod) Provide(*Registry) error { return nil }
func (m *hangingSingletonMod) Start() error            { return nil }
func (m *hangingSingletonMod) Stop()                   {}
func (m *hangingSingletonMod) StopWithContext(context.Context) error {
	<-m.release
	return nil
}

// #9：Service.Shutdown 超时、服务专属 Mod 停机不完整、共享 Mod 停机不完整：都不 Release，只停续期。
func TestSingletonIsNotReleasedWhenShutdownIsIncomplete(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(release chan struct{}) (shared, service []Mod, svcShutdown func(context.Context) error)
	}{
		{
			name: "service_shutdown_timeout",
			setup: func(release chan struct{}) ([]Mod, []Mod, func(context.Context) error) {
				return []Mod{&singletonProbeMod{name: "probe_shared"}}, nil, func(context.Context) error {
					<-release
					return nil
				}
			},
		},
		{
			name: "service_mod_stop_incomplete",
			setup: func(release chan struct{}) ([]Mod, []Mod, func(context.Context) error) {
				return []Mod{&singletonProbeMod{name: "probe_shared"}}, []Mod{&hangingSingletonMod{name: "hanging_service", release: release}}, nil
			},
		},
		{
			name: "shared_mod_stop_incomplete",
			setup: func(release chan struct{}) ([]Mod, []Mod, func(context.Context) error) {
				return []Mod{&hangingSingletonMod{name: "hanging_shared", release: release}}, nil, nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			defer close(release)
			shared, service, shutdown := tc.setup(release)
			h := newSingletonHarness(t, shared, service)
			h.app.cfg.Set("shutdown.total_timeout", 400*time.Millisecond)
			h.svc.onShutdown = shutdown
			result := h.start()
			h.awaitServed(t, result)
			mine := h.store.value(testSingletonKey)
			mustBeMine(t, mine)
			err := h.stop(t, result)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("run error = %v, want an incomplete shutdown", err)
			}
			if len(h.store.releaseCalls()) != 0 || !bytes.Equal(h.store.value(testSingletonKey), mine) {
				t.Fatal("lock released while a component may still use its dependencies")
			}
			if h.clock.pendingTimers() != 0 {
				t.Fatal("renewal still scheduled after run returned")
			}
			// 还在跑的组件可能继续查 Live：store 留到进程退出，不在 run 返回时关闭（收尾审查 6）。
			if h.store.isClosed() {
				t.Fatal("store closed while a component that may still call Live is running")
			}
			live := MustLookup[SingletonLiveness](h.svc.registry, ModSingleton)
			if sids, err := live.Live(context.Background(), "game", []int32{1000}); err != nil || len(sids) != 1 {
				t.Fatalf("Live after an incomplete shutdown = %v, %v; want [1000]", sids, err)
			}
			before := len(h.store.calls())
			h.clock.Advance(10 * testSingletonRenew)
			if len(h.store.calls()) != before {
				t.Fatal("renewal continued after run returned")
			}
		})
	}
}

// erroringStopSingletonMod 的 StopWithContext 立即返回一个普通错误（不是 ctx 取消 / 超时）。
type erroringStopSingletonMod struct {
	name ModName
	err  error
}

func (m *erroringStopSingletonMod) Name() ModName                         { return m.name }
func (m *erroringStopSingletonMod) Init(*viper.Viper) error               { return nil }
func (m *erroringStopSingletonMod) Provide(*Registry) error               { return nil }
func (m *erroringStopSingletonMod) Start() error                          { return nil }
func (m *erroringStopSingletonMod) Stop()                                 {}
func (m *erroringStopSingletonMod) StopWithContext(context.Context) error { return m.err }

// 第 5 笔演练偏差 3 的影响核实：Mod 停机返回普通错误（如 etcd 注销时的 “requested lease not found”）
// 算已停完——错误并入 run 的返回值（退出码非零），但单实例锁照常 Release，下一个进程不用等 TTL。
// 只有 ctx 取消 / 超时（stopIncomplete）才不 Release，见上一个用例。
func TestSingletonIsReleasedWhenAModStopReturnsAnOrdinaryError(t *testing.T) {
	stopErr := errors.New("etcd Discovery: revoke: etcdserver: requested lease not found")
	h := newSingletonHarness(t, []Mod{&erroringStopSingletonMod{name: "erroring_shared", err: stopErr}}, nil)
	result := h.start()
	h.awaitServed(t, result)
	mine := h.store.value(testSingletonKey)
	mustBeMine(t, mine)
	if err := h.stop(t, result); !errors.Is(err, stopErr) {
		t.Fatalf("run error = %v, want the mod stop error", err)
	}
	releases := h.store.releaseCalls()
	if len(releases) != 1 || !bytes.Equal(releases[0].expected, mine) || h.store.value(testSingletonKey) != nil {
		t.Fatalf("releases = %+v, key = %q; want this process's value released", releases, h.store.value(testSingletonKey))
	}
}

// #10：Mod Start 失败、Service.Init 失败：已启动的 Mod 停完后 Release。
func TestSingletonIsReleasedAfterAStartupFailureStopsTheMods(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *singletonHarness, shared *singletonProbeMod)
		want  string
	}{
		{
			name: "mod_start_fails",
			setup: func(_ *singletonHarness, shared *singletonProbeMod) {
				shared.startErr = errors.New("mod start failed")
			},
			want: "mod start failed",
		},
		{
			name: "service_init_fails",
			setup: func(h *singletonHarness, _ *singletonProbeMod) {
				h.svc.initErr = errors.New("service init failed")
			},
			want: "service init failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shared := &singletonProbeMod{name: "probe_shared"}
			h := newSingletonHarness(t, []Mod{shared}, nil)
			var heldAtStop atomic.Bool
			shared.onStop = func() { heldAtStop.Store(len(h.store.value(testSingletonKey)) > 0) }
			tc.setup(h, shared)
			err := awaitRunResult(t, h.start())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("run error = %v, want %q", err, tc.want)
			}
			if shared.stops.Load() != 1 || !heldAtStop.Load() {
				t.Fatalf("mod stops=%d heldAtStop=%v, want stopped while still holding the lock", shared.stops.Load(), heldAtStop.Load())
			}
			if len(h.store.releaseCalls()) != 1 || h.store.value(testSingletonKey) != nil {
				t.Fatalf("releases=%+v key=%q, want the lock released after the mods stopped", h.store.releaseCalls(), h.store.value(testSingletonKey))
			}
			if !h.store.isClosed() {
				t.Fatal("store not closed")
			}
		})
	}
}

// §5：启动期间发生的 RuntimeFailure 让 run 停在下一个阶段边界，不再启动后面的 Mod。单实例锁开启时
// 同样如此，而且这种失败不是失锁：已启动的 Mod 停完后照常 Release。
func TestRunStopsStartingModsAfterARuntimeFailure(t *testing.T) {
	for _, singleton := range []bool{false, true} {
		t.Run(fmt.Sprintf("singleton_%v", singleton), func(t *testing.T) {
			first := &singletonProbeMod{name: "probe_first"}
			second := &singletonProbeMod{name: "probe_second"}
			h := newSingletonHarness(t, []Mod{first, second}, nil)
			h.app.cfg.Set("singleton.enabled", singleton)
			cause := errors.New("dataengine fatal during replay")
			first.onStart = func() {
				MustLookup[*RuntimeFailure](first.registry, ModRuntimeFailure).Fail(cause)
			}
			err := awaitRunResult(t, h.start())
			if !errors.Is(err, cause) {
				t.Fatalf("run error = %v, want the runtime failure", err)
			}
			if n := second.starts.Load(); n != 0 {
				t.Fatalf("a later mod started %d times after the runtime failure", n)
			}
			if first.stops.Load() != 1 {
				t.Fatal("started mod was not stopped")
			}
			if !singleton {
				return
			}
			if errors.Is(err, ErrSingletonLost) {
				t.Fatalf("run error = %v, want a non-singleton failure", err)
			}
			if len(h.store.releaseCalls()) != 1 || h.store.value(testSingletonKey) != nil || !h.store.isClosed() {
				t.Fatalf("releases=%+v key=%q closed=%v, want released and closed after the mods stopped",
					h.store.releaseCalls(), h.store.value(testSingletonKey), h.store.isClosed())
			}
		})
	}
}

// 启动期间失锁（例如 DataEngine 重放很长、卡住期间键被新进程拿走）：run 停在下一个阶段边界，
// 停掉已启动的 Mod，但不 Release——键已是别人的。
func TestSingletonLostDuringStartupStopsTheModsWithoutReleasing(t *testing.T) {
	first := &singletonProbeMod{name: "probe_first"}
	second := &singletonProbeMod{name: "probe_second"}
	h := newSingletonHarness(t, []Mod{first, second}, nil)
	first.onStart = func() {
		lost := make(chan struct{})
		MustLookup[*RuntimeFailure](first.registry, ModRuntimeFailure).OnFail(func(error) { close(lost) })
		<-h.clock.timerReady(1) // 续期 goroutine 停在下一拍
		h.store.set(testSingletonKey, testOtherHolder, 0)
		h.clock.AdvanceToNext(t)
		select {
		case <-lost:
		case <-time.After(testWaitLimit):
			t.Error("renewal did not lose the lock during startup")
		}
	}
	err := awaitRunResult(t, h.start())
	if !errors.Is(err, ErrSingletonLost) || !errors.Is(err, ErrSingletonNotHeld) {
		t.Fatalf("run error = %v, want ErrSingletonLost wrapping ErrSingletonNotHeld", err)
	}
	if n := second.starts.Load(); n != 0 {
		t.Fatalf("a later mod started %d times after the lock was lost", n)
	}
	if first.stops.Load() != 1 {
		t.Fatal("started mod was not stopped")
	}
	if len(h.store.releaseCalls()) != 0 || string(h.store.value(testSingletonKey)) != testOtherHolder {
		t.Fatalf("releases=%+v key=%q, want the new holder's key untouched", h.store.releaseCalls(), h.store.value(testSingletonKey))
	}
	if !h.store.isClosed() {
		t.Fatal("store not closed after every mod stopped")
	}
}

// deadlineSingletonMod 记下 StopWithContext 收到的截止时间。
type deadlineSingletonMod struct {
	name     ModName
	deadline atomic.Pointer[time.Time]
}

func (m *deadlineSingletonMod) Name() ModName           { return m.name }
func (m *deadlineSingletonMod) Init(*viper.Viper) error { return nil }
func (m *deadlineSingletonMod) Provide(*Registry) error { return nil }
func (m *deadlineSingletonMod) Start() error            { return nil }
func (m *deadlineSingletonMod) Stop()                   {}
func (m *deadlineSingletonMod) StopWithContext(ctx context.Context) error {
	if deadline, ok := ctx.Deadline(); ok {
		m.deadline.Store(&deadline)
	}
	return nil
}

// 停机时 Mod 的截止时间只为 Release 提前 singletonReleaseBudget；已 Lost 不会 Release，
// 这段预算还给 Mod 停机（收尾审查 3）。
func TestSingletonLostLockLeavesTheReleaseBudgetToModStop(t *testing.T) {
	for _, tc := range []struct {
		name    string
		lose    bool
		reserve time.Duration
	}{
		{name: "held", reserve: singletonReleaseBudget},
		{name: "lost", lose: true, reserve: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mod := &deadlineSingletonMod{name: "probe_deadline"}
			h := newSingletonHarness(t, []Mod{mod}, nil)
			h.app.cfg.Set("shutdown.total_timeout", 20*time.Second)
			var shutdownDeadline atomic.Pointer[time.Time]
			h.svc.onShutdown = func(ctx context.Context) error {
				if deadline, ok := ctx.Deadline(); ok {
					shutdownDeadline.Store(&deadline)
				}
				return nil
			}
			result := h.start()
			h.awaitServed(t, result)
			var err error
			if tc.lose {
				h.awaitWaiting(t, result, true)
				h.store.set(testSingletonKey, testOtherHolder, 0)
				h.clock.AdvanceToNext(t)
				err = awaitRunResult(t, result)
				if !errors.Is(err, ErrSingletonLost) {
					t.Fatalf("run error = %v, want ErrSingletonLost", err)
				}
			} else if err = h.stop(t, result); err != nil {
				t.Fatalf("run: %v", err)
			}
			shutdownAt, modAt := shutdownDeadline.Load(), mod.deadline.Load()
			if shutdownAt == nil || modAt == nil {
				t.Fatalf("deadlines not recorded: shutdown=%v mod=%v", shutdownAt, modAt)
			}
			if got := shutdownAt.Sub(*modAt); got != tc.reserve {
				t.Fatalf("mod stop deadline is %v before the shutdown deadline, want %v", got, tc.reserve)
			}
		})
	}
}

// #11：enabled=true 无 opener → 启动失败（fail-closed）；enabled=false → 不调用 opener。
func TestSingletonOpenerIsRequiredOnlyWhenEnabled(t *testing.T) {
	t.Run("enabled_without_opener", func(t *testing.T) {
		shared := &singletonProbeMod{name: "probe_shared"}
		h := newSingletonHarness(t, []Mod{shared}, nil)
		h.app.singletonOpener = nil
		err := awaitRunResult(t, h.start())
		if !errors.Is(err, ErrSingletonOpenerMissing) {
			t.Fatalf("run error = %v, want ErrSingletonOpenerMissing", err)
		}
		if shared.inits.Load() != 0 {
			t.Fatal("mods initialized without the singleton lock")
		}
	})
	t.Run("disabled", func(t *testing.T) {
		h := newSingletonHarness(t, nil, nil)
		h.app.cfg.Set("singleton.enabled", false)
		result := h.start()
		h.awaitServed(t, result)
		if _, ok := h.svc.registry.Get(ModSingleton); ok {
			t.Fatal("Live registered while singleton is disabled")
		}
		if err := h.stop(t, result); err != nil {
			t.Fatalf("run: %v", err)
		}
		if h.opens.Load() != 0 {
			t.Fatal("opener called while singleton is disabled")
		}
	})
}

// #12：配置关系违反 → ValidateServiceConfig 报错；默认值满足全部关系。
func TestValidateServiceConfigPinsSingletonTimeRelations(t *testing.T) {
	base := func() *viper.Viper {
		cfg := viper.New()
		cfg.Set("server_type", "game")
		cfg.Set("sid", 1000)
		cfg.Set("singleton.enabled", true)
		cfg.Set("singleton.key_prefix", "roost:demo:singleton")
		return cfg
	}
	if err := ValidateServiceConfig(base()); err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}
	disabled := base()
	disabled.Set("singleton.enabled", false)
	disabled.Set("singleton.key_prefix", "")
	disabled.Set("singleton.guard", time.Second)
	if err := ValidateServiceConfig(disabled); err != nil {
		t.Fatalf("disabled singleton validated: %v", err)
	}
	for _, tc := range []struct {
		name string
		set  map[string]any
		want string
	}{
		{"missing_key_prefix", map[string]any{"singleton.key_prefix": ""}, "singleton.key_prefix"},
		{"key_prefix_whitespace", map[string]any{"singleton.key_prefix": "roost demo"}, "singleton.key_prefix"},
		{"renew_over_guard", map[string]any{"singleton.renew_interval": 6 * time.Second, "singleton.ttl": 30 * time.Second, "singleton.startup_wait": time.Minute}, "renew_interval"},
		{"two_renews_over_window", map[string]any{"singleton.ttl": 10 * time.Second, "singleton.startup_wait": time.Minute}, "ttl"},
		{"startup_wait_too_short", map[string]any{"singleton.startup_wait": 20 * time.Second}, "startup_wait"},
		{"non_positive_ttl", map[string]any{"singleton.ttl": 0}, "singleton.ttl"},
		{"non_positive_guard", map[string]any{"singleton.guard": -time.Second}, "singleton.guard"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			for key, value := range tc.set {
				cfg.Set(key, value)
			}
			err := ValidateServiceConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateServiceConfig = %v, want an error naming %q", err, tc.want)
			}
		})
	}
}

// #13：OnFail 首次失败调用一次、按登记顺序；失败后登记立即调用。
func TestRuntimeFailureOnFailRunsHooksOnceInOrderBeforeDone(t *testing.T) {
	failure := NewRuntimeFailure()
	var order []string
	failure.OnFail(func(error) {
		order = append(order, "first")
		if len(failure.Done()) != 0 {
			t.Error("Done delivered before the hooks ran")
		}
	})
	failure.OnFail(func(error) { order = append(order, "second") })
	cause := errors.New("lock lost")
	failure.Fail(cause)
	failure.Fail(errors.New("later"))
	if fmt.Sprint(order) != "[first second]" {
		t.Fatalf("hooks ran %v, want [first second] once", order)
	}
	select {
	case got := <-failure.Done():
		if !errors.Is(got, cause) {
			t.Fatalf("done = %v", got)
		}
	default:
		t.Fatal("Done not delivered")
	}
	var late error
	failure.OnFail(func(err error) { late = err })
	if !errors.Is(late, cause) {
		t.Fatalf("hook registered after the failure got %v, want it called immediately with the first failure", late)
	}
}

// #13：回调 panic 时 Done 仍投递、后面的回调仍执行；回调里再调 Fail 不死锁。
func TestRuntimeFailureOnFailSurvivesPanicsAndReentry(t *testing.T) {
	failure := NewRuntimeFailure()
	var after atomic.Bool
	failure.OnFail(func(error) { panic("hook exploded") })
	failure.OnFail(func(error) { failure.Fail(errors.New("reentrant")) })
	failure.OnFail(func(error) { after.Store(true) })
	finished := make(chan struct{})
	go func() {
		failure.Fail(errors.New("first"))
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(testWaitLimit):
		t.Fatal("Fail deadlocked on a reentrant hook")
	}
	select {
	case <-failure.Done():
	default:
		t.Fatal("Done not delivered after a hook panicked")
	}
	if !after.Load() {
		t.Fatal("a panic in one hook skipped the next hook")
	}
	if err := failure.Err(); err == nil || !strings.Contains(err.Error(), "hook exploded") || !strings.Contains(err.Error(), "reentrant") {
		t.Fatalf("Err() = %v, want the panic and the reentrant failure joined", err)
	}
}

// #13：并发 Fail 与并发 OnFail 下每个回调恰好调用一次（-race）。
func TestRuntimeFailureOnFailConcurrentRegistrationRunsEachHookOnce(t *testing.T) {
	for range 50 {
		failure := NewRuntimeFailure()
		const hooks = 16
		var counts [hooks]atomic.Int32
		var wg sync.WaitGroup
		for i := range hooks {
			wg.Go(func() { failure.OnFail(func(error) { counts[i].Add(1) }) })
		}
		for range 4 {
			wg.Go(func() { failure.Fail(errors.New("boom")) })
		}
		wg.Wait()
		for i := range hooks {
			if n := counts[i].Load(); n != 1 {
				t.Fatalf("hook %d ran %d times", i, n)
			}
		}
		if len(failure.Done()) != 1 {
			t.Fatal("Done delivered other than once")
		}
	}
}

// #14：Live 登记在 ModSingleton；按 <key_prefix>:<serverType>:<sid> 查询，值非空的 sid 按入参顺序返回。
func TestSingletonLiveReportsSidsHoldingTheLock(t *testing.T) {
	h := newSingletonHarness(t, nil, nil)
	h.store.set(testSingletonPrefix+":game:1002", "a|h|1|1", 0)
	h.store.set(testSingletonPrefix+":game:1001", "b|h|2|1", 0)
	h.store.set(testSingletonPrefix+":match:1003", "c|h|3|1", 0) // 别的服务类型、同号不算
	var live SingletonLiveness
	h.svc.onInit = func(r *Registry) error {
		got, ok := Lookup[SingletonLiveness](r, ModSingleton)
		if !ok {
			return errors.New("ModSingleton not registered")
		}
		live = got
		return nil
	}
	result := h.start()
	h.awaitServed(t, result)
	ctx := context.Background()
	got, err := live.Live(ctx, "game", []int32{1003, 1002, 1000, 1001, 1004})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[1002 1000 1001]" {
		t.Fatalf("Live = %v, want [1002 1000 1001] in argument order", got)
	}
	h.store.mu.Lock()
	lastGet := h.store.gets[len(h.store.gets)-1]
	h.store.mu.Unlock()
	if lastGet[0] != testSingletonPrefix+":game:1003" {
		t.Fatalf("Live queried %v, want <key_prefix>:<serverType>:<sid>", lastGet)
	}
	if _, err := live.Live(ctx, "game", make([]int32, SingletonLiveMaxSIDs+1)); err == nil {
		t.Fatal("Live accepted more sids than the limit")
	}
	h.store.mu.Lock()
	h.store.getErr = errors.New("redis down")
	h.store.mu.Unlock()
	if _, err := live.Live(ctx, "game", []int32{1000}); err == nil || !strings.Contains(err.Error(), "redis down") {
		t.Fatalf("Live with a failing store = %v, want the store error", err)
	}
	if err := h.stop(t, result); err != nil {
		t.Fatalf("run: %v", err)
	}
}
