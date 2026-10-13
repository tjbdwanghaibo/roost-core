package global

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/servicemetrics"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func newService(t *testing.T, mutate ...func(*Config)) (*Service, *clock) {
	t.Helper()
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	cfg := Config{
		Routes: versionstore.NewMemoryStore[int32, RouteBinding](),
		Now:    c.Now,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	service, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return service, c
}

func bind(t *testing.T, s *Service, gameSID int32, globalSID int32) RouteBinding {
	t.Helper()
	binding, err := s.Bind(context.Background(), gameSID, "group-a", globalSID)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

// Binding is insert-only: a game server that is already bound must go through
// migration, which requires the current epoch.
func TestBindIsInsertOnly(t *testing.T) {
	service, _ := newService(t)
	ctx := context.Background()

	binding := bind(t, service, 100, 1)
	if binding.Epoch != 1 || binding.State != RouteActive {
		t.Fatalf("first binding = %+v", binding)
	}
	if _, err := service.Bind(ctx, 100, "group-b", 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("rebinding through Bind returned %v, want ErrConflict", err)
	}
	// The original binding is untouched.
	resolved, err := service.Resolve(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.GlobalGroupID != "group-a" || resolved.GlobalSID != 1 || resolved.Epoch != 1 {
		t.Fatalf("the refused bind changed the binding: %+v", resolved)
	}
	for _, testCase := range []struct {
		label   string
		gameSID int32
		group   string
		global  int32
	}{
		{"zero game sid", 0, "group-a", 1},
		{"empty group", 101, "  ", 1},
		{"zero global sid", 102, "group-a", 0},
	} {
		if _, err := service.Bind(ctx, testCase.gameSID, testCase.group, testCase.global); !errors.Is(err, ErrRouteInvalid) {
			t.Fatalf("%s: %v", testCase.label, err)
		}
	}
	if _, err := service.Resolve(ctx, 999); !errors.Is(err, ErrRouteMissing) {
		t.Fatal("resolving an unbound game did not report not-found")
	}
}

// Migration is epoch-gated. The boundary document of the implementation this
// replaces required epoch CAS and forbade an in-process lock as the
// cross-instance guarantee; the implementation used the lock.
func TestMigrationRequiresTheCurrentEpoch(t *testing.T) {
	service, _ := newService(t)
	ctx := context.Background()
	binding := bind(t, service, 100, 1)

	// A stale epoch is refused.
	if _, err := service.BeginMigration(ctx, 100, 2, binding.Epoch+5); !errors.Is(err, ErrRouteStale) {
		t.Fatalf("a stale epoch was accepted: %v", err)
	}
	migrating, err := service.BeginMigration(ctx, 100, 2, binding.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	if migrating.State != RouteMigrating || migrating.TargetGlobalSID != 2 {
		t.Fatalf("migrating binding = %+v", migrating)
	}
	if migrating.Epoch != binding.Epoch+1 {
		t.Fatalf("epoch = %d, want %d: every accepted change must move the epoch", migrating.Epoch, binding.Epoch+1)
	}
	// The epoch the first caller used is now stale, so a second migration
	// with it loses — this is the property that makes the lock unnecessary.
	if _, err := service.BeginMigration(ctx, 100, 3, binding.Epoch); !errors.Is(err, ErrRouteStale) {
		t.Fatalf("a second migration reused the old epoch: %v", err)
	}
	// Even with the right epoch, a binding already migrating is refused.
	if _, err := service.BeginMigration(ctx, 100, 3, migrating.Epoch); !errors.Is(err, ErrRouteMigrating) {
		t.Fatalf("a concurrent migration was accepted: %v", err)
	}
	completed, err := service.CompleteMigration(ctx, 100, migrating.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	if completed.GlobalSID != 2 || completed.State != RouteActive || completed.TargetGlobalSID != 0 {
		t.Fatalf("completed binding = %+v", completed)
	}
	if completed.Epoch != migrating.Epoch+1 {
		t.Fatalf("completion did not move the epoch: %d", completed.Epoch)
	}
}

// A migration can be abandoned, returning the binding to its current
// instance without losing the epoch discipline.
func TestMigrationCanBeAborted(t *testing.T) {
	service, _ := newService(t)
	ctx := context.Background()
	binding := bind(t, service, 100, 1)
	migrating, err := service.BeginMigration(ctx, 100, 2, binding.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.AbortMigration(ctx, 100, binding.Epoch); !errors.Is(err, ErrRouteStale) {
		t.Fatal("abort accepted a stale epoch")
	}
	aborted, err := service.AbortMigration(ctx, 100, migrating.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	if aborted.State != RouteActive || aborted.GlobalSID != 1 || aborted.TargetGlobalSID != 0 {
		t.Fatalf("aborted binding = %+v", aborted)
	}
	// A retried completion on a binding that is no longer migrating is a
	// no-op rather than a failure, and cannot resurrect the target.
	settled, err := service.CompleteMigration(ctx, 100, aborted.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	if settled.GlobalSID != 1 {
		t.Fatalf("a retried completion moved an aborted binding: %+v", settled)
	}
}

// Concurrent migrations on one binding must produce exactly one winner.
func TestConcurrentMigrationsHaveOneWinner(t *testing.T) {
	service, _ := newService(t)
	ctx := context.Background()
	binding := bind(t, service, 100, 1)

	const racers = 12
	var wait sync.WaitGroup
	var mu sync.Mutex
	won := 0
	stale := 0
	for i := 0; i < racers; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, err := service.BeginMigration(ctx, 100, int32(index+2), binding.Epoch)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won++
			case errors.Is(err, ErrRouteStale), errors.Is(err, ErrRouteMigrating):
				stale++
			default:
				t.Errorf("unexpected error %v", err)
			}
		}(i)
	}
	wait.Wait()
	if won != 1 {
		t.Fatalf("%d migrations were accepted, want exactly 1", won)
	}
	if stale != racers-1 {
		t.Fatalf("%d were refused, want %d", stale, racers-1)
	}
	resolved, err := service.Resolve(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.State != RouteMigrating {
		t.Fatalf("binding = %+v", resolved)
	}
}

func TestNewRejectsAnIncompleteConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("a missing route store was accepted")
	}
}

// The refusal this package exists to make — a stale route epoch — was
// unobservable in the implementation it replaces. Making them is only half the
// job; an operator has to be able to see that they are happening, so the
// reports are asserted rather than assumed.
func TestRefusalsAndAcceptancesAreReported(t *testing.T) {
	sink := servicemetrics.NewRecorder()
	service, _ := newService(t, func(cfg *Config) { cfg.Metrics = sink })
	ctx := context.Background()

	binding := bind(t, service, 7, 100)
	if got := sink.Count("accepted:bind"); got != 1 {
		t.Fatalf("bind reported %d accepts; %s", got, sink.Events())
	}

	// A rebind presenting an epoch that is no longer current: the CAS the
	// boundary document required and an in-process lock cannot provide.
	if _, err := service.BeginMigration(ctx, 7, 200, binding.Epoch+9); !errors.Is(err, ErrRouteStale) {
		t.Fatalf("a stale epoch was accepted: %v", err)
	}
	if got := sink.Count("refused:begin_migration:stale_epoch"); got != 1 {
		t.Fatalf("a stale epoch reported %d refusals; %s", got, sink.Events())
	}

	if _, err := service.BeginMigration(ctx, 7, 200, binding.Epoch); err != nil {
		t.Fatal(err)
	}
	if got := sink.Count("accepted:begin_migration"); got != 1 {
		t.Fatalf("a migration reported %d accepts; %s", got, sink.Events())
	}

	done, err := service.CompleteMigration(ctx, 7, binding.Epoch+1)
	if err != nil {
		t.Fatal(err)
	}
	if got := sink.Count("accepted:complete_migration"); got != 1 {
		t.Fatalf("a completed migration reported %d accepts; %s", got, sink.Events())
	}

	// A second Bind for a game that is already bound loses the Create.
	if _, err := service.Bind(ctx, 7, "group-a", 300); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second bind was accepted: %v", err)
	}
	if got := sink.Count("conflict:bind"); got != 1 {
		t.Fatalf("a refused bind reported %d conflicts; %s", got, sink.Events())
	}
	if done.GlobalSID != 200 {
		t.Fatalf("binding after migration = %+v", done)
	}
}

// A nil reporter must never change behaviour.
func TestANilReporterChangesNothing(t *testing.T) {
	service, _ := newService(t)
	ctx := context.Background()
	binding := bind(t, service, 7, 100)
	moving, err := service.BeginMigration(ctx, 7, 200, binding.Epoch)
	if err != nil {
		t.Fatalf("a service with no reporter failed to begin a migration: %v", err)
	}
	if _, err := service.CompleteMigration(ctx, 7, moving.Epoch); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Bind(ctx, 7, "group-a", 300); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second bind with no reporter = %v, want ErrConflict", err)
	}
}

// Bind 结果未知（写已落库、回复丢了）后用同样的参数重试，得到同一个绑定而不是 ErrConflict；只有已存的 group / globalSID 与请求
// 不一致才是真正的冲突。
// 历史：RR-20261006-05。

// lostReplyStore 让下一次 Create 照常落库、但把回复换成传输错误，模拟结果未知。
type lostReplyStore struct {
	versionstore.Store[int32, RouteBinding]
	loseNext bool
}

var errReplyLost = errors.New("connection reset after write")

func (s *lostReplyStore) Create(ctx context.Context, key int32, value RouteBinding) (versionstore.Versioned[RouteBinding], bool, error) {
	stored, created, err := s.Store.Create(ctx, key, value)
	if s.loseNext && err == nil {
		s.loseNext = false
		return versionstore.Versioned[RouteBinding]{}, false, errReplyLost
	}
	return stored, created, err
}

func TestBindRetriedAfterUnknownOutcomeReturnsTheSameBinding(t *testing.T) {
	sink := servicemetrics.NewRecorder()
	store := &lostReplyStore{Store: versionstore.NewMemoryStore[int32, RouteBinding](), loseNext: true}
	service, _ := newService(t, func(cfg *Config) {
		cfg.Routes = store
		cfg.Metrics = sink
	})
	ctx := context.Background()

	if _, err := service.Bind(ctx, 7, "group-a", 100); !errors.Is(err, errReplyLost) {
		t.Fatalf("first bind: err=%v, want the lost reply", err)
	}
	again, err := service.Bind(ctx, 7, "group-a", 100)
	if err != nil {
		t.Fatalf("retrying the same bind after an unknown outcome: %v, want the stored binding", err)
	}
	if again.GameSID != 7 || again.GlobalGroupID != "group-a" || again.GlobalSID != 100 || again.Epoch != 1 || again.State != RouteActive {
		t.Fatalf("retried bind returned %+v", again)
	}
	if got := sink.Count("replayed:bind"); got != 1 {
		t.Fatalf("a retried bind reported %d replays; %s", got, sink.Events())
	}
	if got := sink.Count("conflict:bind"); got != 0 {
		t.Fatalf("a retried bind reported %d conflicts; %s", got, sink.Events())
	}

	// 已存的绑定与请求不一致仍是冲突：换 group、换 globalSID 都不算重试。
	for _, tc := range []struct {
		group  string
		global int32
	}{{"group-b", 100}, {"group-a", 200}} {
		if _, err := service.Bind(ctx, 7, tc.group, tc.global); !errors.Is(err, ErrConflict) {
			t.Fatalf("bind %s/%d over group-a/100: err=%v, want ErrConflict", tc.group, tc.global, err)
		}
	}
	if got := sink.Count("conflict:bind"); got != 2 {
		t.Fatalf("mismatched binds reported %d conflicts; %s", got, sink.Events())
	}
	resolved, err := service.Resolve(ctx, 7)
	if err != nil || resolved.GlobalGroupID != "group-a" || resolved.GlobalSID != 100 || resolved.Epoch != 1 {
		t.Fatalf("binding after refused binds: %+v err=%v", resolved, err)
	}
}

func expectGuard(t *testing.T, err error, sentinel error, text string) {
	t.Helper()
	if err == nil || !errors.Is(err, sentinel) || !strings.Contains(err.Error(), text) {
		t.Fatalf("error = %v, want %v containing %q", err, sentinel, text)
	}
}

// Migration requests are checked before the compare-and-set runs;
// each refusal is pinned by sentinel and message so a dropped one turns one
// case red. A migration "to where it already is" is the one that would leave
// a binding permanently in Migrating with no move to complete.
func TestMigrationRequestsRefuseEachInvalidShape(t *testing.T) {
	service, _ := newService(t)
	ctx := context.Background()
	binding := bind(t, service, 100, 1)

	_, err := service.BeginMigration(ctx, 100, 0, binding.Epoch)
	expectGuard(t, err, ErrRouteInvalid, "target global sid must be positive")
	_, err = service.BeginMigration(ctx, 100, 1, binding.Epoch)
	expectGuard(t, err, ErrRouteInvalid, "game 100 is already served by 1")
	_, err = service.BeginMigration(ctx, 404, 2, 1)
	expectGuard(t, err, ErrRouteMissing, "game 404")

	resolved, err := service.Resolve(ctx, 100)
	if err != nil || resolved.State != RouteActive || resolved.Epoch != binding.Epoch {
		t.Fatalf("refused migrations must leave the binding untouched: %+v, %v", resolved, err)
	}
}

// Redis 存储缺前缀不能构造；完成 / 中止迁移对未知游戏服报 ErrRouteMissing。
func TestMigrationOperationsRefuseUnknownGames(t *testing.T) {
	ctx := context.Background()
	if _, err := NewRedisStores(nil, "  "); err == nil || !strings.Contains(err.Error(), "key prefix is required") {
		t.Fatalf("NewRedisStores with a blank prefix = %v", err)
	}
	service, _ := newService(t)
	if _, err := service.CompleteMigration(ctx, 999, 1); !errors.Is(err, ErrRouteMissing) {
		t.Fatalf("CompleteMigration for an unknown game = %v", err)
	}
	if _, err := service.AbortMigration(ctx, 999, 1); !errors.Is(err, ErrRouteMissing) {
		t.Fatalf("AbortMigration for an unknown game = %v", err)
	}
}

// A retried CompleteMigration at the same epoch is a replay, and is reported
// as one. Counting it as an acceptance makes the migration rate read higher
// than the number of migrations, which is the number operators alarm on.
func TestARetriedCompletionIsReportedAsAReplay(t *testing.T) {
	sink := servicemetrics.NewRecorder()
	service, _ := newService(t, func(cfg *Config) { cfg.Metrics = sink })
	ctx := context.Background()
	binding := bind(t, service, 7, 100)
	moving, err := service.BeginMigration(ctx, 7, 200, binding.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	done, err := service.CompleteMigration(ctx, 7, moving.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	again, err := service.CompleteMigration(ctx, 7, done.Epoch)
	if err != nil || again.Epoch != done.Epoch || again.GlobalSID != 200 {
		t.Fatalf("retried completion: err=%v binding=%+v", err, again)
	}
	if got := sink.Count("accepted:complete_migration"); got != 1 {
		t.Errorf("one migration completed reported %d accepts; %s", got, sink.Events())
	}
	if got := sink.Count("replayed:complete_migration"); got != 1 {
		t.Errorf("a retried completion reported %d replays; %s", got, sink.Events())
	}
}

func TestCompleteMigrationReplaysTheOriginalEpoch(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	initial := bind(t, s, 1, 10)
	moving, err := s.BeginMigration(ctx, 1, 20, initial.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.CompleteMigration(ctx, 1, moving.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.CompleteMigration(ctx, 1, moving.Epoch)
	if err != nil || replay != first {
		t.Fatalf("original completion retry = %+v %v, first=%+v", replay, err, first)
	}
	second, err := s.BeginMigration(ctx, 1, 30, first.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CompleteMigration(ctx, 1, moving.Epoch); !errors.Is(err, ErrRouteStale) {
		t.Fatalf("old completion crossed new migration: %v", err)
	}
	if _, err = s.AbortMigration(ctx, 1, second.Epoch); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CompleteMigration(ctx, 1, second.Epoch); !errors.Is(err, ErrRouteStale) {
		t.Fatalf("aborted migration reported completed: %v", err)
	}
}

func TestRouteCodecRequiresCompletionReceiptFormat(t *testing.T) {
	codec := routeCodec{}
	if _, err := codec.Decode([]byte(`{"GameSID":1,"Epoch":2}`)); err == nil {
		t.Fatal("legacy route accepted")
	}
	want := RouteBinding{Epoch: 3, CompletedFromEpoch: 2}
	raw, err := codec.Encode(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := codec.Decode(raw)
	if err != nil || got != want {
		t.Fatalf("round trip: %+v %v", got, err)
	}
}
