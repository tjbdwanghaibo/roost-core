// External acceptance: a consumer uses two plain local entity kinds and the real
// public manager and dispatcher. No remote manager, group membership setter,
// hand-populated guard, package-private dispatch, or explicitly added peer target.
package companioncontract_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/lock"
)

const (
	pk entity.EntityKind = 181
	ak entity.EntityKind = 182
	ck entity.EntityKind = 183
)

type local struct {
	*entity.EntityBase
	amount int
	record bool // Only the strict admission test asks to persist these values.
}

func (s *local) Base() *entity.EntityBase { return s.EntityBase }
func (s *local) CaptureRollback(tx *nest.RollbackTx) error {
	value, recorded := s.amount, s.record
	tx.DeferRollback(func() error { s.amount, s.record = value, recorded; return nil })
	return nil
}

func (s *local) PrepareCommit(tx *nest.RollbackTx) error {
	if !s.record {
		return nil
	}
	data, err := json.Marshal(s.amount)
	if err != nil {
		return err
	}
	return tx.AddMutation(nest.EntityMutation{Key: dataengine.DocumentKey{Database: "companion_test", Resource: "amount", ID: s.ID()}, Kind: dataengine.MutationPut, ExpectedVersion: 0, NextVersion: 1, Schema: 1, Codec: "json", Data: data})
}

type player struct{ *local }
type activity struct{ *local }
type other struct{ *local }

var register sync.Once

func kinds() {
	register.Do(func() {
		for _, spec := range []struct {
			k    entity.EntityKind
			c    entity.EntityCategory
			make func(*local) entity.IThreadSafeEntity
		}{{pk, entity.EntityCategoryPlayer, func(x *local) entity.IThreadSafeEntity { return &player{x} }}, {ak, entity.EntityCategoryPlayerScoped, func(x *local) entity.IThreadSafeEntity { return &activity{x} }}, {ck, entity.EntityCategoryWorld, func(x *local) entity.IThreadSafeEntity { return &other{x} }}} {
			entity.RegisterEntityBuilder(&entity.EntityBuilderParam{Kind: spec.k, Category: spec.c, NoPersist: true, Builder: func(p *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
				return spec.make(&local{EntityBase: entity.NewEntityBaseWithMutex(p.Id, p.Category, true, p.Mutex, p.Kind)}), nil
			}})
		}
		if err := entity.RegisterCompanionKind[*player, *activity](pk, ak); err != nil {
			panic(err)
		}
	})
}

type fixture struct {
	m        *entity.EntityManager
	n        *nest.NestMgr
	p        *player
	a        *activity
	c        *other
	ctx      context.Context
	rootLock *observedMutex
}

