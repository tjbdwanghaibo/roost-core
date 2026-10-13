// 所有权与 marker：Claim 只选出一个 owner，模式切换推进 epoch，丢回复时随权威收敛或冻结，键前缀隔离与参数校验。

package remoteentity

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

func TestConcurrentOwnershipClaimElectsExactlyOneOwner(t *testing.T) {
	const kind entity.EntityKind = 132
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id := testRemoteFullIDWithKind(1412, 1, kind)
	store := newMockMarkerStore()
	first := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	second := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 2000)
	first.SetOwnershipStore(store)
	second.SetOwnershipStore(store)

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, mgr := range []*Manager{first, second} {
		wg.Add(1)
		go func(m *Manager) {
			defer wg.Done()
			_, err := m.ClaimRemoteOwnership(context.Background(), id)
			errs <- err
		}(mgr)
	}
	wg.Wait()
	close(errs)
	succeeded, fenced := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, entity.ErrRemoteFenced):
			fenced++
		default:
			t.Fatalf("unexpected claim error: %v", err)
		}
	}
	if succeeded != 1 || fenced != 1 {
		t.Fatalf("claim results: succeeded=%d fenced=%d", succeeded, fenced)
	}
	lease, found, err := first.GetRemoteOwnership(context.Background(), id)
	if err != nil || !found || !validOwnershipLease(lease) || (lease.OwnerSid != 1000 && lease.OwnerSid != 2000) {
		t.Fatalf("ownership lease=%+v found=%v err=%v", lease, found, err)
	}
}

