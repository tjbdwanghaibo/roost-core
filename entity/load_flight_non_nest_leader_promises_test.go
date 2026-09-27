package entity

import (
	"context"
	"errors"
	"testing"
	"time"
)

// OPEN-ITEMS B20：RR-20260926-54 之后共享加载在独立 goroutine 上运行。bf-54 记为“非 Nest 的领头方持有别的实体锁时
// 触发冷加载，没有专门覆盖”。（B20 验证时，没有指定本地执行器的领头方的发布就地在加载 goroutine 上执行；RR-20260927-27
// 起持有实体锁的这类领头方仍在等待时，发布交回领头方自己的 goroutine 执行，本用例的断言对两种位置都成立。）
//
// 验证：Nest 之外的调用方在 WithGuardScope 里持有 X 的锁，Get 冷加载 Y；loader 的发布在自己的 Guard 作用域里取 Y 的锁
// 再 TryAdd（与 DataEngine 仓库经 RunLocal → EntityManager.Create 发布同形）。结论：不死锁，Y 已发布且锁已归还，
// X 的锁在整个过程中仍由领头方持有。
//
// 边界：本用例只覆盖“发布只锁被加载的实体”。loader 的发布还要锁领头方正持有的实体的形状见下方
// TestNonNestLeaderHoldingLockNeededByPublishDoesNotDeadlock（RR-20260927-27）。
type publishUnderOwnGuardLoader struct {
	manager *EntityManager
	locked  chan bool // 发布时领头方持有的 X 是否可被加载 goroutine 取得（应为 false）
	x       IThreadSafeEntity
}

func (l *publishUnderOwnGuardLoader) LoadEntity(ctx context.Context, id int64, _ EntityKind) (IThreadSafeEntity, error) {
	value := newMgrTestEntity(id, testEntityCategoryPlayer)
	var addErr error
	if err := RunLocal(ctx, func() {
		l.locked <- lockableElsewhere(l.x)
		addErr = WithGuardScope("load_publish", func(scope *GuardScope) error {
			if !scope.Guard().RequireEntity(value) {
				return errors.New("publish could not lock the loaded entity")
			}
			return l.manager.TryAdd(value)
		})
	}); err != nil {
		return nil, err
	}
	return value, addErr
}

func TestNonNestLeaderHoldingAnotherLockColdLoadsWithoutDeadlock(t *testing.T) {
	manager := NewEntityManager()
	access := NewManagerAccess(manager)
	x := newMgrTestEntity(mustBuildTestEntityID(t, 5501, testEntityCategoryPlayer, EntityKindNone), testEntityCategoryPlayer)
	if err := manager.TryAdd(x); err != nil {
		t.Fatal(err)
	}
	loader := &publishUnderOwnGuardLoader{manager: manager, locked: make(chan bool, 1), x: x}
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	y := mustBuildTestEntityID(t, 5502, testEntityCategoryPlayer, EntityKindNone)

	type leaderResult struct {
		value IThreadSafeEntity
		err   error
	}
	done := make(chan leaderResult, 1)
	go func() {
		var got leaderResult
		got.err = WithGuardScope("non_nest_leader", func(scope *GuardScope) error {
			if !scope.Guard().RequireEntity(x) {
				return errors.New("leader could not lock X")
			}
			var err error
			got.value, err = access.Get(context.Background(), y, EntityCategoryNone)
			return err
		})
		done <- got
	}()
	var got leaderResult
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the non-Nest leader holding X never got Y back: the cold load's publish deadlocked")
	}
	if got.err != nil || got.value == nil {
		t.Fatalf("leader Get(Y): value=%v err=%v", got.value, got.err)
	}
	if xFreeDuringPublish := <-loader.locked; xFreeDuringPublish {
		t.Fatal("X was lockable by the load goroutine while the leader held it")
	}
	if published := manager.Get(y); published != got.value {
		t.Fatalf("Y not published: manager has %v, leader got %v", published, got.value)
	}
	if !lockableElsewhere(got.value) || !lockableElsewhere(x) {
		t.Fatalf("locks leaked: Y free=%v X free=%v", lockableElsewhere(got.value), lockableElsewhere(x))
	}
}

// RR-20260927-27（B20 相邻形状）：loader 的发布还要锁领头方正持有的 X。RR-54 把发布从领头方 goroutine 挪到加载 goroutine 后，
// 加载 goroutine 在发布里等 X，领头方在等加载；领头方自己的 ctx 到期也离不开——leaderLeft 要等持有 leaderMu 的在途发布结束，
// 永久死锁。修后：Nest 之外、没有指定执行器且持有实体锁的领头方仍在等待时，发布交回领头方自己的 goroutine 执行（修前的位置，
// 可重入锁不阻塞）；领头方先按自己的 ctx 离开时，发布改在加载 goroutine 上执行，等领头方放锁后完成。
type publishLocksLeaderHeldLoader struct {
	manager *EntityManager
	x       IThreadSafeEntity
	gate    chan struct{} // 非 nil 时 LoadEntity 在发布前等它（模拟慢 I/O），用于让领头方先离开
	panics  bool          // 发布步骤 panic：交回领头方执行的步骤 panic 仍按加载 panic 处理
}