func fresh(t *testing.T, opts ...nest.NestOption) *fixture {
	t.Helper()
	kinds()
	f := &fixture{m: entity.NewEntityManager()}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	t.Cleanup(cancel)
	f.ctx = ctx
	build := func(k entity.EntityKind, u, p int64) entity.IThreadSafeEntity {
		t.Helper()
		param := &entity.EntityCreateParam{IsCreate: true, Kind: k, UniqueID: u, CompanionOf: p}
		if k == pk {
			id, err := entity.BuildEntityID(u, k)
			if err != nil {
				t.Fatal(err)
			}
			f.rootLock = &observedMutex{Mutex: lock.NewDefaultMutex(id), busy: make(chan struct{}, 1)}
			param.Mutex = f.rootLock
		}
		v, err := f.m.Create(param)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	f.p = build(pk, 121, 0).(*player)
	f.a = build(ak, 0, f.p.ID()).(*activity)
	f.c = build(ck, 122, 0).(*other)
	if f.a.UniqueID() != f.p.UniqueID() || f.a.ID() == f.p.ID() {
		t.Fatal("bad derived identity")
	}
	f.n = nest.NewEngine(append([]nest.NestOption{nest.NestOptionWithGetter(entity.NewManagerAccess(f.m))}, opts...)...)
	return f
}
func (f *fixture) on(s string, cb func([]entity.IThreadSafeEntity) (any, error)) nest.HandlerName {
	h := nest.NewHandlerName(s)
	f.n.MustRegisterHandlerWithMeta(h, func(v []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) { return cb(v) }, nest.HandlerMeta{Rollback: nest.RollbackState, Durability: nest.DurabilityMemory})
	return h
}
func (f *fixture) start(t *testing.T) {
	t.Helper()
	if err := f.n.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		if err := f.n.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
}

func TestSingleTargetEnlistsItsPeerAndSwitchesAfterReturn(t *testing.T) {
	f := fresh(t)
	aborted := errors.New("roll back this change")
	adjust := f.on("public_companion_adjust", func(es []entity.IThreadSafeEntity) (any, error) {
		if len(es) != 1 || es[0] != f.p {
			return nil, fmt.Errorf("implicit peer leaked into handler args: %v", es)
		}
		a, err := entity.GetCompanion[*activity]()
		if err != nil {
			return nil, err
		}
		if a != f.a {
			return nil, errors.New("wrong companion")
		}
		f.p.amount += 2
		a.amount += 3
		return nil, nil
	})
	inspect := f.on("public_companion_read", func(es []entity.IThreadSafeEntity) (any, error) {
		p, err := entity.GetPrincipal[*player]()
		if err != nil {
			return nil, err
		}
		if p != f.p || len(es) != 1 || es[0] != f.a {
			return nil, errors.New("bad reverse lookup")
		}
		return [2]int{p.amount, f.a.amount}, nil
	})
	abort := f.on("public_companion_abort", func(_ []entity.IThreadSafeEntity) (any, error) {
		p, err := entity.GetPrincipal[*player]()
		if err != nil {
			return nil, err
		}
		p.amount = 900
		f.a.amount = 500
		if err := entity.DetachCompanion(f.a.ID()); err != nil {
			return nil, err
		}
		return nil, aborted
	})
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	detach := f.on("public_companion_detach", func(_ []entity.IThreadSafeEntity) (any, error) {
		a, err := entity.GetCompanion[*activity]()
		if err != nil {
			return nil, err
		}
		if err = entity.DetachCompanion(a.ID()); err != nil {
			return nil, err
		}
		again, err := entity.GetCompanion[*activity]()
		if err != nil || again != a {
			return nil, fmt.Errorf("detach changed current scope: %v", err)
		}
		close(entered)
		select {
		case <-release:
		case <-f.ctx.Done():
			return nil, f.ctx.Err()
		}
		return nil, nil
	})
	observer := f.observer(t)
	split := observer.on("public_companion_split", func(es []entity.IThreadSafeEntity) (any, error) {
		if len(es) != 1 {
			return nil, errors.New("wrong arity")
		}
		if es[0] == f.a {
			p, e := entity.GetPrincipal[*player]()
			if p != nil || !errors.Is(e, entity.ErrCompanionDetached) {
				return nil, fmt.Errorf("child after detach: %v %v", p, e)
			}
			f.a.amount++
			return nil, nil
		}
		a, e := entity.GetCompanion[*activity]()
		if a != nil || !errors.Is(e, entity.ErrCompanionDetached) {
			return nil, fmt.Errorf("principal after detach: %v %v", a, e)
		}
		return nil, nil
	})
	attach := f.on("public_companion_attach", func(_ []entity.IThreadSafeEntity) (any, error) { return nil, entity.AttachCompanion(f.a.ID()) })
	f.start(t)
	observer.start(t)
	defer unblock()
	call := func(h nest.HandlerName, id int64) (any, error) { return f.n.Request(f.ctx, h, id, nil) }
	if _, e := call(adjust, f.p.ID()); e != nil {
		t.Fatal(e)
	}
	if _, e := call(abort, f.a.ID()); !errors.Is(e, aborted) {
		t.Fatalf("unexpected aborted request: %v", e)
	}
	if v, e := call(inspect, f.a.ID()); e != nil || v != [2]int{2, 3} {
		t.Fatalf("rollback or paired membership lost: %v %v", v, e)
	}
	done := make(chan error, 1)
	go func() { _, e := call(detach, f.p.ID()); done <- e }()
	select {
	case <-entered:
	case e := <-done:
		t.Fatalf("detach did not enter: %v", e)
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	contender, err := observer.contend(split, f.a.ID())
	if err != nil {
		t.Fatal(err)
	}
	unblock()
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	if res := <-contender; res.err != nil {
		t.Fatal(res.err)
	}
	if _, e := observer.n.Request(f.ctx, split, f.p.ID(), nil); e != nil {
		t.Fatal(e)
	}
	if _, e := call(attach, f.p.ID()); e != nil {
		t.Fatal(e)
	}
	if v, e := call(inspect, f.a.ID()); e != nil || v != [2]int{2, 4} {
		t.Fatalf("reattach or independent child write: %v %v", v, e)
	}
}

func TestReversedMultiStillHasThreeLogicalEntities(t *testing.T) {
	f := fresh(t)
	h := f.on("public_companion_three", func(es []entity.IThreadSafeEntity) (any, error) {
		seen := map[int64]bool{}
		for _, e := range es {
			seen[e.ID()] = true
		}
		if len(es) != 3 || len(seen) != 3 || !seen[f.p.ID()] || !seen[f.a.ID()] || !seen[f.c.ID()] {
			return nil, errors.New("lost logical participants")
		}
		f.p.amount++
		f.a.amount += 2
		f.c.amount += 3
		return nil, nil
	})
	f.start(t)
	const loops = 20
	gate := make(chan struct{})
	errs := make(chan error, 2)
	for _, ids := range [][]int64{{f.p.ID(), f.a.ID(), f.c.ID()}, {f.c.ID(), f.a.ID(), f.p.ID()}} {
		go func(ids []int64) {
			<-gate
			for i := 0; i < loops; i++ {
				if _, err := f.n.RequestMulti(f.ctx, h, ids, nil); err != nil {
					errs <- err
					return
				}
			}
			errs <- nil
		}(ids)
	}
	close(gate)
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatal(err)
			}
		case <-f.ctx.Done():
			t.Fatal("multi did not complete", f.ctx.Err())
		}
	}
	if v := [3]int{f.p.amount, f.a.amount, f.c.amount}; v != [3]int{2 * loops, 4 * loops, 6 * loops} {
		t.Fatalf("unexpected end state %v", v)
	}
}