func TestOwnershipModeTransitionsAdvanceMarkerEpoch(t *testing.T) {
	const kind entity.EntityKind = 133
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id := testRemoteFullIDWithKind(1413, 1, kind)
	mgr := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	mgr.SetOwnershipStore(newMockMarkerStore())

	claimed, err := mgr.ClaimRemoteOwnership(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := mgr.EnterRemoteSharedMode(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	local, err := mgr.LeaveRemoteSharedMode(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Shared || !shared.Shared || local.Shared || claimed.MarkerEpoch+1 != shared.MarkerEpoch || shared.MarkerEpoch+1 != local.MarkerEpoch {
		t.Fatalf("claimed=%+v shared=%+v local=%+v", claimed, shared, local)
	}
}

type lostReplyModeStore struct {
	*lostReplyMarkerStore
	loseEnter, loseLeave bool
	failBefore           bool
}

func (s *lostReplyModeStore) EnterSharedExpected(ctx context.Context, id int64, expected entity.RemoteEntityMarkerLease) (entity.RemoteEntityMarkerLease, error) {
	if s.failBefore {
		return entity.RemoteEntityMarkerLease{}, errors.New("i/o timeout before send")
	}
	next, err := s.mockMarkerStore.EnterSharedExpected(ctx, id, expected)
	if err != nil {
		return next, err
	}
	if s.afterTransfer != nil {
		s.afterTransfer()
	}
	if s.loseEnter {
		return entity.RemoteEntityMarkerLease{}, errors.New("i/o timeout: enter-shared reply lost")
	}
	return next, nil
}

func (s *lostReplyModeStore) LeaveSharedExpected(ctx context.Context, id int64, expected entity.RemoteEntityMarkerLease) (entity.RemoteEntityMarkerLease, error) {
	if s.failBefore {
		return entity.RemoteEntityMarkerLease{}, errors.New("i/o timeout before send")
	}
	next, err := s.mockMarkerStore.LeaveSharedExpected(ctx, id, expected)
	if err != nil {
		return next, err
	}
	if s.loseLeave {
		return entity.RemoteEntityMarkerLease{}, errors.New("i/o timeout: leave-shared reply lost")
	}
	return next, nil
}

func modeSetup(t *testing.T, kind entity.EntityKind, seq int64) (*Manager, *lostReplyModeStore, *testRemoteEntity) {
	t.Helper()
	mgr, inner, live := indeterminateTransferSetup(t, kind, seq)
	store := &lostReplyModeStore{lostReplyMarkerStore: inner}
	mgr.SetOwnershipStore(store)
	return mgr, store, live
}

func probeAdmission(t *testing.T, mgr *Manager, live *testRemoteEntity) error {
	t.Helper()
	batch, err := mgr.PrepareRemoteWriteBatch(context.Background(), []int64{live.GUId()})
	if err == nil {
		_ = batch.Abort(context.Background(), errors.New("probe"))
		_ = batch.Close(context.Background())
	}
	return err
}

func TestEnterSharedPromiseLostReplyFollowsAuthorityIntoSharedMode(t *testing.T) {
	mgr, store, live := modeSetup(t, 251, 1501)
	store.loseEnter = true
	ctx := context.Background()

	next, err := mgr.EnterRemoteSharedMode(ctx, live.GUId())
	authority := store.leases[live.GUId()]
	if !store.marks[live.GUId()] {
		t.Fatal("test premise: authority must be shared after the executed CAS")
	}
	state := live.RemoteOwnershipState()
	if state == entity.RemoteOwnershipLocalOwned {
		t.Fatalf("authority_shared=%v state=%s admission=%v: entered shared at authority but old local-owned write admitted",
			store.marks[live.GUId()], state, probeAdmission(t, mgr, live))
	}
	// 权威已确认进入共享:结果应与一次正常成功的 EnterShared 一致。
	if err != nil || !next.Shared || state != entity.RemoteOwnershipShared {
		t.Fatalf("enter-shared applied but reported err=%v next=%+v state=%s authority=%+v", err, next, state, authority)
	}
	if live.RemoteVersionVector().MarkerEpoch != authority.MarkerEpoch {
		t.Fatalf("live marker epoch %d != authority %d", live.RemoteVersionVector().MarkerEpoch, authority.MarkerEpoch)
	}
	// 之后的准入走共享路径(需要分布式锁),而不是独占快捷路径。
	if err := probeAdmission(t, mgr, live); err != nil {
		t.Fatalf("shared-mode admission after recovery: %v", err)
	}
	if w, ok := mgr.get(live.GUId()); !ok || !w.isMarked() {
		t.Fatal("wrapper marker did not follow the authority into shared mode")
	}
}

func TestEnterSharedPromiseLostReplyFreezesUntilAuthorityAnswers(t *testing.T) {
	mgr, store, live := modeSetup(t, 252, 1502)
	store.loseEnter = true
	store.afterTransfer = func() { store.blackout.Store(true) }
	ctx := context.Background()

	if _, err := mgr.EnterRemoteSharedMode(ctx, live.GUId()); err == nil {
		t.Fatal("unknown outcome with unreachable authority must not report success")
	}
	if state := live.RemoteOwnershipState(); state == entity.RemoteOwnershipLocalOwned || state == entity.RemoteOwnershipShared {
		t.Fatalf("unknown outcome restored a writable mode: live=%s", state)
	}
	if err := probeAdmission(t, mgr, live); err == nil {
		t.Fatal("write admitted while mode is unknown and authority unreachable")
	}
	store.blackout.Store(false)
	if err := probeAdmission(t, mgr, live); err != nil {
		t.Fatalf("after the authority answered (shared, ours): %v", err)
	}
	if state := live.RemoteOwnershipState(); state != entity.RemoteOwnershipShared {
		t.Fatalf("thawed into %s, want shared as the authority says", state)
	}
}

func TestLeaveSharedPromiseLostReplyFollowsAuthorityIntoLocalOwned(t *testing.T) {
	mgr, store, live := modeSetup(t, 253, 1503)
	ctx := context.Background()
	if _, err := mgr.EnterRemoteSharedMode(ctx, live.GUId()); err != nil {
		t.Fatal(err)
	}
	store.loseLeave = true
	next, err := mgr.LeaveRemoteSharedMode(ctx, live.GUId())
	if store.marks[live.GUId()] {
		t.Fatal("test premise: authority must have left shared mode")
	}
	if err != nil || next.Shared || live.RemoteOwnershipState() != entity.RemoteOwnershipLocalOwned {
		t.Fatalf("leave-shared applied but reported err=%v next=%+v state=%s", err, next, live.RemoteOwnershipState())
	}
	// RR-20260913-13:旧实现把 live 恢复成 shared,后续普通写第一次因刷新后非 shared 被拒,
	// 第二、三次卡在 `shared -> local_owned` 非法迁移上永远不恢复。连续三次准入都必须成功。
	for attempt := 1; attempt <= 3; attempt++ {
		if err := probeAdmission(t, mgr, live); err != nil {
			t.Fatalf("attempt %d: repeated admission did not recover after authoritative refresh: %v (live=%s)", attempt, err, live.RemoteOwnershipState())
		}
	}
}

// 对照:发送前失败、Lua 没执行——恢复原模式,继续可写。
func TestSharedModePromiseFailureBeforeSendRestoresPreviousMode(t *testing.T) {
	mgr, store, live := modeSetup(t, 254, 1504)
	ctx := context.Background()
	store.failBefore = true
	if _, err := mgr.EnterRemoteSharedMode(ctx, live.GUId()); err == nil {
		t.Fatal("must report the send failure")
	}
	if live.RemoteOwnershipState() != entity.RemoteOwnershipLocalOwned {
		t.Fatalf("proven no-op must restore local_owned, got %s", live.RemoteOwnershipState())
	}
	if err := probeAdmission(t, mgr, live); err != nil {
		t.Fatalf("before-send recovery failed: %v", err)
	}
	store.failBefore = false
	if _, err := mgr.EnterRemoteSharedMode(ctx, live.GUId()); err != nil {
		t.Fatal(err)
	}
	store.failBefore = true
	if _, err := mgr.LeaveRemoteSharedMode(ctx, live.GUId()); err == nil {
		t.Fatal("must report the send failure")
	}
	if live.RemoteOwnershipState() != entity.RemoteOwnershipShared {
		t.Fatalf("proven no-op must restore shared, got %s", live.RemoteOwnershipState())
	}
	if err := probeAdmission(t, mgr, live); err != nil {
		t.Fatalf("before-send recovery failed: %v", err)
	}
}

type lostReplyMarkerStore struct {
	*mockMarkerStore
	loseReply     atomic.Bool // 执行成功后返回 I/O 错误
	failBefore    atomic.Bool // 执行前就失败（对照：确定没执行）
	blackout      atomic.Bool // 之后的权威查询也不可用
	afterTransfer func()      // 执行成功后、返回前的钩子
}

func (s *lostReplyMarkerStore) TransferExpected(ctx context.Context, id int64, expected entity.RemoteEntityMarkerLease, owner int32) (entity.RemoteEntityMarkerLease, error) {
	if s.failBefore.Load() {
		return entity.RemoteEntityMarkerLease{}, errors.New("i/o timeout before send")
	}
	next, err := s.mockMarkerStore.TransferExpected(ctx, id, expected, owner)
	if err != nil {
		return next, err
	}
	if s.afterTransfer != nil {
		s.afterTransfer()
	}
	if s.loseReply.Load() {
		return entity.RemoteEntityMarkerLease{}, errors.New("i/o timeout: transfer reply lost")
	}
	return next, nil
}

func (s *lostReplyMarkerStore) GetOwnership(ctx context.Context, id int64) (entity.RemoteEntityMarkerLease, bool, error) {
	if s.blackout.Load() {
		return entity.RemoteEntityMarkerLease{}, false, errors.New("i/o timeout: marker store unavailable")
	}
	return s.mockMarkerStore.GetOwnership(ctx, id)
}

func indeterminateTransferSetup(t *testing.T, kind entity.EntityKind, seq int64) (*Manager, *lostReplyMarkerStore, *testRemoteEntity) {
	t.Helper()
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	cfg := DefaultConfig()
	cfg.MarkerCacheTTL = time.Minute // 放大热 marker 的信任窗口，让旧行为稳定暴露
	cfg.OpTimeout = 2 * time.Second
	mgr := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = mgr.StopFinalizer(ctx)
	})
	store := &lostReplyMarkerStore{mockMarkerStore: newMockMarkerStore()}
	mgr.SetOwnershipStore(store)
	loader := newRemoteTestLoader()
	mgr.SetBackend(loader)
	live := newTestRemoteEntity(seq, 1, kind)
	if err := live.SetRemoteVersionVector(entity.RemoteVersionVector{MarkerEpoch: 2, RouteEpoch: 3}); err != nil {
		t.Fatal(err)
	}
	loader.add(live)
	store.leases[live.GUId()] = entity.RemoteEntityMarkerLease{OwnerSid: 1000, MarkerEpoch: 2, RouteEpoch: 3}
	return mgr, store, live
}

func TestTransferPromiseLostReplyFencesOldOwnerWhenAuthorityConfirmsTransfer(t *testing.T) {
	mgr, store, live := indeterminateTransferSetup(t, 246, 1406)
	store.loseReply.Store(true)

	next, err := mgr.TransferRemoteOwnership(context.Background(), live.GUId(), 2000)
	authority := store.leases[live.GUId()]
	if authority.OwnerSid != 2000 {
		t.Fatalf("test premise: authority owner=%d, want 2000", authority.OwnerSid)
	}
	if batch, admitErr := mgr.PrepareRemoteWriteBatch(context.Background(), []int64{live.GUId()}); !errors.Is(admitErr, entity.ErrRemoteFenced) {
		if admitErr == nil {
			_ = batch.Abort(context.Background(), errors.New("probe"))
			_ = batch.Close(context.Background())
		}
		t.Fatalf("lost reply: Redis owner=%d live=%s old owner write admitted=%v err=%v",
			authority.OwnerSid, live.RemoteOwnershipState(), admitErr == nil, admitErr)
	}
	if state := live.RemoteOwnershipState(); state != entity.RemoteOwnershipFenced {
		t.Fatalf("lost reply: Redis owner=%d live=%s, want fenced", authority.OwnerSid, state)
	}
	// 权威已经确认转移成功：结果应当和一次正常成功的 Transfer 一样。
	if err != nil || next.OwnerSid != 2000 {
		t.Fatalf("transfer applied but reported err=%v next=%+v", err, next)
	}
}

func TestTransferPromiseLostReplyFreezesOldOwnerUntilAuthorityAnswers(t *testing.T) {
	mgr, store, live := indeterminateTransferSetup(t, 247, 1407)
	store.loseReply.Store(true)
	// 回复丢失后权威也查不到——这才是真正的“未知”。
	store.afterTransfer = func() { store.blackout.Store(true) }

	_, err := mgr.TransferRemoteOwnership(context.Background(), live.GUId(), 2000)
	if err == nil {
		t.Fatal("transfer with unknown outcome and unreachable authority must not report success")
	}
	if state := live.RemoteOwnershipState(); state == entity.RemoteOwnershipLocalOwned || state == entity.RemoteOwnershipShared {
		t.Fatalf("unknown outcome restored old owner: live=%s", state)
	}
	// 权威不可用期间不能放行。
	if _, err := mgr.PrepareRemoteWriteBatch(context.Background(), []int64{live.GUId()}); err == nil {
		t.Fatal("old owner write admitted while ownership is unknown and authority unreachable")
	}
	// 权威恢复后给出真相：已经转移，旧 owner 被 fence。
	store.blackout.Store(false)
	_, err = mgr.PrepareRemoteWriteBatch(context.Background(), []int64{live.GUId()})
	if !errors.Is(err, entity.ErrRemoteFenced) {
		t.Fatalf("after authority recovered: err=%v, want ErrRemoteFenced", err)
	}
}

// 对照：发送前就失败、Lua 没执行——权威仍是旧 owner，恢复本地所有权是正确的。
func TestTransferPromiseFailureBeforeSendRestoresOldOwner(t *testing.T) {
	mgr, store, live := indeterminateTransferSetup(t, 248, 1408)
	store.failBefore.Store(true)

	if _, err := mgr.TransferRemoteOwnership(context.Background(), live.GUId(), 2000); err == nil {
		t.Fatal("transfer must report the send failure")
	}
	if store.leases[live.GUId()].OwnerSid != 1000 {
		t.Fatal("test premise: authority must still name the old owner")
	}
	if state := live.RemoteOwnershipState(); state != entity.RemoteOwnershipLocalOwned {
		t.Fatalf("proven no-op must restore local ownership, got %s", state)
	}
	batch, err := mgr.PrepareRemoteWriteBatch(context.Background(), []int64{live.GUId()})
	if err != nil {
		t.Fatalf("old owner must keep writing after a proven no-op: %v", err)
	}
	_ = batch.Abort(context.Background(), errors.New("probe"))
	_ = batch.Close(context.Background())
}

func TestMarkerLeaseStaysExactDecimalAtLargeEpochs(t *testing.T) {
	ctx := context.Background()
	for _, epoch := range []uint64{1 << 46, 100_000_000_000_000, 1<<53 + 1} {
		stub := newMarkerEvalStub()
		store := newRedisMarkerForEval(stub, "marks")
		// A lease already at a large epoch, in the exact form Go writes.
		seed := entity.RemoteEntityMarkerLease{OwnerSid: 1001, MarkerEpoch: epoch, RouteEpoch: 1}
		stub.values["42"] = formatMarkerLease(seed)

		shared, err := store.EnterSharedExpected(ctx, 42, seed)
		if err != nil {
			t.Fatalf("epoch %d: enter shared = %v; the record was rewritten before this failed", epoch, err)
		}
		if shared.MarkerEpoch != epoch+1 {
			t.Fatalf("epoch %d: new epoch = %d, want %d", epoch, shared.MarkerEpoch, epoch+1)
		}
		// And the stored record must still be readable.
		got, ok, err := store.GetOwnership(ctx, 42)
		if err != nil || !ok {
			t.Fatalf("epoch %d: reading back the lease = ok:%v err:%v", epoch, ok, err)
		}
		if got.MarkerEpoch != epoch+1 || !got.Shared {
			t.Fatalf("epoch %d: read back %#v", epoch, got)
		}
	}
}

type keyedMarkerEval struct {
	mu   sync.Mutex
	keys map[string]*markerEvalStub
}

func newKeyedMarkerEval() *keyedMarkerEval {
	return &keyedMarkerEval{keys: map[string]*markerEvalStub{}}
}

func (k *keyedMarkerEval) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	k.mu.Lock()
	stub := k.keys[keys[0]]
	if stub == nil {
		stub = newMarkerEvalStub()
		k.keys[keys[0]] = stub
	}
	k.mu.Unlock()
	return stub.Eval(ctx, script, keys, args...)
}