func (l *publishLocksLeaderHeldLoader) LoadEntity(ctx context.Context, id int64, _ EntityKind) (IThreadSafeEntity, error) {
	if l.gate != nil {
		<-l.gate
	}
	value := newMgrTestEntity(id, testEntityCategoryPlayer)
	var addErr error
	if err := RunLocal(ctx, func() {
		addErr = WithGuardScope("load_publish_locks_x", func(scope *GuardScope) error {
			if !scope.Guard().RequireEntity(l.x) {
				return errors.New("publish could not lock X")
			}
			if l.panics {
				panic("publish step panicked")
			}
			if !scope.Guard().RequireEntity(value) {
				return errors.New("publish could not lock the loaded entity")
			}
			return l.manager.TryAdd(value)
		})
	}); err != nil {
		return nil, err
	}
	return value, addErr
}

func TestNonNestLeaderHoldingLockNeededByPublishDoesNotDeadlock(t *testing.T) {
	setup := func(t *testing.T, gate chan struct{}, panics bool) (*EntityManager, *ManagerAccess, IThreadSafeEntity, int64) {
		t.Helper()
		manager := NewEntityManager()
		access := NewManagerAccess(manager)
		x := newMgrTestEntity(mustBuildTestEntityID(t, 5511, testEntityCategoryPlayer, EntityKindNone), testEntityCategoryPlayer)
		if err := manager.TryAdd(x); err != nil {
			t.Fatal(err)
		}
		if _, err := access.ConfigureLoader(&publishLocksLeaderHeldLoader{manager: manager, x: x, gate: gate, panics: panics}); err != nil {
			t.Fatal(err)
		}
		return manager, access, x, mustBuildTestEntityID(t, 5512, testEntityCategoryPlayer, EntityKindNone)
	}
	type leaderResult struct {
		value IThreadSafeEntity
		err   error
	}
	leadWhileHoldingX := func(access *ManagerAccess, x IThreadSafeEntity, y int64, budget time.Duration, done chan<- leaderResult) {
		var got leaderResult
		got.err = WithGuardScope("non_nest_leader_holding_x", func(scope *GuardScope) error {
			if !scope.Guard().RequireEntity(x) {
				return errors.New("leader could not lock X")
			}
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()
			var err error
			got.value, err = access.Get(ctx, y, EntityCategoryNone)
			return err
		})
		done <- got
	}

	t.Run("publish_while_leader_waits", func(t *testing.T) {
		manager, access, x, y := setup(t, nil, false)
		done := make(chan leaderResult, 1)
		go leadWhileHoldingX(access, x, y, 300*time.Millisecond, done)
		var got leaderResult
		select {
		case got = <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("leader (own ctx 300ms) still stuck after 3s: the load goroutine's publish waits for X held by the leader, and the leader cannot leave because leaderLeft waits for that publish (deadlock)")
		}
		if got.err != nil || got.value == nil {
			t.Fatalf("leader Get(Y): value=%v err=%v", got.value, got.err)
		}
		if published := manager.Get(y); published != got.value {
			t.Fatalf("Y not published: manager has %v, leader got %v", published, got.value)
		}
		if !lockableElsewhere(got.value) || !lockableElsewhere(x) {
			t.Fatalf("locks leaked: Y free=%v X free=%v", lockableElsewhere(got.value), lockableElsewhere(x))
		}
	})

	t.Run("leader_leaves_before_publish", func(t *testing.T) {
		gate := make(chan struct{})
		manager, access, x, y := setup(t, gate, false)
		done := make(chan leaderResult, 1)
		go leadWhileHoldingX(access, x, y, 100*time.Millisecond, done)
		var got leaderResult
		select {
		case got = <-done:
		case <-time.After(3 * time.Second):
			close(gate)
			t.Fatal("leader did not leave on its own ctx while the load was still in I/O")
		}
		if !errors.Is(got.err, context.DeadlineExceeded) {
			t.Fatalf("leader err=%v, want its own context.DeadlineExceeded", got.err)
		}
		close(gate) // 领头方已离开并放掉 X：发布在加载 goroutine 上取 X、Y 后完成
		deadline := time.Now().Add(3 * time.Second)
		for manager.Get(y) == nil && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		published := manager.Get(y)
		if published == nil {
			t.Fatal("Y was never published after the leader left and released X")
		}
		value, err := access.Get(context.Background(), y, EntityCategoryNone)
		if err != nil || value != published {
			t.Fatalf("Get(Y) after publish: value=%v err=%v", value, err)
		}
		if !lockableElsewhere(published) || !lockableElsewhere(x) {
			t.Fatalf("locks leaked: Y free=%v X free=%v", lockableElsewhere(published), lockableElsewhere(x))
		}
	})
	t.Run("publish_panics_on_leader", func(t *testing.T) {
		manager, access, x, y := setup(t, nil, true)
		panicked := make(chan any, 1)
		go func() {
			defer func() { panicked <- recover() }()
			done := make(chan leaderResult, 1)
			leadWhileHoldingX(access, x, y, 2*time.Second, done)
		}()
		select {
		case r := <-panicked:
			if r == nil {
				t.Fatal("the leader returned normally although its load's publish step panicked")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("leader stuck after the publish step panicked")
		}
		if manager.Get(y) != nil {
			t.Fatal("Y was published although the publish step panicked")
		}
		if !lockableElsewhere(x) {
			t.Fatal("X still locked after the leader's scope unwound")
		}
		access.flightMu.Lock()
		_, inFlight := access.flights[y]
		access.flightMu.Unlock()
		if inFlight {
			t.Fatal("the panicked load left its flight behind")
		}
	})
}
