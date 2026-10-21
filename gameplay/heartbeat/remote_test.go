package heartbeat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// remoteAuthority 扮演 Remote 的所有权权威（Mongo marker）：记当前拥有方 sid。
type remoteAuthority struct {
	mu    sync.Mutex
	owner int32
}

func (a *remoteAuthority) get() int32 { a.mu.Lock(); defer a.mu.Unlock(); return a.owner }
func (a *remoteAuthority) set(sid int32) {
	a.mu.Lock()
	a.owner = sid
	a.mu.Unlock()
}

// fakeRemoteManager 模拟 remoteentity.Manager 的写准入（beginWrite）：权威拥有方不是本进程 → fenced；实例处于
// Draining / Fenced / Quarantined → 拒绝；Unknown / Recovering → 取得写许可并转到 LocalOwned（权威共享时转 Shared）；
// Sharing / Shared → 准入（共享模式各写者经分布式锁写）。每个准入的写持有
// 写门（读锁）到批次 Close；所有权转移取写门（写锁）先排空已准入的本地写，与真实 TransferRemoteOwnership 一致。
type fakeRemoteManager struct {
	entity.IRemoteEntityManager
	sid      int32
	auth     *remoteAuthority
	manager  *entity.EntityManager
	gate     sync.RWMutex
	shared   atomic.Bool // 权威处于共享模式：Unknown / Recovering 准入后转 Shared（真实 newWriteLease 的 marked 分支）
	refused  atomic.Int32
	admitted atomic.Int32
}

func (m *fakeRemoteManager) PrepareRemoteWriteBatch(_ context.Context, ids []int64) (entity.RemoteWriteBatch, error) {
	m.gate.RLock()
	fail := func(err error) (entity.RemoteWriteBatch, error) {
		m.gate.RUnlock()
		m.refused.Add(1)
		return nil, err
	}
	if m.auth.get() != m.sid {
		return fail(fmt.Errorf("%w: owner is %d", entity.ErrRemoteFenced, m.auth.get()))
	}
	live, _ := m.manager.Get(ids[0]).(entity.IThreadSafeRemoteEntity)
	if live == nil {
		return fail(entity.ErrRemoteRejected)
	}
	switch live.RemoteOwnershipState() {
	case entity.RemoteOwnershipDraining, entity.RemoteOwnershipFenced, entity.RemoteOwnershipQuarantined:
		return fail(entity.ErrRemoteOwnerTransition)
	case entity.RemoteOwnershipUnknown, entity.RemoteOwnershipRecovering:
		next := entity.RemoteOwnershipLocalOwned
		if m.shared.Load() {
			next = entity.RemoteOwnershipShared
		}
		if err := live.TransitionRemoteOwnership(next); err != nil {
			return fail(err)
		}
	}
	m.admitted.Add(1)
	return &fakeRemoteBatch{ids: ids, release: m.gate.RUnlock}, nil
}

type fakeRemoteBatch struct {
	entity.RemoteWriteBatch
	ids     []int64
	release func()
	once    sync.Once
}

func (b *fakeRemoteBatch) EntityIDs() []int64                                   { return b.ids }
func (b *fakeRemoteBatch) FinalizeLocked(entity.RemoteTransactionOutcome) error { return nil }
func (b *fakeRemoteBatch) Commits() []entity.RemoteCommit                       { return nil }
func (b *fakeRemoteBatch) Commit(context.Context) ([]entity.RemoteCommitReceipt, error) {
	return nil, nil
}
func (b *fakeRemoteBatch) Abort(context.Context, error) error         { return nil }
func (b *fakeRemoteBatch) Indeterminate(context.Context, error) error { return nil }
func (b *fakeRemoteBatch) Close(context.Context) error {
	b.once.Do(b.release)
	return nil
}

func lockedTransition(t *testing.T, e entity.IThreadSafeRemoteEntity, states ...entity.RemoteOwnershipState) {
	t.Helper()
	e.GetMutex().Lock()
	defer e.GetMutex().Unlock()
	for _, s := range states {
		if err := e.TransitionRemoteOwnership(s); err != nil {
			t.Fatal(err)
		}
	}
}