func (k *keyedMarkerEval) touched() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	keys := make([]string, 0, len(k.keys))
	for key := range k.keys {
		keys = append(keys, key)
	}
	return keys
}

func TestRedisMarkerDefaultKeyIsUnchanged(t *testing.T) {
	redis := newKeyedMarkerEval()
	store, err := newRedisMarkerForEvalWithKeyPrefix(redis, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimOwnership(context.Background(), 7311, 1000); err != nil {
		t.Fatal(err)
	}
	// 旧版本的键逐字写出来，不经 markerKeyForPrefix，防止两边一起改。
	if got := redis.touched(); len(got) != 1 || got[0] != "remote_entity:marks" {
		t.Fatalf("default marker keys=%v, want exactly %q (unchanged, no migration)", got, "remote_entity:marks")
	}
	legacy := newRedisMarkerForEval(redis, "")
	if lease, found, err := legacy.GetOwnership(context.Background(), 7311); err != nil || !found || lease.OwnerSid != 1000 {
		t.Fatalf("marker without a prefix does not read the default marker's lease: found=%v lease=%+v err=%v", found, lease, err)
	}
}

func TestRedisMarkerKeyPrefixIsolatesDeploymentsSharingOneRedis(t *testing.T) {
	redis := newKeyedMarkerEval()
	ctx := context.Background()
	a, err := newRedisMarkerForEvalWithKeyPrefix(redis, "roost:game-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := newRedisMarkerForEvalWithKeyPrefix(redis, "roost:game-b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimOwnership(ctx, 7312, 1000); err != nil {
		t.Fatal(err)
	}
	// 另一个部署看不到这份租约：GetOwnership 找不到，自己的 Claim 也不会被当成别人的 owner 拒绝。
	if lease, found, err := b.GetOwnership(ctx, 7312); err != nil || found {
		t.Fatalf("deployment b reads deployment a's lease: found=%v lease=%+v err=%v", found, lease, err)
	}
	claimed, err := b.ClaimOwnership(ctx, 7312, 2000)
	if err != nil || claimed.OwnerSid != 2000 {
		t.Fatalf("deployment b claim = %+v, err=%v; want its own owner 2000", claimed, err)
	}
	if lease, found, err := a.GetOwnership(ctx, 7312); err != nil || !found || lease.OwnerSid != 1000 {
		t.Fatalf("deployment a's lease changed by deployment b: found=%v lease=%+v err=%v", found, lease, err)
	}
	for _, key := range redis.touched() {
		if key != "roost:game-a:remote_entity:marks" && key != "roost:game-b:remote_entity:marks" {
			t.Fatalf("marker touched key %q outside both prefixes", key)
		}
	}
	if got := len(redis.touched()); got != 2 {
		t.Fatalf("touched %d keys, want one hash per deployment: %v", got, redis.touched())
	}
}

func TestMarkerKeyPrefixValidation(t *testing.T) {
	for _, prefix := range []string{"", "roost:game-a", "{roost:game-a}", "roost:{game-a}:remote"} {
		if err := ValidateMarkerKeyPrefix(prefix); err != nil {
			t.Fatalf("prefix %q rejected: %v", prefix, err)
		}
	}
	// 标记只有一把 hash 键，hash tag 无害、允许；但写了花括号就必须是首个非空闭合 tag——
	// Redis 对 "e{" / "e{}" 不做 tag 哈希，运维会以为钉住了槽而没有（与 lock_key 的 Cluster 规则同一判断）。
	for _, prefix := range []string{" roost", "roost ", "roost game", "roost\tgame", "roost{", "roost{}", "roost{}{game}"} {
		if err := ValidateMarkerKeyPrefix(prefix); err == nil {
			t.Fatalf("prefix %q accepted", prefix)
		}
	}
	if _, err := newRedisMarkerForEvalWithKeyPrefix(newKeyedMarkerEval(), "roost{}"); err == nil || !strings.Contains(err.Error(), "hash tag") {
		t.Fatalf("constructor accepted an empty hash tag: %v", err)
	}
}

type markerEvalStub struct {
	mu     sync.Mutex
	values map[string]string
}

func newMarkerEvalStub() *markerEvalStub {
	return &markerEvalStub{values: make(map[string]string)}
}

func (s *markerEvalStub) Eval(_ context.Context, script string, _ []string, args ...any) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(args) == 0 {
		return nil, errors.New("missing marker field")
	}
	field := args[0].(string)
	switch script {
	case ownershipGetScript:
		return s.values[field], nil
	case ownershipClaimScript:
		if len(args) != 2 {
			return nil, errors.New("invalid claim args")
		}
		owner := args[1].(string)
		if current := s.values[field]; current != "" {
			_, lease, err := parseMarkerLease(current)
			if err == nil && lease.OwnerSid == mustParseMarkerOwner(owner) {
				return current, nil
			}
			return "", nil
		}
		value := "local:" + owner + ":1:1"
		s.values[field] = value
		return value, nil
	case ownershipEnterSharedScript:
		if len(args) != 2 || s.values[field] != args[1].(string) {
			return "", nil
		}
		_, lease, err := parseMarkerLease(s.values[field])
		if err != nil || lease.Shared {
			return "", nil
		}
		lease.Shared = true
		value := markerLeaseAsLuaWould(lease, incMarkerEpochAsLuaWould(lease.MarkerEpoch))
		if value == "" {
			return "", nil
		}
		s.values[field] = value
		return value, nil
	case ownershipLeaveSharedScript:
		if len(args) != 2 || s.values[field] != args[1].(string) {
			return "", nil
		}
		_, lease, err := parseMarkerLease(s.values[field])
		if err != nil || !lease.Shared {
			return "", nil
		}
		lease.Shared = false
		value := markerLeaseAsLuaWould(lease, incMarkerEpochAsLuaWould(lease.MarkerEpoch))
		if value == "" {
			return "", nil
		}
		s.values[field] = value
		return value, nil
	default:
		return nil, errors.New("unexpected script")
	}
}

// markerLeaseAsLuaWould models how the script builds the new lease string: the
// script increments the decimal digits itself (a Lua number would render 10^14
// as "1e+14") and refuses anything past uint64, so this double does the same;
// an empty result models that refusal. 历史：RR-20260913-11。
func markerLeaseAsLuaWould(lease entity.RemoteEntityMarkerLease, nextEpoch uint64) string {
	if nextEpoch == 0 {
		return ""
	}
	lease.MarkerEpoch = nextEpoch
	return formatMarkerLease(lease)
}

// incMarkerEpochAsLuaWould is the script's incdec: exact, and nil past uint64.
func incMarkerEpochAsLuaWould(value uint64) uint64 {
	if value == ^uint64(0) {
		return 0
	}
	return value + 1
}

func mustParseMarkerOwner(raw string) int32 {
	_, lease, _ := parseMarkerLease("local:" + raw + ":1:1")
	return lease.OwnerSid
}

func TestParseMarkerLeasePreservesModeAndFence(t *testing.T) {
	shared, lease, err := parseMarkerLease("shared:1001:9:4")
	if err != nil || !shared || !lease.Shared || lease.OwnerSid != 1001 || lease.MarkerEpoch != 9 || lease.RouteEpoch != 4 {
		t.Fatalf("shared parse = %v %#v %v", shared, lease, err)
	}
	shared, lease, err = parseMarkerLease("local:1001:10:4")
	if err != nil || shared || lease.Shared || lease.OwnerSid != 1001 || lease.MarkerEpoch != 10 || lease.RouteEpoch != 4 {
		t.Fatalf("local parse = %v %#v %v", shared, lease, err)
	}
}

func TestRedisOwnershipClaimAndEnterSharedRejectStaleLease(t *testing.T) {
	redis := newMarkerEvalStub()
	store := newRedisMarkerForEval(redis, "marks")
	expected, err := store.ClaimOwnership(context.Background(), 42, 1001)
	if err != nil || expected != (entity.RemoteEntityMarkerLease{OwnerSid: 1001, MarkerEpoch: 1, RouteEpoch: 1}) {
		t.Fatalf("claim = %#v, err=%v", expected, err)
	}
	if _, err = store.ClaimOwnership(context.Background(), 42, 2002); err == nil {
		t.Fatal("a second owner must not claim an existing lease")
	}

	first, err := store.EnterSharedExpected(context.Background(), 42, expected)
	if err != nil || !first.Shared || first.MarkerEpoch != 2 || first.OwnerSid != 1001 || first.RouteEpoch != 1 {
		t.Fatalf("enter shared = %#v, err=%v", first, err)
	}
	if _, err = store.EnterSharedExpected(context.Background(), 42, expected); err == nil {
		t.Fatal("stale expected lease must not overwrite an existing shared lease")
	}
	observed, found, err := store.GetOwnership(context.Background(), 42)
	if err != nil || !found || observed != first {
		t.Fatalf("ownership after stale transition found=%v lease=%#v err=%v", found, observed, err)
	}
}

// countingEval records how many Lua transitions reached Redis so a refusal
// that must be decided locally can prove it never touched the store.
type countingEval struct {
	inner *markerEvalStub
	calls int
}

func (c *countingEval) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	c.calls++
	return c.inner.Eval(ctx, script, keys, args...)
}

