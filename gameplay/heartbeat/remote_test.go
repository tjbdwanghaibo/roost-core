package heartbeat

import (
	"context"
	"fmt"
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
// Draining / Fenced / Quarantined → 拒绝；Unknown / Recovering → 取得写许可并转到 LocalOwned。每个准入的写持有
// 写门（读锁）到批次 Close；所有权转移取写门（写锁）先排空已准入的本地写，与真实 TransferRemoteOwnership 一致。
type fakeRemoteManager struct {
	entity.IRemoteEntityManager
	sid      int32
	auth     *remoteAuthority
	manager  *entity.EntityManager
	gate     sync.RWMutex
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
		if err := live.TransitionRemoteOwnership(entity.RemoteOwnershipLocalOwned); err != nil {
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
// B 在成为拥有方之前不执行任何一拍：两边不会同时跳。共享模式下（P1）两边都不跳。
func TestRemoteHeartbeatFollowsOwnershipWithoutDoubleBeats(t *testing.T) {
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

	// 共享模式：拥有方切到 Shared 后两边都不投（P1 只在 LocalOwned 跳）。
	lockedTransition(t, b.e, entity.RemoteOwnershipSharing, entity.RemoteOwnershipShared)
	before := b.f.driver.Stats().Delivered
	stepBoth(50)
	if got := b.f.driver.Stats().Delivered; got != before {
		t.Fatalf("delivered %d beats in shared mode", got-before)
	}
}