// Remote 托管实体的心跳只在当前拥有方跳；所有权从进程 A 转到 B 时，A 在离开 LocalOwned 之后不再执行任何一拍，
// B 在成为拥有方之前不执行任何一拍：两边不会同时跳。共享模式见下面的用例（不允许挂载）。
func TestRemoteHeartbeatFollowsOwnershipWithoutDoubleBeats(t *testing.T) {
	forEachDelivery(t, func(t *testing.T) {
		auth := &remoteAuthority{owner: 1}
		type proc struct {
			f      *nestFixture
			remote *fakeRemoteManager
			e      *remoteHBEntity
			reg    *Reg
		}
		var seq atomic.Int64
		type event struct {
			proc string
			seq  int64
		}
		var mu sync.Mutex
		var events []event
		logEvent := func(p string) {
			mu.Lock()
			events = append(events, event{p, seq.Add(1)})
			mu.Unlock()
		}
		id := mustID(t, 41, kindRemote)
		newProc := func(name string, sid int32) *proc {
			p := &proc{remote: &fakeRemoteManager{sid: sid, auth: auth}}
			p.f = newNestFixture(t, nil, nest.NestOptionWithRemoteEntityManager(p.remote))
			p.remote.manager = p.f.manager
			e := &remoteHBEntity{RemoteEntityBase: entity.NewRemoteEntityBase(id, entity.EntityCategoryRemote, true, kindRemote)}
			e.host = p.f.driver.NewHost(e)
			p.e = e
			r, err := e.host.Register("x", 50*time.Millisecond, func(Beat) error { logEvent(name); return nil })
			if err != nil {
				t.Fatal(err)
			}
			r.Start()
			p.reg = r
			p.f.manager.Add(e)
			return p
		}
		a := newProc("A", 1)
		b := newProc("B", 2)
		// B 上留着一份早先被转走的实例（Fenced）：不投递。
		lockedTransition(t, b.e, entity.RemoteOwnershipRecovering, entity.RemoteOwnershipFenced)
		stepBoth := func(frames int) {
			for range frames {
				a.f.step(1)
				b.f.step(1)
				a.f.idle()
				b.f.idle()
			}
		}
		stepBoth(50) // 10 个周期

		// 所有权转移：排空 A 已准入的写（写门）→ A 实例 Draining → 权威改到 B → A 实例 Fenced；
		// B 收到新租约：Fenced → Recovering → LocalOwned。驱动在另一个 goroutine 里同时推进。
		var marker int64
		done := make(chan struct{})
		go func() {
			defer close(done)
			for range 25 {
				a.f.step(1)
				b.f.step(1)
			}
		}()
		a.remote.gate.Lock()
		lockedTransition(t, a.e, entity.RemoteOwnershipDraining)
		auth.set(2)
		lockedTransition(t, a.e, entity.RemoteOwnershipFenced)
		marker = seq.Add(1)
		a.remote.gate.Unlock()
		lockedTransition(t, b.e, entity.RemoteOwnershipRecovering, entity.RemoteOwnershipLocalOwned)
		<-done
		// A 可能有拍在途（准入被拒、handler 没执行）：卡住兜底按 10s 重投，重投也只会被丢弃。推进 12s 覆盖它。
		stepBoth(1200)

		mu.Lock()
		var beforeA, afterA, beforeB, afterB int
		for _, ev := range events {
			switch {
			case ev.proc == "A" && ev.seq < marker:
				beforeA++
			case ev.proc == "A":
				afterA++
			case ev.seq < marker:
				beforeB++
			default:
				afterB++
			}
		}
		mu.Unlock()
		if afterA != 0 || beforeB != 0 {
			t.Fatalf("double beating: A ran %d beats after leaving ownership, B ran %d before owning (events %v, marker %d)", afterA, beforeB, events, marker)
		}
		if beforeA < 9 || afterB < 200 {
			t.Fatalf("owner beats: A %d before the transfer, B %d after (want ≥ 9 and ≥ 200)", beforeA, afterB)
		}
		if a.remote.admitted.Load() < int32(beforeA) || b.remote.admitted.Load() < int32(afterB) {
			t.Fatalf("beats did not go through the Remote write admission: A %d B %d", a.remote.admitted.Load(), b.remote.admitted.Load())
		}
		if st := a.f.driver.Stats(); st.DroppedNotOwner == 0 {
			t.Fatalf("A kept delivering after losing ownership: %+v", st)
		}
		if st := b.f.driver.Stats(); st.DroppedNotOwner < 10 {
			t.Fatalf("B delivered while fenced: %+v", st)
		}
	})
}