func expectMarkerErr(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to contain %q", err, want)
	}
}

// ownership marker 的本地参数检查与 CAS 冲突翻译各按自己的原因拒绝；本地检查不花一次 Redis 往返。
func TestRedisMarkerRefusesEachInvalidTransitionArgument(t *testing.T) {
	eval := &countingEval{inner: newMarkerEvalStub()}
	store := newRedisMarkerForEval(eval, "marks")
	ctx := context.Background()

	_, err := store.ClaimOwnership(ctx, 0, 1001)
	expectMarkerErr(t, err, "invalid ownership claim for 0")
	_, err = store.ClaimOwnership(ctx, 42, 0)
	expectMarkerErr(t, err, "invalid ownership claim for 42")
	_, err = store.EnterSharedExpected(ctx, 42, entity.RemoteEntityMarkerLease{OwnerSid: 1001, MarkerEpoch: 1, RouteEpoch: 1, Shared: true})
	expectMarkerErr(t, err, "expected local lease for 42")
	_, err = store.LeaveSharedExpected(ctx, 42, entity.RemoteEntityMarkerLease{OwnerSid: 1001, MarkerEpoch: 2, RouteEpoch: 1})
	expectMarkerErr(t, err, "invalid shared lease for 42")
	_, err = store.LeaveSharedExpected(ctx, 42, entity.RemoteEntityMarkerLease{OwnerSid: 1001, MarkerEpoch: 0, RouteEpoch: 1, Shared: true})
	expectMarkerErr(t, err, "invalid shared lease for 42")
	if eval.calls != 0 {
		t.Fatalf("locally refused transitions reached Redis %d times", eval.calls)
	}

	lease, err := store.ClaimOwnership(ctx, 42, 1001)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.ClaimOwnership(ctx, 42, 2002)
	expectMarkerErr(t, err, "ownership claim conflict for 42")
	stale := lease
	stale.MarkerEpoch++
	_, err = store.EnterSharedExpected(ctx, 42, stale)
	expectMarkerErr(t, err, "enter shared compare-and-swap failed for 42")
	shared, err := store.EnterSharedExpected(ctx, 42, lease)
	if err != nil || !shared.Shared {
		t.Fatalf("enter shared = %#v, %v", shared, err)
	}
	_, err = store.LeaveSharedExpected(ctx, 42, entity.RemoteEntityMarkerLease{OwnerSid: 1001, MarkerEpoch: shared.MarkerEpoch + 1, RouteEpoch: 1, Shared: true})
	if err == nil {
		t.Fatal("leave shared with a stale lease must fail")
	}
	observed, found, err := store.GetOwnership(ctx, 42)
	if err != nil || !found || observed != shared {
		t.Fatalf("ownership after refused transitions = %#v %v %v", observed, found, err)
	}
}

