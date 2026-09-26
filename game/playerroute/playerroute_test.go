package playerroute

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

// fakeKeyspace is the four Redis operations the store uses, with real SetNX
// semantics (insert-only) and real expiry, because those two are what the
// ownership rules rest on. Everything else about Redis is irrelevant here.
type fakeKeyspace struct {
	mu     sync.Mutex
	values map[string]string
	expiry map[string]time.Time
	now    time.Time
	fail   error
	// afterGet runs with the lock held, right after a Get has read a value
	// and before the caller can act on it.
	afterGet func(f *fakeKeyspace, key string)
	// beforeCompare runs with the lock held, immediately before a
	// compare-and-X evaluates. It is the moment the lease lapses and another
	// process takes it — everything the caller believed a moment ago is now
	// stale, and the operation has to notice by itself.
	beforeCompare func(f *fakeKeyspace, key string)
}

func (f *fakeKeyspace) runBeforeCompareLocked(key string) {
	if f.beforeCompare == nil {
		return
	}
	hook := f.beforeCompare
	f.beforeCompare = nil
	hook(f, key)
}

// setLocked is what a hook uses to hand the key to somebody else.
func (f *fakeKeyspace) setLocked(key, value string, ttl time.Duration) {
	f.values[key] = value
	f.expiry[key] = f.now.Add(ttl)
}

func newFakeKeyspace() *fakeKeyspace {
	return &fakeKeyspace{values: map[string]string{}, expiry: map[string]time.Time{}, now: time.Unix(1_700_000_000, 0)}
}

func (f *fakeKeyspace) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// liveLocked drops a key whose lease has run out, which is how a dead
// process stops owning its players.
func (f *fakeKeyspace) liveLocked(key string) bool {
	deadline, ok := f.expiry[key]
	if ok && !f.now.Before(deadline) {
		delete(f.values, key)
		delete(f.expiry, key)
		return false
	}
	_, present := f.values[key]
	return present
}

func (f *fakeKeyspace) SetNX(_ context.Context, key string, value any, expiration time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return false, f.fail
	}
	if f.liveLocked(key) {
		return false, nil
	}
	f.values[key] = value.(string)
	f.expiry[key] = f.now.Add(expiration)
	return true, nil
}

func (f *fakeKeyspace) Get(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return nil, f.fail
	}
	if !f.liveLocked(key) {
		return nil, fredis.ErrNil
	}
	value := f.values[key]
	if f.afterGet != nil {
		hook := f.afterGet
		f.afterGet = nil
		hook(f, key)
	}
	return []byte(value), nil
}

// CompareAndExpire and CompareAndDelete mirror core's Lua: read, compare the
// whole value, act only on a match — all without releasing the lock, which is
// what "one round trip" means here. The fake must not be more forgiving than
// the script, or the race these tests exist for becomes invisible.
func (f *fakeKeyspace) CompareAndExpire(_ context.Context, key, expect string, expiration time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return false, f.fail
	}
	f.runBeforeCompareLocked(key)
	if !f.liveLocked(key) || f.values[key] != expect {
		return false, nil
	}
	f.expiry[key] = f.now.Add(expiration)
	return true, nil
}

func (f *fakeKeyspace) CompareAndDelete(_ context.Context, key, expect string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return false, f.fail
	}
	f.runBeforeCompareLocked(key)
	if !f.liveLocked(key) || f.values[key] != expect {
		return false, nil
	}
	delete(f.values, key)
	delete(f.expiry, key)
	return true, nil
}

func (f *fakeKeyspace) deadline(t *testing.T, key string) time.Time {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	deadline, ok := f.expiry[key]
	if !ok {
		t.Fatalf("no lease on %q", key)
	}
	return deadline
}

// newTestStore gives every store its own token, the way two processes have
// their own. newTestStoreAs pins the token, for the restart cases where a
// process comes back on the same sid and must NOT be mistaken for itself.
func newTestStore(t *testing.T, shared *fakeKeyspace, sid int32) *Store {
	t.Helper()
	return newTestStoreAs(t, shared, sid, fmt.Sprintf("token-%d-%d", sid, testTokenSeq()))
}

