// Manager 生命周期：模式配置、冻结内容与预算、Drain、在途交付与 Stop / Close、会话重开与传输生命期、异步加载。

package entitysync

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/nettransport"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// Push 中同步改变控制面，固定“已捕获、尚未采纳”的交错，无需靠 sleep 碰运气。
func TestInFlightDeliveryCannotOverwriteNewIntent(t *testing.T) {
	for _, change := range []string{"hold", "reopen", "reopen_failure", "profile", "unsubscribe", "unregister"} {
		t.Run(change, func(t *testing.T) {
			records := newRecordingTransport()
			var manager *Manager
			var hook func()
			manager = newTestManager(t, TransportFunc(func(ctx context.Context, sid SessionID, data []byte) error {
				if hook != nil {
					f := hook
					hook = nil
					f()
					if change == "reopen_failure" {
						return errors.New("old connection failed")
					}
				}
				return records.Push(ctx, sid, data)
			}), ManagerConfig{})
			packs := 0
			state := testSubject(t, 4101, &packs)
			if err := manager.Register(state); err != nil {
				t.Fatal(err)
			}
			open(t, manager, 1)
			mustSubscribe(t, manager, 1, 4101, entity.SyncProfile{})
			hook = func() {
				switch change {
				case "hold":
					if err := manager.HoldSession(1); err != nil {
						t.Fatal(err)
					}
				case "reopen", "reopen_failure":
					manager.CloseSession(1)
					open(t, manager, 1)
					mustSubscribe(t, manager, 1, 4101, entity.SyncProfile{})
				case "profile":
					mustSubscribe(t, manager, 1, 4101, entity.SyncProfile{Key: "far", LOD: 2})
				case "unsubscribe":
					if err := manager.Unsubscribe(1, 4101); err != nil {
						t.Fatal(err)
					}
				case "unregister":
					if err := manager.Unregister(4101); err != nil {
						t.Fatal(err)
					}
				}
			}
			mustFlush(t, manager)
			records.take(1)
			if change == "hold" {
				if manager.Stats().HeldSessions != 1 {
					t.Fatal("old delivery overwrote the held session")
				}
				if err := manager.ReadySession(1); err != nil {
					t.Fatal(err)
				}
			}
			mustFlush(t, manager)
			got := oneFrame(t, records, 1)
			switch change {
			case "hold", "reopen", "reopen_failure":
				if got.wire.Kind != frame.Full || got.objects[4101] != frame.ObjectCreate {
					t.Fatalf("new lifetime lost snapshot: %+v", got)
				}
				if change == "hold" && got.wire.Epoch != 2 {
					t.Fatalf("epoch = %d", got.wire.Epoch)
				}
			case "profile":
				if !got.updates[4101].Full || got.updates[4101].Profile.Key != "far" {
					t.Fatalf("profile change lost: %+v", got.updates)
				}
			default:
				if len(got.wire.Objects) != 1 || got.wire.Objects[0].Operation != frame.ObjectRemove {
					t.Fatal("accepted create was not removed")
				}
			}
		})
	}
}

func TestStopAndCloseRespectDeadlineWhileAnotherFlushIsInFlight(t *testing.T) {
	for _, operation := range []string{"stop", "close"} {
		t.Run(operation, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			manager := newTestManager(t, TransportFunc(func(context.Context, SessionID, []byte) error { close(started); <-release; return nil }), ManagerConfig{})
			packs := 0
			state := testSubject(t, 4201, &packs)
			if err := manager.Register(state); err != nil {
				t.Fatal(err)
			}
			open(t, manager, 1)
			mustSubscribe(t, manager, 1, 4201, entity.SyncProfile{})
			flushed := make(chan error, 1)
			go func() { flushed <- manager.Flush(context.Background()) }()
			<-started
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			stopped := make(chan error, 1)
			go func() {
				if operation == "stop" {
					stopped <- manager.Stop(ctx)
				} else {
					stopped <- manager.Close(ctx)
				}
			}()
			returned := false
			select {
			case err := <-stopped:
				returned = true
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("got %v, want deadline exceeded", err)
				}
			case <-time.After(time.Second):
			}
			close(release)
			if err := <-flushed; err != nil {
				t.Fatal(err)
			}
			if !returned {
				<-stopped
				t.Fatal("deadline was ignored while waiting for Flush")
			}
		})
	}
}