type plainStorageBackend struct{}

func (plainStorageBackend) CommitRemote(context.Context, entity.RemoteCommit) (entity.RemoteCommitReceipt, error) {
	return entity.RemoteCommitReceipt{}, nil
}
func (plainStorageBackend) CommitStatus(context.Context, entity.RemoteTransactionID) (entity.RemoteCommitStatus, error) {
	return entity.RemoteCommitStatus{}, nil
}
func (plainStorageBackend) CommitRemoteBatch(context.Context, []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error) {
	return nil, nil
}
func (plainStorageBackend) LoadRemoteSnapshot(context.Context, entity.RemoteSnapshotKey, entity.RemoteReadConsistency, uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
	return entity.RemoteSnapshotEnvelope{}, false, nil
}
func (plainStorageBackend) PendingRemoteCommits(context.Context, int) ([]entity.RemoteCommitStatus, error) {
	return nil, nil
}
func (plainStorageBackend) MarkRemoteCommitPublished(context.Context, entity.RemoteTransactionID) error {
	return nil
}
func (plainStorageBackend) EnsureRemoteStorage(context.Context) error { return nil }

type nopLoader struct{}

func (nopLoader) LoadRemoteEntity(context.Context, int64, entity.EntityKind) (entity.IThreadSafeRemoteEntity, error) {
	return nil, nil
}
