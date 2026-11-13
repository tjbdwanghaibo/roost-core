package cellworld

import (
	"context"
	"crypto/rand"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

// 同一个测试 Grid 的进程显式共享一个内存授权服务；它只存在于夹具，不是生产配置的隐式降级。
var testOwnerSets sync.Map

type testOwnerSet struct {
	mu       sync.Mutex
	live     map[OwnerProcess]bool
	registry *OwnerRegistry
}

func attachTestOwners(t testing.TB, g *Grid, cfg *NodeConfig) func() {
	t.Helper()
	candidate := &testOwnerSet{live: map[OwnerProcess]bool{}}
	candidate.registry, _ = NewOwnerRegistry(versionstore.NewMemoryStore[int64, OwnerRecord](), func(_ context.Context, p OwnerProcess) (bool, error) {
		candidate.mu.Lock()
		defer candidate.mu.Unlock()
		return candidate.live[p], nil
	}, versionstore.NewMemoryStore[uint32, DeathBucket]())
	loaded, existed := testOwnerSets.LoadOrStore(g, candidate)
	set := loaded.(*testOwnerSet)
	if !existed {
		t.Cleanup(func() { testOwnerSets.Delete(g) })
	}
	process := OwnerProcess{SID: cfg.SID, Incarnation: rand.Text()}
	set.mu.Lock()
	set.live[process] = true
	set.mu.Unlock()
	cfg.Owners, cfg.Incarnation = set.registry, process.Incarnation
	cfg.CheckLease = func() error {
		set.mu.Lock()
		defer set.mu.Unlock()
		if !set.live[process] {
			return errors.New("test process lost lease")
		}
		return nil
	}
	return func() { set.mu.Lock(); delete(set.live, process); set.mu.Unlock() }
}

// 同一 Base 的旧位置不能证明当前 Cell 已经停止写入。两格都没有本地 Ghost 时也只能存在一个 Real。
func TestEnterCannotCreateTwoRealsFromAnOldPosition(t *testing.T) {
	spec, _ := loopbackSpec(t, 2, 1)
	w := startWorld(t, spec)
	w.enter(1, spatial.Point{X: 12000, Y: 5000})
	a, b := CellID{0, 0}, CellID{1, 0}
	entry := w.baseState(1).Entry
	msg := &wireMsg{From: worldBaseSID, Kind: kindEnter, Enter: &enterReq{
		State: EntityState{ID: actorID(1), Kind: KindPlayer, Base: worldBaseSID, X: 1000, Y: 5000, HP: 100}, Entry: entry,
	}}
	ctx, cancel := w.ctx()
	defer cancel()
	_, err := w.cells[a].engine.RequestMulti(ctx, hActor, []int64{w.cells[a].node.cfg.Scene, actorID(1)}, []any{msg})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, settleWithin, "ownership lookup to finish", func() (bool, string) { s, ok := w.roleOf(a, actorID(1)); return !ok || s.Role != "pending", s.Role })
	w.assertSingleReal(actorID(1), b)
}

func registryForTest(t *testing.T, live func(context.Context, OwnerProcess) (bool, error)) (*OwnerRegistry, *versionstore.MemoryStore[int64, OwnerRecord]) {
	t.Helper()
	s := versionstore.NewMemoryStore[int64, OwnerRecord]()
	r, err := NewOwnerRegistry(s, live, versionstore.NewMemoryStore[uint32, DeathBucket]())
	if err != nil {
		t.Fatal(err)
	}
	return r, s
}

func TestOwnerRegistryTransferDecisionsSurviveRetries(t *testing.T) {
	r, _ := registryForTest(t, func(context.Context, OwnerProcess) (bool, error) { return true, nil })
	ctx := context.Background()
	source := OwnerProcess{101, "a"}
	target := OwnerProcess{102, "b"}
	old, err := r.claim(ctx, ownerClaim{Actor: 1, Kind: KindNPC, Process: source, Token: "first"})
	if err != nil {
		t.Fatal(err)
	}
	transfer := OwnerTransfer{ID: "handoff", Target: target.SID, Epoch: old.Grant.Epoch + 1}
	if _, err = r.begin(ctx, 1, old.Grant, transfer); err != nil {
		t.Fatal(err)
	}
	if _, err = r.reject(ctx, 1, old.Grant, transfer, target); err != nil {
		t.Fatal(err)
	}
	again, err := r.begin(ctx, 1, old.Grant, transfer)
	if err != nil || again.Phase != ownerActive || again.Grant != old.Grant {
		t.Fatalf("late begin revived rejected transfer: %+v %v", again, err)
	}
	if _, err = r.accept(ctx, 1, old.Grant, transfer, target); !errors.Is(err, ErrOwnerConflict) {
		t.Fatalf("late accept: %v", err)
	}
	transfer.ID = "next"
	transfer.Epoch++
	if _, err = r.begin(ctx, 1, old.Grant, transfer); err != nil {
		t.Fatal(err)
	}
	got, err := r.accept(ctx, 1, old.Grant, transfer, target)
	if err != nil {
		t.Fatal(err)
	}
	again, err = r.accept(ctx, 1, old.Grant, transfer, target)
	if err != nil || again != got {
		t.Fatalf("accept retry: %+v %v", again, err)
	}
	if _, err = r.retire(ctx, 1, old.Grant); !errors.Is(err, ErrOwnerConflict) {
		t.Fatalf("old owner retired target: %v", err)
	}
}

