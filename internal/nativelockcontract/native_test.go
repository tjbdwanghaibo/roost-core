// 仅用公开业务 API。真实 Nest 可以取得实体与组内伙伴，不代表 handler、旧引用或
// 另一个 goroutine 获得原生锁票；测试不导入框架内部授权包。
package nativelockcontract_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/lock"
)

type account struct {
	*entity.EntityBase
	balance int
}

func (a *account) Base() *entity.EntityBase { return a.EntityBase }
func (a *account) CaptureRollback(tx *nest.RollbackTx) error {
	v := a.balance
	tx.DeferRollback(func() error { a.balance = v; return nil })
	return nil
}

func scopePanic(fn func()) (ok bool) {
	defer func() { err, _ := recover().(error); ok = errors.Is(err, entity.ErrEntityLockScope) }()
	fn()
	return false
}

type businessMutex interface {
	lock.Mutex
	LockContext(context.Context) error
}

func denied(mu businessMutex, id int64) error {
	if mu.LockId() != id {
		return fmt.Errorf("lock identity %d, want %d", mu.LockId(), id)
	}
	if mu.TryLock() {
		return errors.New("TryLock granted a ticket")
	}
	if mu.LockWithTimeout(time.Millisecond) {
		return errors.New("timed lock granted a ticket")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := mu.LockContext(ctx); !errors.Is(err, entity.ErrEntityLockScope) {
		return fmt.Errorf("LockContext returned %v, want scope denial", err)
	}
	if err := lock.LockContext(ctx, mu); !errors.Is(err, entity.ErrEntityLockScope) {
		return fmt.Errorf("lock.LockContext adapter returned %v, want scope denial", err)
	}
	if !scopePanic(mu.Unlock) {
		return errors.New("Unlock did not panic with scope denial")
	}
	if !scopePanic(mu.Lock) {
		return errors.New("Lock did not panic with scope denial")
	}
	return nil
}

const kind entity.EntityKind = 190

var once sync.Once

func TestNativeLockFacadeNeverOwnsTheBusinessTicket(t *testing.T) {
	once.Do(func() {
		entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: entity.EntityCategoryWorld})
	})
	m := entity.NewEntityManager()
	create := func(unique int64) (*account, businessMutex, lock.Mutex) {
		t.Helper()
		id, err := entity.BuildEntityID(unique, kind)
		if err != nil {
			t.Fatal(err)
		}
		physical := lock.NewDefaultMutex(id)
		a := &account{EntityBase: entity.NewEntityBaseWithMutex(id, entity.EntityCategoryWorld, true, physical, kind)}
		old := a.GetMutex() // 构造后、第一次发布前缓存的引用。
		if err := denied(old, id); err != nil {
			t.Fatalf("before publication: %v", err)
		}
		if !physical.TryLock() {
			t.Fatal("idle positive control: native lock unavailable")
		}
		physical.Unlock()
		if err := m.TryAdd(a); err != nil {
			t.Fatal(err)
		}
		return a, old, physical
	}
	a, beforePublish, nativeA := create(1)
	b, _, nativeB := create(2)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	n := nest.NewEngine(nest.NestOptionWithGetter(entity.NewManagerAccess(m)))
	join := nest.NewHandlerName("native_contract_join")
	work := nest.NewHandlerName("native_contract_transfer")
	verify := nest.NewHandlerName("native_contract_read")
	meta := nest.HandlerMeta{Rollback: nest.RollbackState, Durability: nest.DurabilityMemory}
	n.MustRegisterHandlerWithMeta(join, func(_ []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		return nil, nest.JoinEntityGroup(501)
	}, meta)
	n.MustRegisterHandlerWithMeta(work, func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		if len(es) != 1 || es[0] != a {
			return nil, fmt.Errorf("explicit entities: %v", es)
		}
		self, err := nest.CastOne[*account](a.ID())
		if err != nil || self != a {
			return nil, fmt.Errorf("CastOne self = %v, %v", self, err)
		}
		peer, err := nest.GetGroupMember[*account](b.ID())
		if err != nil || peer != b {
			return nil, fmt.Errorf("GetGroupMember peer = %v, %v", peer, err)
		}
		self.balance += 10
		peer.balance += 20
		if err := denied(self.GetMutex(), a.ID()); err != nil {
			return nil, fmt.Errorf("explicit target: %w", err)
		}
		if err := denied(peer.GetMutex(), b.ID()); err != nil {
			return nil, fmt.Errorf("group getter: %w", err)
		}
		if err := denied(beforePublish, a.ID()); err != nil {
			return nil, fmt.Errorf("retained before publication: %w", err)
		}
		escaped := peer.GetMutex()
		other := make(chan error, 1)
		go func() {
			if err := denied(escaped, b.ID()); err != nil {
				other <- err
				return
			}
			// 拒绝 Unlock 不得先把 Nest 的票还掉再 panic。这里试的是注入夹具的
			// 两把原锁，而非总会拒绝的业务接口。
			for _, mu := range []lock.Mutex{nativeA, nativeB} {
				if mu.TryLock() {
					mu.Unlock()
					other <- fmt.Errorf("Nest lost the physical lock for %d inside handler", mu.LockId())
					return
				}
			}
			other <- nil
		}()
		select {
		case err := <-other:
			if err != nil {
				return nil, fmt.Errorf("other goroutine: %w", err)
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		self.balance++
		peer.balance++
		return escaped, nil
	}, meta)
	n.MustRegisterHandlerWithMeta(verify, func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		if len(es) != 1 || es[0] != b {
			return nil, fmt.Errorf("wrong read target: %v", es)
		}
		peer, err := nest.GetGroupMember[*account](a.ID())
		if err != nil {
			return nil, err
		}
		return [2]int{peer.balance, es[0].(*account).balance}, nil
	}, meta)
	if err := n.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stop, done := context.WithTimeout(context.Background(), 3*time.Second)
		defer done()
		if err := n.Shutdown(stop); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	}()
	for _, e := range []*account{a, b} {
		if _, err := n.Request(ctx, join, e.ID(), nil); err != nil {
			t.Fatal(err)
		}
	}
	v, err := n.Request(ctx, work, a.ID(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := denied(v.(businessMutex), b.ID()); err != nil {
		t.Fatalf("retained after handler: %v", err)
	}
	if v, err := n.Request(ctx, verify, b.ID(), nil); err != nil || v != [2]int{11, 21} {
		t.Fatalf("next Nest request saw %v, %v", v, err)
	}
	for _, mu := range []lock.Mutex{nativeA, nativeB} {
		if !mu.TryLock() {
			t.Fatalf("after Nest release physical lock %d is still held", mu.LockId())
		}
		mu.Unlock()
	}
}