// stepIdle 逐格推进，每格等引擎排空（被准入拒绝的拍不清在途，不能按在途标志等）。
func stepIdle(f *nestFixture, frames int) {
	for range frames {
		f.step(1)
		f.idle()
	}
}

// newSharedTestProc 是一个进程（真实 Nest + 模拟 Remote 写准入）上的一个 Remote 托管实体，实例已发布、还没登记心跳。
func newSharedTestProc(t *testing.T, unique int64) (*nestFixture, *fakeRemoteManager, *remoteHBEntity) {
	t.Helper()
	remote := &fakeRemoteManager{sid: 1, auth: &remoteAuthority{owner: 1}}
	f := newNestFixture(t, nil, nest.NestOptionWithRemoteEntityManager(remote))
	remote.manager = f.manager
	e := &remoteHBEntity{RemoteEntityBase: entity.NewRemoteEntityBase(mustID(t, unique, kindRemote), entity.EntityCategoryRemote, true, kindRemote)}
	e.host = f.driver.NewHost(e)
	f.manager.Add(e)
	return f, remote, e
}

// Remote 共享模式（Sharing / Shared）下不允许挂载心跳（维护者 2026-10-21）：各写者实例都是 Shared，分不出谁该跳。
// 实例已在共享模式时登记直接拒绝。
func TestRegisterOnASharedRemoteEntityIsRejected(t *testing.T) {
	forEachDelivery(t, func(t *testing.T) {
		for _, states := range [][]entity.RemoteOwnershipState{
			{entity.RemoteOwnershipShared},
			{entity.RemoteOwnershipLocalOwned, entity.RemoteOwnershipSharing},
		} {
			_, _, e := newSharedTestProc(t, 50+int64(len(states)))
			lockedTransition(t, e, states...)
			e.GetMutex().Lock()
			r, err := e.host.Register("x", 100*time.Millisecond, func(Beat) error { return nil })
			e.GetMutex().Unlock()
			if err == nil {
				t.Fatalf("Register on a Remote entity in %s succeeded (registration %v), want a refusal", e.RemoteOwnershipState(), r != nil)
			}
			if !errors.Is(err, ErrHeartbeatRemoteShared) || r != nil {
				t.Fatalf("Register on a Remote entity in %s: %v (registration %v), want ErrHeartbeatRemoteShared", e.RemoteOwnershipState(), err, r != nil)
			}
		}
	})
}