func TestStopCancelsPushAndPreventsRestartUntilItReturns(t *testing.T) {
	started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	manager := newTestManager(t, TransportFunc(func(ctx context.Context, _ SessionID, _ []byte) error {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release
		return ctx.Err()
	}), ManagerConfig{Interval: time.Millisecond})
	packs := 0
	state := testSubject(t, 4202, &packs)
	if err := manager.Register(state); err != nil {
		t.Fatal(err)
	}
	state.MarkDirty(1)
	open(t, manager, 1)
	mustSubscribe(t, manager, 1, 4202, entity.SyncProfile{})
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("tick did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- manager.Stop(ctx) }()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		cancel()
		close(release)
		t.Fatal("Stop did not cancel in-flight Push")
	}
	cancel()
	if err := <-stopped; !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop: %v", err)
	}
	if err := manager.Start(context.Background()); !errors.Is(err, ErrManagerStopping) {
		t.Fatalf("Start during stop: %v", err)
	}
	manager.runMu.Lock()
	done := manager.stopState.done
	manager.runMu.Unlock()
	close(release)
	<-done
	if manager.Stats().Sessions != 1 || !state.PendingDirty() {
		t.Fatal("cancelling the tick blamed the session or lost dirty content")
	}
	// 清理不再次调用上面的单次屏障传输；下一次 Flush 用已取消 context 不会进入 Push。
	manager.CloseSession(1)
	if err := manager.Stop(nil); err != nil {
		t.Fatal(err)
	}
}

func TestCloseReleasesStateAfterFinalFlushError(t *testing.T) {
	manager := newTestManager(t, TransportFunc(func(context.Context, SessionID, []byte) error { return ErrRetryLater }), ManagerConfig{})
	packs := 0
	if err := manager.Register(testSubject(t, 4203, &packs)); err != nil {
		t.Fatal(err)
	}
	open(t, manager, 1)
	mustSubscribe(t, manager, 1, 4203, entity.SyncProfile{})
	if err := manager.Close(context.Background()); !errors.Is(err, ErrRetryLater) {
		t.Fatalf("Close: %v", err)
	}
	if !manager.isClosed() || manager.Stats().Sessions != 0 || manager.Stats().Subjects != 0 {
		t.Fatal("final flush failure prevented cleanup")
	}
}