func TestOwnerRegistryClaimDoesNotRevokeAnOwnerChangedDuringLivenessCheck(t *testing.T) {
	checked, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r, _ := registryForTest(t, func(_ context.Context, p OwnerProcess) (bool, error) {
		if p.Incarnation == "a" {
			once.Do(func() { close(checked); <-resume })
			return false, nil
		}
		return true, nil
	})
	ctx := context.Background()
	old, err := r.claim(ctx, ownerClaim{Actor: 1, Kind: KindNPC, Process: OwnerProcess{101, "a"}, Token: "first"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan OwnerRecord, 1)
	errs := make(chan error, 1)
	go func() {
		got, err := r.claim(ctx, ownerClaim{Actor: 1, Kind: KindNPC, Process: OwnerProcess{103, "c"}, Token: "recover"})
		done <- got
		errs <- err
	}()
	<-checked
	transfer := OwnerTransfer{ID: "move", Target: 102, Epoch: 2}
	if _, err = r.begin(ctx, 1, old.Grant, transfer); err != nil {
		t.Fatal(err)
	}
	moved, err := r.accept(ctx, 1, old.Grant, transfer, OwnerProcess{102, "b"})
	if err != nil {
		t.Fatal(err)
	}
	close(resume)
	select {
	case got := <-done:
		if err := <-errs; err != nil || got != moved {
			t.Fatalf("revoked replacement: %+v %v", got, err)
		}
	case <-time.After(time.Second):
		t.Fatal("claim did not finish")
	}
}

func TestOwnerRegistryUncertainLivenessDoesNotGrantAnotherWriter(t *testing.T) {
	offline := errors.New("identity service unavailable")
	r, s := registryForTest(t, func(context.Context, OwnerProcess) (bool, error) { return false, offline })
	ctx := context.Background()
	first, err := r.claim(ctx, ownerClaim{Actor: 1, Kind: KindNPC, Process: OwnerProcess{101, "old"}, Token: "grant"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.claim(ctx, ownerClaim{Actor: 1, Kind: KindNPC, Process: OwnerProcess{101, "new"}, Token: "recovery"}); !errors.Is(err, offline) {
		t.Fatalf("unknown liveness: %v", err)
	}
	got, _, _ := s.Get(ctx, 1)
	if got.Value != first {
		t.Fatalf("unknown liveness changed grant: %+v", got)
	}
}

func TestOwnerRegistryAcceptAndRetireCannotBothWin(t *testing.T) {
	r, s := registryForTest(t, func(context.Context, OwnerProcess) (bool, error) { return true, nil })
	ctx := context.Background()
	source, target := OwnerProcess{101, "old"}, OwnerProcess{102, "new"}
	for id := int64(1); id <= 64; id++ {
		first, err := r.claim(ctx, ownerClaim{Actor: id, Kind: KindNPC, Process: source, Token: "grant"})
		if err != nil {
			t.Fatal(err)
		}
		transfer := OwnerTransfer{ID: "transfer", Target: 102, Epoch: 2}
		if _, err = r.begin(ctx, id, first.Grant, transfer); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		results := make(chan error, 2)
		go func() { <-start; _, err := r.accept(ctx, id, first.Grant, transfer, target); results <- err }()
		go func() { <-start; _, err := r.retire(ctx, id, first.Grant); results <- err }()
		close(start)
		accepted, rejected := 0, 0
		for range 2 {
			err := <-results
			switch {
			case err == nil:
				accepted++
			case errors.Is(err, ErrOwnerConflict):
				rejected++
			default:
				t.Fatal(err)
			}
		}
		if accepted != 1 || rejected != 1 {
			t.Fatalf("won %d lost %d", accepted, rejected)
		}
		current, _, _ := s.Get(ctx, id)
		if current.Value.Phase == ownerClosed && current.Value.Grant != first.Grant {
			t.Fatal("retired new target grant")
		}
	}
}

// 模拟 CAS 已写入，但响应在网络上丢失；应用层只能按同一操作身份重试。
type lostOwnerReplyStore struct {
	versionstore.Store[int64, OwnerRecord]
	lose atomic.Bool
}

var errLostOwnerReply = errors.New("ownership reply lost after write")

func (s *lostOwnerReplyStore) Update(ctx context.Context, id int64, m versionstore.Mutate[OwnerRecord]) (versionstore.Versioned[OwnerRecord], bool, error) {
	record, saved, err := s.Store.Update(ctx, id, m)
	if err == nil && saved && s.lose.CompareAndSwap(true, false) {
		return versionstore.Versioned[OwnerRecord]{}, false, errLostOwnerReply
	}
	return record, saved, err
}

func TestOwnerRegistryRetriesAnUnknownClaimAndAcceptWithoutNewEpoch(t *testing.T) {
	ctx := context.Background()
	memory := versionstore.NewMemoryStore[int64, OwnerRecord]()
	store := &lostOwnerReplyStore{Store: memory}
	r, err := NewOwnerRegistry(store, func(context.Context, OwnerProcess) (bool, error) { return true, nil }, versionstore.NewMemoryStore[uint32, DeathBucket]())
	if err != nil {
		t.Fatal(err)
	}
	req := ownerClaim{Actor: 1, Kind: KindNPC, Process: OwnerProcess{101, "old"}, Token: "grant"}
	store.lose.Store(true)
	if _, err = r.claim(ctx, req); !errors.Is(err, errLostOwnerReply) {
		t.Fatalf("claim error: %v", err)
	}
	first, err := r.claim(ctx, req)
	if err != nil || first.Grant.Epoch != 1 {
		t.Fatalf("claim retry: %+v %v", first, err)
	}
	transfer := OwnerTransfer{ID: "transfer", Target: 102, Epoch: 2}
	target := OwnerProcess{102, "new"}
	if _, err = r.begin(ctx, 1, first.Grant, transfer); err != nil {
		t.Fatal(err)
	}
	store.lose.Store(true)
	if _, err = r.accept(ctx, 1, first.Grant, transfer, target); !errors.Is(err, errLostOwnerReply) {
		t.Fatalf("accept error: %v", err)
	}
	current, err := r.accept(ctx, 1, first.Grant, transfer, target)
	if err != nil || current.Grant.ID != "transfer" || current.Grant.Epoch != 2 {
		t.Fatalf("accept retry: %+v %v", current, err)
	}
}

func TestOwnerRegistryClosedEntryAndGenerationDoNotReset(t *testing.T) {
	ctx := context.Background()
	r, _ := registryForTest(t, func(context.Context, OwnerProcess) (bool, error) { return false, nil })
	req := ownerClaim{Actor: 1, Kind: KindPlayer, Base: 900, Entry: 7, Process: OwnerProcess{101, "old"}, Token: "first"}
	first, err := r.claim(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.retireEntry(ctx, 1, first.Grant, 7); err != nil {
		t.Fatal(err)
	}
	req.Token = "late"
	req.Process = OwnerProcess{102, "next"}
	if _, err = r.claim(ctx, req); !errors.Is(err, ErrOwnerClosed) {
		t.Fatalf("closed entry: %v", err)
	}
	req.Entry = 8
	next, err := r.claim(ctx, req)
	if err != nil || next.Grant.Epoch <= first.Grant.Epoch {
		t.Fatalf("new entry: %+v %v", next, err)
	}
	req.Process.Incarnation = "restart"
	req.Token = "recovery"
	recovered, err := r.claim(ctx, req)
	if err != nil || recovered.Grant.Epoch <= next.Grant.Epoch {
		t.Fatalf("recovery: %+v %v", recovered, err)
	}
}

type blockedOwnerStore struct {
	versionstore.Store[int64, OwnerRecord]
	entered chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (s *blockedOwnerStore) Update(ctx context.Context, id int64, m versionstore.Mutate[OwnerRecord]) (versionstore.Versioned[OwnerRecord], bool, error) {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.resume:
	case <-ctx.Done():
		return versionstore.Versioned[OwnerRecord]{}, false, ctx.Err()
	}
	return s.Store.Update(ctx, id, m)
}

func TestOwnershipIOReleasesEntityLocksAndRetriesAFullQueue(t *testing.T) {
	store := &blockedOwnerStore{Store: versionstore.NewMemoryStore[int64, OwnerRecord](), entered: make(chan struct{}), resume: make(chan struct{})}
	registry, err := NewOwnerRegistry(store, func(context.Context, OwnerProcess) (bool, error) { return true, nil }, versionstore.NewMemoryStore[uint32, DeathBucket]())
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := loopbackSpec(t, 1, 1)
	spec.cellMod = func(cfg *NodeConfig) { cfg.Owners = registry; cfg.OwnershipWorkers = 1; cfg.OwnershipQueue = 1 }
	w := startWorld(t, spec)
	var release sync.Once
	defer release.Do(func() { close(store.resume) })
	for id := int64(1); id <= 3; id++ {
		w.base.player(id)
		ctx, cancel := w.ctx()
		err := w.base.node.Enter(ctx, playerEntityID(id), actorID(id), spatial.Point{X: 1000 + id, Y: 5000}, 100)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-store.entered:
	case <-time.After(settleWithin):
		t.Fatal("ownership I/O did not start")
	}
	cell := w.cells[CellID{}]
	waitFor(t, settleWithin, "bounded queue to reject extra work", func() bool { return cell.node.Stats().OwnershipQueueFull > 0 })
	// I/O 在等 Redis 时，同一个 Actor 的请求仍可取得 Nest 本地锁并完成。
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = cell.engine.Request(ctx, hInput, actorID(1), []any{&wireMsg{From: worldBaseSID, Kind: kindInput, Input: &inputReq{ID: actorID(1), Entry: 1, VX: 500, Seq: 1}}})
	if err != nil {
		t.Fatalf("ownership I/O held the actor lock: %v", err)
	}
	release.Do(func() { close(store.resume) })
	for id := int64(1); id <= 3; id++ {
		w.waitView(id, settleWithin, "pending actor recovers after backpressure", func(v View) bool { return v.Self.ID == actorID(id) })
	}
}

func TestLeaveWhileClaimIsPendingNeverActivatesTheActor(t *testing.T) {
	created := map[int64]*plainActor{}
	n := plainNode(t, worldGrid(t, 1, 1), CellID{}, created)
	r := NewRegion()
	now := time.Now()
	req := &enterReq{State: EntityState{ID: actorID(80), X: 1000, Y: 5000, HP: 100}, Entry: 3}
	if err := n.onEnter(r, nil, 900, req, now, &txEffects{}); err != nil {
		t.Fatal(err)
	}
	a := created[req.State.ID]
	// 授权已在存储中生效，完成回调还没回到 Nest；此时收到离场。
	if _, err := n.executeOwner(context.Background(), n.ownerWork(a.st)); err != nil {
		t.Fatal(err)
	}
	n.onLeave(r, a, &leaveReq{ID: a.ID(), Entry: 3}, now, &txEffects{})
	resolvePlain(t, n, r, a)
	if a.st.Role != RoleGone {
		t.Fatalf("left actor activated: %+v", a.st)
	}
	resolvePlain(t, n, r, a)
	_, err := n.cfg.Owners.claim(context.Background(), ownerClaim{Actor: a.ID(), Kind: KindPlayer, Base: 900, Entry: 3, Process: n.ownerProcess(), Token: "late-enter"})
	if !errors.Is(err, ErrOwnerClosed) {
		t.Fatalf("late enter resurrected a closed entry: %v", err)
	}
}

func TestExpiredCellLeaseRollsBackAndSuppressesEffects(t *testing.T) {
	expired := false
	want := errors.New("lease expired")
	n := &Node{cfg: NodeConfig{CheckLease: func() error {
		if expired {
			return want
		}
		return nil
	}}}
	state := &undoCell[int]{}
	published := false
	_, err := nest.RunDetachedTransaction(context.Background(), nullCommitter{}, "lease boundary", func() (any, error) {
		if err := n.guardTransaction(); err != nil {
			return nil, err
		}
		state.set(9)
		n.registerCommit(&txEffects{hooks: []func(){func() { published = true }}})
		expired = true
		return nil, nil
	})
	if !errors.Is(err, want) || state.v != 0 || published {
		t.Fatalf("expired commit: err=%v state=%d published=%v", err, state.v, published)
	}
	// 已经提交、尚未执行的外部效果也必须检查，不能随 SIGCONT 放出旧队列。
	n.registerCommit(&txEffects{hooks: []func(){func() { published = true }}})
	if published {
		t.Fatal("expired writer emitted an effect")
	}
}