// 登记时合法、运行期才进入共享模式（EnterRemoteSharedMode：LocalOwned → Sharing → Shared）：已挂的心跳由下一拍在实体锁内
// 停掉（计数、Warn），不静默停跳；共享期间 Start 返回错误；回到独占（LeaveRemoteSharedMode）后业务可以重新 Start。
func TestEnteringRemoteSharedModeStopsTheHeartbeat(t *testing.T) {
	forEachDelivery(t, func(t *testing.T) {
		f, _, e := newSharedTestProc(t, 61)
		logs := &countingHandler{}
		f.driver.config.Logger = slog.New(logs)
		lockedTransition(t, e, entity.RemoteOwnershipLocalOwned)
		var runs atomic.Int32
		e.GetMutex().Lock()
		r, err := e.host.Register("x", 50*time.Millisecond, func(Beat) error { runs.Add(1); return nil })
		if err == nil {
			err = r.Start()
		}
		e.GetMutex().Unlock()
		if err != nil {
			t.Fatal(err)
		}
		stepIdle(f, 20)
		if runs.Load() < 3 {
			t.Fatalf("beats before shared mode: %d, want ≥ 3", runs.Load())
		}

		lockedTransition(t, e, entity.RemoteOwnershipSharing, entity.RemoteOwnershipShared)
		before := runs.Load()
		stepIdle(f, 100)
		e.GetMutex().Lock()
		active := r.Active()
		e.GetMutex().Unlock()
		if active || runs.Load() != before {
			t.Fatalf("after entering shared mode: registration active=%v, callbacks run %d (want stopped, 0); stats %+v",
				active, runs.Load()-before, f.driver.Stats())
		}
		if st := f.driver.Stats(); st.StoppedShared != 1 || st.Registered != 0 {
			t.Fatalf("stats after the shared-mode stop: %+v, want StoppedShared 1 and nothing registered", st)
		}
		f.driver.flush(time.Now())
		if logs.count() != 1 {
			t.Fatalf("warns after the shared-mode stop = %d, want 1", logs.count())
		}

		// 共享期间 Start 返回错误、不启动。
		e.GetMutex().Lock()
		err = r.Start()
		active = r.Active()
		e.GetMutex().Unlock()
		if !errors.Is(err, ErrHeartbeatRemoteShared) || active {
			t.Fatalf("Start in shared mode: err %v active %v, want ErrHeartbeatRemoteShared and still stopped", err, active)
		}

		// LeaveRemoteSharedMode：Shared → Draining → LocalOwned 之后业务重新 Start，照常跳。
		lockedTransition(t, e, entity.RemoteOwnershipDraining, entity.RemoteOwnershipLocalOwned)
		e.GetMutex().Lock()
		err = r.Start()
		e.GetMutex().Unlock()
		if err != nil {
			t.Fatalf("Start after leaving shared mode: %v", err)
		}
		before = runs.Load()
		stepIdle(f, 20)
		if runs.Load()-before < 3 {
			t.Fatalf("beats after leaving shared mode and restarting: %d, want ≥ 3", runs.Load()-before)
		}
	})
}

// 第二个写者首次接触共享实体：加载时实例是 Unknown（登记合法），第一拍的 Remote 写准入把它转到 Shared；这一拍在实体锁内
// 停掉心跳，回调不执行。
func TestFirstAdmissionIntoSharedModeStopsTheHeartbeat(t *testing.T) {
	forEachDelivery(t, func(t *testing.T) {
		f, remote, e := newSharedTestProc(t, 71)
		remote.shared.Store(true)
		var runs atomic.Int32
		e.GetMutex().Lock()
		r, err := e.host.Register("x", 50*time.Millisecond, func(Beat) error { runs.Add(1); return nil })
		if err == nil {
			err = r.Start()
		}
		e.GetMutex().Unlock()
		if err != nil {
			t.Fatalf("Register on an Unknown Remote entity: %v", err)
		}
		stepIdle(f, 100)
		e.GetMutex().Lock()
		active := r.Active()
		e.GetMutex().Unlock()
		if e.RemoteOwnershipState() != entity.RemoteOwnershipShared {
			t.Fatalf("admission left the instance in %s, want shared", e.RemoteOwnershipState())
		}
		if active || runs.Load() != 0 {
			t.Fatalf("after the first admission into shared mode: registration active=%v, callbacks run %d (want stopped, 0); stats %+v",
				active, runs.Load(), f.driver.Stats())
		}
		if st := f.driver.Stats(); st.StoppedShared != 1 || st.Delivered != 1 || st.Registered != 0 {
			t.Fatalf("stats: %+v, want one delivered beat that stopped the registration", st)
		}
	})
}