func TestModeConfigurationRequiresProducerAndKeepsDefault(t *testing.T) {
	transport := newRecordingTransport()
	for _, config := range []ManagerConfig{{Mode: SyncMode(9)}, {Interval: -1}, {MaxFrozenBytes: -1}} {
		config.Transport = transport
		if _, err := NewManager(config); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
	m := newTestManager(t, transport, ManagerConfig{Mode: ModeOnChange})
	if err := m.Start(context.Background()); err == nil {
		t.Fatal("unbound on_change started")
	}
	m.BindSyncProducer()
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mode, err := ParseSyncMode(""); err != nil || mode != ModePeriodic {
		t.Fatalf("default %v %v", mode, err)
	}
}

func TestOnChangeSnapshotBudgetIsSharedAcrossFlushes(t *testing.T) {
	transport := newRecordingTransport()
	m := newTestManager(t, transport, ManagerConfig{Mode: ModeOnChange, Interval: time.Hour, SnapshotBudget: SnapshotBudget{MaxObjects: 1, PerSessionObjects: 1}})
	open(t, m, 1)
	for id := int64(1); id <= 3; id++ {
		packs := 0
		if err := m.Register(testSubject(t, id, &packs)); err != nil {
			t.Fatal(err)
		}
		mustSubscribe(t, m, 1, id, entity.SyncProfile{})
	}
	mustFlush(t, m)
	mustFlush(t, m)
	mustFlush(t, m)
	if got := m.Counters().CreatesAdmitted; got != 1 {
		t.Fatalf("immediate flush reset budget: %d", got)
	}
	m.budgetWindow = time.Now().Add(-2 * time.Hour)
	mustFlush(t, m)
	if got := m.Counters().CreatesAdmitted; got != 2 {
		t.Fatalf("new window did not refill: %d", got)
	}
}

func TestFrozenContentCoalescesAndRetainsBudgetUntilSettlement(t *testing.T) {
	m := newTestManager(t, newRecordingTransport(), ManagerConfig{Mode: ModeOnChange, MaxFrozenBytes: 2})
	value := byte(1)
	packs := 0
	pack := func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
		packs++
		return entity.CopyFrozenSyncPayload(1, []byte{value}), nil
	}
	s := entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{Enabled: true, SubjectID: 1, Packer: entity.SubjectSyncPackFunc{Snapshot: pack, Delta: func(p entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) { return pack(p) }}})
	reserve := m.reserveFrozen
	s.MarkDirty(1)
	if err := s.FreezeSyncViews([]entity.SyncProfile{{}}, nil, reserve); err != nil {
		t.Fatal(err)
	}
	first, err := s.PrepareViews([]entity.SyncProfile{{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Counters().FrozenBytes != 1 {
		t.Fatal("inflight bytes were released before settlement")
	}
	value = 2
	s.MarkDirty(1)
	if err = s.FreezeSyncViews([]entity.SyncProfile{{}}, nil, reserve); err != nil {
		t.Fatal(err)
	}
	value = 3
	s.MarkDirty(1)
	if err = s.FreezeSyncViews([]entity.SyncProfile{{}}, nil, reserve); err != nil {
		t.Fatal(err)
	}
	if m.Counters().FrozenBytes != 2 {
		t.Fatal("pending slot not bounded")
	}
	if err = first.Commit(); err != nil {
		t.Fatal(err)
	}
	next, err := s.PrepareViews([]entity.SyncProfile{{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := next.Updates()[0]; !got.Full || got.Payload.BytesCopy()[0] != 3 || got.BaseVersion != 1 {
		t.Fatalf("bad coalesced content %+v", got)
	}
	if packs != 3 {
		t.Fatalf("frozen content repacked %d", packs)
	}
	if err = next.Commit(); err != nil {
		t.Fatal(err)
	}
	if m.Counters().FrozenBytes != 0 {
		t.Fatal("retained byte leak")
	}
}

func TestFrozenCapacityFailureLeavesDirtyForRecovery(t *testing.T) {
	m := newTestManager(t, newRecordingTransport(), ManagerConfig{Mode: ModeOnChange, MaxFrozenBytes: 1})
	packs := 0
	s := testSubject(t, 1, &packs)
	s.MarkDirty(1)
	err := s.FreezeSyncViews([]entity.SyncProfile{{}}, nil, m.reserveFrozen)
	if !errors.Is(err, entity.ErrSyncFrozenCapacity) || !s.PendingDirty() {
		t.Fatalf("capacity=%v dirty=%v", err, s.PendingDirty())
	}
	p, err := s.PrepareViews([]entity.SyncProfile{{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestFrozenNewProfileRecapturesOneConsistentVersion(t *testing.T) {
	m := newTestManager(t, newRecordingTransport(), ManagerConfig{Mode: ModeOnChange})
	packs := 0
	s := testSubject(t, 17, &packs)
	s.MarkDirty(1)
	if err := s.FreezeSyncViews([]entity.SyncProfile{{Key: "old"}}, nil, m.reserveFrozen); err != nil {
		t.Fatal(err)
	}
	p, err := s.PrepareViews([]entity.SyncProfile{{Key: "new"}}, []entity.SyncProfile{{Key: "new"}})
	if err != nil {
		t.Fatal(err)
	}
	if packs != 3 || p.Updates()[0].Version != p.Snapshots()[0].Version {
		t.Fatal("mixed profile generation")
	}
	if err = p.Commit(); err != nil {
		t.Fatal(err)
	}
	if m.Counters().FrozenBytes != 0 {
		t.Fatal("discarded profile cache retained bytes")
	}
}

func TestDrainWaitsForBudgetAndHonorsCancellation(t *testing.T) {
	m := newTestManager(t, newRecordingTransport(), ManagerConfig{Mode: ModeOnChange, Interval: time.Millisecond, SnapshotBudget: SnapshotBudget{MaxObjects: 1}})
	open(t, m, 1)
	for id := int64(1); id <= 3; id++ {
		packs := 0
		if err := m.Register(testSubject(t, id, &packs)); err != nil {
			t.Fatal(err)
		}
		mustSubscribe(t, m, 1, id, entity.SyncProfile{})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if got := m.Counters().CreatesAdmitted; got != 3 {
		t.Fatalf("drain admitted %d", got)
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if err := m.Drain(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestDrainIncludesPolicyFactsWithoutDirtySubjects(t *testing.T) {
	m := newTestManager(t, newRecordingTransport(), ManagerConfig{Interval: time.Millisecond})
	cancelPolicy := m.RegisterPolicy(PolicyHooks{Apply: func() error { return nil }, Pending: func() bool { return true }})
	defer cancelPolicy()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	// 政策没有 Stalls 回调时也要在超时错误里报出“该政策仍有待处理事实”。
	if err := m.Drain(ctx); !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "with 1 entry: policy: policy_pending") {
		t.Fatalf("unconfirmed policy drained: %v", err)
	}
}

// D3：Drain 在 ctx 超时时仍保留 errors.Is 语义，同时列出卡住的条目与原因。持久水位永远不到的 subject
// 就是“永久暂缓”：Drain 不放弃它，只把它和等待时长报出来，并计数。
func TestDrainTimeoutNamesStuckEntries(t *testing.T) {
	previous := metrics.DefaultRegistry()
	metrics.SetDefaultRegistry(metrics.NewRegistry())
	t.Cleanup(func() { metrics.SetDefaultRegistry(previous) })
	m := newTestManager(t, newRecordingTransport(), ManagerConfig{Interval: time.Millisecond, DurableWatermark: func() uint64 { return 9 }})
	open(t, m, 1)
	const stuck = DrainStallLimit + 3
	for id := int64(1001); id < 1001+stuck; id++ {
		packs := 0
		state := testSubject(t, id, &packs)
		state.SetLastCommitLSN(10)
		if err := m.Register(state); err != nil {
			t.Fatal(err)
		}
		mustSubscribe(t, m, 1, id, entity.SyncProfile{})
		state.MarkDirty(1)
	}
	cancelPolicy := m.RegisterPolicy(PolicyHooks{Apply: func() error { return nil }, Pending: func() bool { return true }})
	defer cancelPolicy()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := m.Drain(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain: %v", err)
	}
	var stalled *DrainStalledError
	if !errors.As(err, &stalled) {
		t.Fatalf("drain timeout without stalled entries: %v", err)
	}
	if stalled.Total != stuck+1 || len(stalled.Stalls) != DrainStallLimit {
		t.Fatalf("stalls listed %d of %d, want %d of %d", len(stalled.Stalls), stalled.Total, DrainStallLimit, stuck+1)
	}
	first := stalled.Stalls[0]
	if first.Source != "subject" || first.Subject != 1001 || first.Reason != "durability_wait" || first.Waited <= 0 ||
		!strings.Contains(first.Detail, "watermark 9") {
		t.Fatalf("first stall = %+v", first)
	}
	text := err.Error()
	for _, want := range []string{"subject 1001: durability_wait", "commit lsn 10 above durable watermark 9", "and 4 more", "context deadline exceeded"} {
		if !strings.Contains(text, want) {
			t.Fatalf("drain error %q does not mention %q", text, want)
		}
	}
	if got := gaugeValue(metricDrainRemaining); got != stuck+1 {
		t.Fatalf("remaining gauge = %d", got)
	}
	if got := counterTotal(metricDrainTimeout); got != 1 {
		t.Fatalf("drain timeout counter = %d", got)
	}
	// 取消同样带条目，errors.Is(context.Canceled) 不变。
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if err := m.Drain(cancelled); !errors.Is(err, context.Canceled) || !errors.As(err, &stalled) || stalled.Total != stuck+1 {
		t.Fatalf("cancelled drain: %v", err)
	}
}

func gaugeValue(name string) int64 {
	for _, metric := range metrics.Snapshot() {
		if metric.Name == name {
			return int64(metric.Value)
		}
	}
	return -1
}

func counterTotal(name string) int64 {
	var total int64
	for _, metric := range metrics.Snapshot() {
		if metric.Name == name {
			total += int64(metric.Value)
		}
	}
	return total
}

type drainingSender struct{ entered, release chan struct{} }

func (s *drainingSender) SendReliable(context.Context, nettransport.SessionID, []byte) error {
	close(s.entered)
	<-s.release
	return nil
}
func TestReopenRefusesDrainingTransportLifetime(t *testing.T) {
	sender := &drainingSender{make(chan struct{}), make(chan struct{})}
	transport, err := nettransport.NewAsyncTransport(sender, nettransport.DefaultAsyncTransportConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close(context.Background())
	adapter, _ := NewAsyncTransport(transport)
	manager, err := NewManager(ManagerConfig{Transport: adapter})
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.OpenSession(1); err != nil {
		t.Fatal(err)
	}
	if err = adapter.Push(context.Background(), 1, []byte("old")); err != nil {
		t.Fatal(err)
	}
	<-sender.entered
	defer close(sender.release)
	manager.CloseSession(1)
	if err = manager.OpenSession(1); !errors.Is(err, nettransport.ErrSessionAlreadyExists) {
		t.Fatalf("open=%v; must refuse old queue", err)
	}
	if manager.Stats().Sessions != 0 {
		t.Fatal("failed open created a fake session")
	}
}

// gatedLifecycle 让测试决定 SessionOpened 何时返回、返回什么；与 nettransport 一样，
// 只有打开成功的会话才能 Push。
type gatedLifecycle struct {
	entered    chan struct{}
	result     chan error
	mu         sync.Mutex
	registered map[SessionID]bool
}

func newGatedLifecycle() *gatedLifecycle {
	return &gatedLifecycle{entered: make(chan struct{}), result: make(chan error), registered: make(map[SessionID]bool)}
}

func (g *gatedLifecycle) SessionOpened(id SessionID) error {
	g.entered <- struct{}{}
	err := <-g.result
	if err == nil {
		g.mu.Lock()
		g.registered[id] = true
		g.mu.Unlock()
	}
	return err
}

func (g *gatedLifecycle) SessionClosed(id SessionID) {
	g.mu.Lock()
	delete(g.registered, id)
	g.mu.Unlock()
}

func (g *gatedLifecycle) Push(_ context.Context, id SessionID, _ []byte) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.registered[id] {
		return nettransport.ErrSessionNotRegistered
	}
	return nil
}

// 第一次 OpenSession 的传输注册尚未返回时，同 ID 的并发 OpenSession 不能先得到 nil；每个调用方的结果都与注册结束后的实际会话状态一致。
// 历史：RR-20260926-15。
func TestConcurrentOpenResultMatchesSessionState(t *testing.T) {
	for name, openErr := range map[string]error{"transport_accepts": nil, "transport_refuses": errors.New("transport refused")} {
		t.Run(name, func(t *testing.T) {
			gate := newGatedLifecycle()
			m := newTestManager(t, gate, ManagerConfig{})
			first := make(chan error, 1)
			go func() { first <- m.OpenSession(1) }()
			<-gate.entered
			second := m.OpenSession(1)
			gate.result <- openErr
			firstErr := <-first
			sessions := m.Stats().Sessions
			t.Logf("first open=%v concurrent open=%v sessions=%d", firstErr, second, sessions)
			if !errors.Is(firstErr, openErr) || (firstErr == nil) != (sessions == 1) {
				t.Fatalf("first open=%v but sessions=%d", firstErr, sessions)
			}
			if second == nil && sessions != 1 {
				t.Fatalf("concurrent OpenSession returned nil but no session exists (sessions=%d)", sessions)
			}
		})
	}
}

// 传输尚未确认打开前，会话不被 Subscribe / Flush 看见；否则 Flush 向未注册的传输 Push，触发假的 SessionLost 并删掉随后打开的会话。
func TestSessionInvisibleUntilTransportOpened(t *testing.T) {
	for name, openErr := range map[string]error{"transport_accepts": nil, "transport_refuses": errors.New("transport refused")} {
		t.Run(name, func(t *testing.T) {
			gate := newGatedLifecycle()
			var lost []error
			m := newTestManager(t, gate, ManagerConfig{SessionLost: func(_ SessionID, err error) { lost = append(lost, err) }})
			packs := 0
			if err := m.Register(testSubject(t, 7, &packs)); err != nil {
				t.Fatal(err)
			}
			opened := make(chan error, 1)
			go func() { opened <- m.OpenSession(1) }()
			<-gate.entered
			subscribeErr := m.Subscribe(1, 7, entity.SyncProfile{})
			flushErr := m.Flush(t.Context())
			gate.result <- openErr
			openResult := <-opened
			sessions := m.Stats().Sessions
			t.Logf("during open: subscribe=%v flush=%v; open=%v SessionLost=%v sessions=%d", subscribeErr, flushErr, openResult, lost, sessions)
			if len(lost) != 0 {
				t.Fatalf("session reported lost before its transport lifetime existed: %v", lost)
			}
			if (openResult == nil) != (sessions == 1) {
				t.Fatalf("open=%v but sessions=%d", openResult, sessions)
			}
		})
	}
}

// firstSendBlocks 让第一条发送卡住直到 release，之后的发送只计数。
type firstSendBlocks struct {
	entered, release chan struct{}
	mu               sync.Mutex
	calls            int
}

func (s *firstSendBlocks) SendReliable(context.Context, nettransport.SessionID, []byte) error {
	s.mu.Lock()
	s.calls++
	first := s.calls == 1
	s.mu.Unlock()
	if first {
		close(s.entered)
		<-s.release
	}
	return nil
}

func (s *firstSendBlocks) sends() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// 正式 AsyncTransport 下旧发送在途时，多个调用方并发重开同一 ID：每个都明确失败且可用 errors.Is 识别为可重试
// （ErrSessionClosing / ErrSessionOpening），不会有人拿到 nil 而会话不存在；旧发送退出后重试成功，新会话照常收帧。
func TestConcurrentReopenWhileDrainingIsRetryable(t *testing.T) {
	sender := &firstSendBlocks{entered: make(chan struct{}), release: make(chan struct{})}
	transport, err := nettransport.NewAsyncTransport(sender, nettransport.DefaultAsyncTransportConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close(context.Background())
	adapter, _ := NewAsyncTransport(transport)
	m := newTestManager(t, adapter, ManagerConfig{})
	open(t, m, 1)
	if err := adapter.Push(context.Background(), 1, []byte("old")); err != nil {
		t.Fatal(err)
	}
	<-sender.entered
	m.CloseSession(1)

	errs := make([]error, 8)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() { errs[i] = m.OpenSession(1) })
	}
	wg.Wait()
	sessions := m.Stats().Sessions
	t.Logf("concurrent reopen while draining: %v sessions=%d", errs, sessions)
	for _, err := range errs {
		switch {
		case errors.Is(err, ErrSessionClosing):
			if !errors.Is(err, nettransport.ErrSessionAlreadyExists) {
				t.Fatalf("ErrSessionClosing lost the transport cause: %v", err)
			}
		case errors.Is(err, ErrSessionOpening):
		default:
			t.Fatalf("reopen while old send in flight must fail with a retryable sentinel, got %v (sessions=%d)", err, sessions)
		}
	}
	if sessions != 0 {
		t.Fatalf("failed reopen left %d sessions", sessions)
	}

	close(sender.release)
	deadline := time.Now().Add(10 * time.Second)
	for err = m.OpenSession(1); errors.Is(err, ErrSessionClosing); err = m.OpenSession(1) {
		if time.Now().After(deadline) {
			t.Fatal("old send never drained")
		}
		runtime.Gosched() // 等旧 worker 退出；只让出调度，不用 sleep 制造时序
	}
	if err != nil {
		t.Fatalf("retry after drain: %v", err)
	}
	packs := 0
	if err := m.Register(testSubject(t, 7, &packs)); err != nil {
		t.Fatal(err)
	}
	mustSubscribe(t, m, 1, 7, entity.SyncProfile{})
	mustFlush(t, m)
	if err := transport.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sender.sends(); got != 2 || m.Stats().SessionsLost != 0 {
		t.Fatalf("reopened session did not receive its frame: sends=%d lost=%d", got, m.Stats().SessionsLost)
	}
}

// 真实异步队列 + 独立连接：慢端停止读取，正常端逐帧解码，背压只关闭慢会话。
type pipeLoadSender struct {
	conns   map[SessionID]net.Conn
	started chan struct{}
}

func (s *pipeLoadSender) SendReliable(ctx context.Context, id SessionID, raw []byte) error {
	conn := s.conns[id]
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetWriteDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetWriteDeadline(time.Now()) })
	defer stop()
	if id == 1 {
		select {
		case s.started <- struct{}{}:
		default:
		}
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(raw)))
	_, err := (&net.Buffers{size[:], raw}).WriteTo(conn)
	return err
}
func TestAsyncLoadSlowConsumerDoesNotDelayHealthyBaseline(t *testing.T) {
	sender := &pipeLoadSender{conns: make(map[SessionID]net.Conn), started: make(chan struct{}, 1)}
	slowServer, slowClient := net.Pipe()
	defer slowServer.Close()
	defer slowClient.Close()
	sender.conns[1] = slowServer
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	sender.conns[2] = server
	queue, err := nettransport.NewAsyncTransport(sender, nettransport.AsyncTransportConfig{ReliableQueueSize: 1, SendTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	transport, err := NewAsyncTransport(queue)
	if err != nil {
		t.Fatal(err)
	}
	m := newTestManager(t, transport, ManagerConfig{})
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = queue.Close(ctx)
	}()
	received := make(chan frame.Frame, 4)
	fail := make(chan error, 1)
	go func() {
		for range 3 {
			var size [4]byte
			_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, err := io.ReadFull(client, size[:]); err != nil {
				fail <- err
				return
			}
			raw := make([]byte, binary.BigEndian.Uint32(size[:]))
			if _, err := io.ReadFull(client, raw); err != nil {
				fail <- err
				return
			}
			f, err := DecodeFrame(raw, frame.DefaultLimits())
			if err != nil {
				fail <- err
				return
			}
			received <- f
		}
	}()
	packs := 0
	s := testSubject(t, 1, &packs)
	if err := m.Register(s); err != nil {
		t.Fatal(err)
	}
	open(t, m, 1, 2)
	for _, id := range []SessionID{1, 2} {
		mustSubscribe(t, m, id, 1, entity.SyncProfile{})
	}
	var ref frame.ObjectRef
	for i := 0; i < 3; i++ {
		if i > 0 {
			s.MarkDirty(1)
		}
		mustFlush(t, m)
		if i == 0 {
			select {
			case <-sender.started:
			case <-time.After(time.Second):
				t.Fatal("slow sender never started")
			}
		}
		select {
		case f := <-received:
			if f.Tick != uint32(i+1) || f.BaseTick != uint32(i) {
				t.Fatalf("clock: %+v", f)
			}
			if i == 0 {
				ref = f.Objects[0].Ref
			} else if f.Objects[0].Ref != ref {
				t.Fatal("healthy reference reset")
			}
			update, err := DecodeSubjectUpdate(f.Objects[0].Components[0].Data, 0)
			if err != nil || update.Version != uint64(i) || (i > 0 && update.BaseVersion != uint64(i-1)) {
				t.Fatalf("version: %+v %v", update, err)
			}
		case err := <-fail:
			t.Fatal(err)
		case <-time.After(time.Second):
			t.Fatal("healthy receiver blocked by slow session")
		}
	}
	if stats := m.Stats(); stats.Sessions != 1 || stats.SessionsLost != 1 {
		t.Fatalf("slow consumer isolation: %+v", stats)
	}
	if queue.Stats().ReliableBackpressure != 1 {
		t.Fatal("slow session did not exercise backpressure")
	}
}

// 同一 tick 用一个新对象替换一个旧对象，最终容量未增长；准入结果不应由 ID 大小决定。
func TestFullSessionCanReplaceAnObjectRegardlessOfSubjectID(t *testing.T) {
	for _, ids := range [][2]int64{{100, 1}, {1, 100}} {
		t.Run(fmt.Sprintf("%d_to_%d", ids[0], ids[1]), func(t *testing.T) {
			transport := newRecordingTransport()
			manager := newTestManager(t, transport, ManagerConfig{Limits: frame.Limits{MaxObjects: 1}})
			packs := 0
			for _, id := range ids {
				if err := manager.Register(testSubject(t, id, &packs)); err != nil {
					t.Fatal(err)
				}
			}
			open(t, manager, 1)
			mustSubscribe(t, manager, 1, ids[0], entity.SyncProfile{})
			mustFlush(t, manager)
			original := oneFrame(t, transport, 1)
			if err := manager.Unsubscribe(1, ids[0]); err != nil {
				t.Fatal(err)
			}
			mustSubscribe(t, manager, 1, ids[1], entity.SyncProfile{})
			mustFlush(t, manager)
			payloads := transport.take(1)
			if len(payloads) != 2 || manager.Stats().SessionsLost != 0 {
				t.Fatalf("legal replacement got %d frames, stats %+v", len(payloads), manager.Stats())
			}
			removed := decodeFrame(t, payloads[0])
			created := decodeFrame(t, payloads[1])
			if removed.wire.Objects[0].Operation != frame.ObjectRemove || removed.wire.Objects[0].Ref != original.wire.Objects[0].Ref {
				t.Fatalf("first frame must release the old object: %+v", removed.wire.Objects)
			}
			if created.objects[ids[1]] != frame.ObjectCreate || created.wire.BaseTick != removed.wire.Tick {
				t.Fatalf("replacement did not continue the client's frame history: %+v", created.wire)
			}
			if created.wire.Objects[0].Ref == original.wire.Objects[0].Ref {
				t.Fatal("a reused reference must advance its generation")
			}
		})
	}
}