func newTestStoreAs(t *testing.T, shared *fakeKeyspace, sid int32, token string) *Store {
	t.Helper()
	store, err := newStore(shared, "game:route", sid, token)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

var testTokens atomic.Int64

func testTokenSeq() int64 { return testTokens.Add(1) }

// The rule the whole package exists for: one owner at a time, and the second
// process to ask is told who owns the player rather than being told it does.
func TestClaimGivesOnePlayerOneOwner(t *testing.T) {
	shared := newFakeKeyspace()
	first, second := newTestStore(t, shared, 1000), newTestStore(t, shared, 1001)
	ctx := context.Background()

	route, err := first.Claim(ctx, 42)
	if err != nil || route.SID != 1000 {
		t.Fatalf("first claim = %+v, %v; want sid 1000", route, err)
	}
	route, err = second.Claim(ctx, 42)
	if err != nil || route.SID != 1000 {
		t.Fatalf("second process's claim = %+v, %v; want the first owner, sid 1000", route, err)
	}
	if owns, err := second.Owns(ctx, 42); err != nil || owns {
		t.Fatalf("the process that lost the claim reports ownership: %v, %v", owns, err)
	}
	if owns, err := first.Owns(ctx, 42); err != nil || !owns {
		t.Fatalf("the owner does not report ownership: %v, %v", owns, err)
	}
}

// A claim this process already holds is extended, not refused: a live player
// must not lose their owner because the owner asked again.
func TestClaimByTheOwnerExtendsTheLease(t *testing.T) {
	shared := newFakeKeyspace()
	store := newTestStore(t, shared, 1000)
	ctx := context.Background()
	if _, err := store.Claim(ctx, 42); err != nil {
		t.Fatal(err)
	}
	before := shared.deadline(t, "game:route:owner:42")
	shared.advance(Lease / 3)
	if _, err := store.Claim(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if after := shared.deadline(t, "game:route:owner:42"); !after.After(before) {
		t.Fatalf("re-claiming by the owner did not extend the lease: %v then %v", before, after)
	}
}

// What makes a dead process stop owning players, and the reason Refresh
// exists: without it a live player's claim runs out too.
func TestLeaseLapsesWithoutRefreshAndRefreshOnlyExtendsOurOwn(t *testing.T) {
	shared := newFakeKeyspace()
	owner, other := newTestStore(t, shared, 1000), newTestStore(t, shared, 1001)
	ctx := context.Background()
	if _, err := owner.Claim(ctx, 42); err != nil {
		t.Fatal(err)
	}

	// Another process refreshing our key must not extend it: a stale refresh
	// from a process the player has left would keep the wrong owner alive.
	shared.advance(Lease / 2)
	before := shared.deadline(t, "game:route:owner:42")
	other.Refresh(ctx, []int64{42})
	if after := shared.deadline(t, "game:route:owner:42"); !after.Equal(before) {
		t.Fatalf("a non-owner's Refresh moved the lease: %v then %v", before, after)
	}

	owner.Refresh(ctx, []int64{42})
	if after := shared.deadline(t, "game:route:owner:42"); !after.After(before) {
		t.Fatal("the owner's Refresh did not extend the lease")
	}

	// Nobody refreshes: the process is gone, and the player becomes claimable.
	shared.advance(Lease + time.Second)
	if owns, err := owner.Owns(ctx, 42); err != nil || owns {
		t.Fatalf("a lapsed lease still reports an owner: %v, %v", owns, err)
	}
	route, err := other.Claim(ctx, 42)
	if err != nil || route.SID != 1001 {
		t.Fatalf("after the lease lapsed the next process could not take the player: %+v, %v", route, err)
	}
}

// Release is the owner's statement, and only the owner's.
func TestReleaseOnlyFreesOurOwnClaim(t *testing.T) {
	shared := newFakeKeyspace()
	owner, other := newTestStore(t, shared, 1000), newTestStore(t, shared, 1001)
	ctx := context.Background()
	if _, err := owner.Claim(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if err := other.Release(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if owns, err := owner.Owns(ctx, 42); err != nil || !owns {
		t.Fatalf("a stale release from another process freed the owner's claim: %v, %v", owns, err)
	}
	if err := owner.Release(ctx, 42); err != nil {
		t.Fatal(err)
	}
	route, found, err := owner.GetRoute(ctx, 42)
	if err != nil || found || route.SID != 0 {
		t.Fatalf("after the owner released, GetRoute = %+v, %v, %v; want nobody", route, found, err)
	}
}

// GetRoute must report an unowned player as NOT FOUND. Answering "me" would
// make every process the owner of every idle player, which is the corruption
// the package exists to prevent.
func TestGetRouteReportsAnUnownedPlayerAsNotFound(t *testing.T) {
	shared := newFakeKeyspace()
	store := newTestStore(t, shared, 1000)
	ctx := context.Background()
	route, found, err := store.GetRoute(ctx, 7)
	if err != nil || found || route.SID != 0 {
		t.Fatalf("GetRoute of an unowned player = %+v, %v, %v; want not found", route, found, err)
	}
	if _, err := store.Claim(ctx, 7); err != nil {
		t.Fatal(err)
	}
	route, found, err = store.GetRoute(ctx, 7)
	if err != nil || !found || route.OwnerRouteSid() != 1000 {
		t.Fatalf("GetRoute of an owned player = %+v, %v, %v", route, found, err)
	}
}

func TestNewStoreRefusesWhatItCannotAddress(t *testing.T) {
	shared := newFakeKeyspace()
	if _, err := newStore(nil, "game:route", 1000, "t"); err == nil {
		t.Fatal("a store without a client was accepted")
	}
	if _, err := newStore(shared, "", 1000, "t"); err == nil {
		t.Fatal("a store without a key prefix was accepted")
	}
	if _, err := newStore(shared, "game:route", 0, "t"); err == nil {
		t.Fatal("a store without a sid was accepted")
	}
	// A store with no token cannot tell itself from its own previous
	// incarnation, which is the whole point of having one.
	if _, err := newStore(shared, "game:route", 1000, ""); err == nil {
		t.Fatal("a store without an owner token was accepted")
	}
	store := newTestStore(t, shared, 1000)
	if _, err := store.Claim(context.Background(), 0); err == nil {
		t.Fatal("a claim on player 0 was accepted")
	}
}

// RR-20260920-03：租约的"只动自己的"必须是一个原子操作。
//
// Claim / Refresh / Release 都是先 GET 确认 SID、再发第二条命令。两条命令之间
// 租约可以过期并被另一个进程取得，于是旧 owner 给新 owner 续了期，或者直接删掉
// 了新 owner 的 key——注释里写的 "only if it is ours" 不是实现提供的保证。
//
// 下面两条把那个交错钉死：hook 在 GET 读到旧值之后、调用方动作之前，把 key 交给
// 新 owner。不依赖时间调度。

func TestAStaleReleaseMustNotDeleteTheNewOwner(t *testing.T) {
	shared := newFakeKeyspace()
	old, next := newTestStore(t, shared, 1000), newTestStore(t, shared, 1001)
	ctx := context.Background()
	if _, err := old.Claim(ctx, 42); err != nil {
		t.Fatal(err)
	}
	// The lease lapses and 1001 takes it, in the instant before the old
	// owner's release reaches Redis.
	shared.beforeCompare = func(f *fakeKeyspace, key string) {
		f.setLocked(key, next.self().encode(), Lease)
	}
	if err := old.Release(ctx, 42); err != nil {
		t.Fatal(err)
	}
	route, found, err := next.GetRoute(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if !found || route.SID != 1001 {
		t.Fatalf("a release from the previous owner deleted the new owner's lease: route=%+v found=%v", route, found)
	}
}

func TestAStaleRefreshMustNotExtendTheNewOwner(t *testing.T) {
	shared := newFakeKeyspace()
	old, next := newTestStore(t, shared, 1000), newTestStore(t, shared, 1001)
	ctx := context.Background()
	if _, err := old.Claim(ctx, 42); err != nil {
		t.Fatal(err)
	}
	shared.beforeCompare = func(f *fakeKeyspace, key string) {
		// The new owner takes it with a short remainder on purpose: if the
		// stale refresh extends it, the deadline moves and this is visible.
		f.setLocked(key, next.self().encode(), Lease/3)
	}
	old.Refresh(ctx, []int64{42})
	deadline := shared.deadline(t, "game:route:owner:42")
	if want := shared.now.Add(Lease / 3); !deadline.Equal(want) {
		t.Fatalf("the previous owner extended a lease that is no longer theirs: deadline=%v want=%v", deadline, want)
	}
	if owns, err := next.Owns(ctx, 42); err != nil || !owns {
		t.Fatalf("the new owner lost the lease: %v, %v", owns, err)
	}
}

// 同一个 sid 重启必须换 token，否则新旧实例分不开。
//
// 这是 RR-20260920-03 里最容易被漏掉的一半：进程崩了又起来，sid 没变，但它不是
// 之前那个进程——之前那个也许还活着。新实例不能续期、也不能释放旧实例留下的租约；
// 它只能等租约自然到期，然后重新 Claim。
func TestARestartOnTheSameSidIsADifferentOwner(t *testing.T) {
	shared := newFakeKeyspace()
	ctx := context.Background()
	before := newTestStoreAs(t, shared, 1000, "incarnation-1")
	if _, err := before.Claim(ctx, 42); err != nil {
		t.Fatal(err)
	}

	after := newTestStoreAs(t, shared, 1000, "incarnation-2")
	if owns, err := after.Owns(ctx, 42); err != nil || owns {
		t.Fatalf("the restarted process claims to own a lease it never took: %v, %v", owns, err)
	}

	// It must not extend the lease its predecessor holds.
	deadline := shared.deadline(t, "game:route:owner:42")
	shared.advance(Lease / 3)
	results := after.Refresh(ctx, []int64{42})
	if len(results) != 1 || results[0].Held {
		t.Fatalf("the restarted process renewed its predecessor's lease: %+v", results)
	}
	if got := shared.deadline(t, "game:route:owner:42"); !got.Equal(deadline) {
		t.Fatalf("the lease deadline moved: %v then %v", deadline, got)
	}

	// Nor release it.
	if err := after.Release(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if owns, err := before.Owns(ctx, 42); err != nil || !owns {
		t.Fatalf("the restarted process released its predecessor's lease: %v, %v", owns, err)
	}

	// Once the lease lapses it can take the player properly.
	shared.advance(Lease + time.Second)
	route, err := after.Claim(ctx, 42)
	if err != nil || route != after.self() {
		t.Fatalf("after the lease lapsed the restarted process could not claim: %+v, %v", route, err)
	}
	if owns, err := before.Owns(ctx, 42); err != nil || owns {
		t.Fatalf("the previous incarnation still reports ownership: %v, %v", owns, err)
	}
}

// Refresh 的结果必须能区分"确认还是我的"和"不知道"。RR-20260920-04 的修复建立在
// 这个返回值上：读不出来时不能当成还持有。
func TestRefreshReportsPerPlayerOutcomesIncludingUnknown(t *testing.T) {
	shared := newFakeKeyspace()
	ctx := context.Background()
	store := newTestStore(t, shared, 1000)
	if _, err := store.Claim(ctx, 42); err != nil {
		t.Fatal(err)
	}
	results := store.Refresh(ctx, []int64{42, 99})
	if len(results) != 2 {
		t.Fatalf("results = %d, want one per player", len(results))
	}
	if !results[0].Held || results[0].Err != nil {
		t.Fatalf("the owner's own lease was not confirmed: %+v", results[0])
	}
	if results[1].Held {
		t.Fatalf("a player this process never claimed came back held: %+v", results[1])
	}

	shared.fail = errors.New("redis down")
	results = store.Refresh(ctx, []int64{42})
	if results[0].Held || results[0].Err == nil {
		t.Fatalf("an unreadable renewal must be UNKNOWN, not held: %+v", results[0])
	}
}