// 进入共享模式时停掉实体上全部已挂的心跳（多个登记、不同档位），每个计一次；Warn 按 WarnInterval 限频，间隔内只累计。
func TestSharedModeStopsEveryRegistrationAndWarnsAtALimitedRate(t *testing.T) {
	forEachDelivery(t, func(t *testing.T) {
		f, _, e1 := newSharedTestProc(t, 81)
		logs := &countingHandler{}
		f.driver.config.Logger = slog.New(logs)
		e2 := &remoteHBEntity{RemoteEntityBase: entity.NewRemoteEntityBase(mustID(t, 82, kindRemote), entity.EntityCategoryRemote, true, kindRemote)}
		e2.host = f.driver.NewHost(e2)
		f.manager.Add(e2)

		var regs []*Reg
		for _, e := range []*remoteHBEntity{e1, e2} {
			lockedTransition(t, e, entity.RemoteOwnershipLocalOwned)
			e.GetMutex().Lock()
			for i, period := range []time.Duration{50 * time.Millisecond, time.Second} {
				r, err := e.host.Register(fmt.Sprint("r", i), period, func(Beat) error { return nil })
				if err == nil {
					err = r.Start()
				}
				if err != nil {
					e.GetMutex().Unlock()
					t.Fatal(err)
				}
				regs = append(regs, r)
			}
			e.GetMutex().Unlock()
		}
		stepIdle(f, 10)

		t0 := time.Unix(1_900_000_000, 0)
		lockedTransition(t, e1, entity.RemoteOwnershipSharing, entity.RemoteOwnershipShared)
		stepIdle(f, 110) // 覆盖 1s 档的一个周期
		if st := f.driver.Stats(); st.StoppedShared != 2 || st.Registered != 2 {
			t.Fatalf("after e1 entered shared mode: %+v, want both of its registrations stopped (StoppedShared 2, 2 left)", st)
		}
		f.driver.flush(t0)
		if logs.count() != 1 {
			t.Fatalf("warns after the first stops = %d, want 1", logs.count())
		}

		lockedTransition(t, e2, entity.RemoteOwnershipSharing, entity.RemoteOwnershipShared)
		stepIdle(f, 110)
		f.driver.flush(t0.Add(time.Second))
		if logs.count() != 1 {
			t.Fatalf("warns inside the interval = %d, want still 1", logs.count())
		}
		f.driver.flush(t0.Add(DefaultWarnInterval))
		if logs.count() != 2 {
			t.Fatalf("warns after the interval = %d, want 2", logs.count())
		}
		for _, r := range regs {
			r.host.owner.GetMutex().Lock()
			active := r.Active()
			r.host.owner.GetMutex().Unlock()
			if active {
				t.Fatalf("registration %s on entity %d still active in shared mode", r.Name(), r.host.id)
			}
		}
		if st := f.driver.Stats(); st.StoppedShared != 4 || st.Registered != 0 {
			t.Fatalf("after both entities entered shared mode: %+v, want StoppedShared 4 and nothing registered", st)
		}
	})
}

// 维护者决定（2026-10-21）：从共享模式回到独占（LeaveRemoteSharedMode：Shared → Draining → LocalOwned）不自动恢复被停掉的
// 心跳；业务调用 Start 之前保持停止、不跳，Start 之后照常跳。
func TestLeavingSharedModeDoesNotResumeTheHeartbeat(t *testing.T) {
	forEachDelivery(t, func(t *testing.T) {
		f, _, e := newSharedTestProc(t, 91)
		lockedTransition(t, e, entity.RemoteOwnershipLocalOwned)
		var runs atomic.Int32
		e.GetMutex().Lock()
		r, err := e.host.Register("x", 50*time.Millisecond, func(Beat) error { runs.Add(1); return nil })
		if err != nil {
			e.GetMutex().Unlock()
			t.Fatal(err)
		}
		r.Start()
		e.GetMutex().Unlock()
		stepIdle(f, 20)

		lockedTransition(t, e, entity.RemoteOwnershipSharing, entity.RemoteOwnershipShared)
		stepIdle(f, 20)
		lockedTransition(t, e, entity.RemoteOwnershipDraining, entity.RemoteOwnershipLocalOwned)
		before := runs.Load()
		stepIdle(f, 50)
		e.GetMutex().Lock()
		active := r.Active()
		e.GetMutex().Unlock()
		if active || runs.Load() != before {
			t.Fatalf("back to local ownership without Start: active=%v, callbacks run %d (want stopped, 0); stats %+v",
				active, runs.Load()-before, f.driver.Stats())
		}

		e.GetMutex().Lock()
		r.Start()
		active = r.Active()
		e.GetMutex().Unlock()
		stepIdle(f, 20)
		if !active || runs.Load()-before < 3 {
			t.Fatalf("after Start back in local ownership: active=%v, beats %d (want ≥ 3)", active, runs.Load()-before)
		}
	})
}
