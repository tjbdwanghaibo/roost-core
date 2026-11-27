// External contract: plain local entities, their real manager, and only public Nest
// requests. No companion declarations, lock-group setup, test guards, or raw locks.
package peergroupcontract_test

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

const (
	unitKind   entity.EntityKind = 187
	ledgerKind entity.EntityKind = 188
	red        int64             = 4501
	blue       int64             = 4502
	database                     = "peer_contract"
	resource                     = "balance"
)

type state struct {
	*entity.EntityBase
	balance int
	persist bool
}

func (s *state) Base() *entity.EntityBase { return s.EntityBase }
func (s *state) CaptureRollback(tx *nest.RollbackTx) error {
	balance, persist := s.balance, s.persist
	tx.DeferRollback(func() error { s.balance, s.persist = balance, persist; return nil })
	return nil
}
func (s *state) PrepareCommit(tx *nest.RollbackTx) error {
	if !s.persist {
		return nil
	}
	return tx.AddMutation(nest.EntityMutation{
		Key:  dataengine.DocumentKey{Database: database, Resource: resource, ID: s.ID()},
		Kind: dataengine.MutationPut, ExpectedVersion: 0, NextVersion: 1, Schema: 1,
		Codec: "json", Data: []byte(strconv.Itoa(s.balance)),
	})
}

// A and B necessarily have the same kind, category, and concrete Go type.
// C is a different concrete type. None has a special owner/principal role.
type unit struct{ *state }
type ledger struct{ *state }

var registerKinds sync.Once

type world struct {
	manager *entity.EntityManager
	a, b    *unit
	c, d    *ledger
	ctx     context.Context
}

func newWorld(t *testing.T) *world {
	t.Helper()
	registerKinds.Do(func() {
		for _, k := range []entity.EntityKind{unitKind, ledgerKind} {
			entity.RegisterEntityBuilder(&entity.EntityBuilderParam{Kind: k, Category: entity.EntityCategoryWorld, NoPersist: true, Builder: func(p *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
				s := &state{EntityBase: entity.NewEntityBaseWithMutex(p.Id, p.Category, true, p.Mutex, p.Kind)}
				if p.Kind == unitKind {
					return &unit{s}, nil
				}
				return &ledger{s}, nil
			}})
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	w := &world{manager: entity.NewEntityManager(), ctx: ctx}
	create := func(k entity.EntityKind, unique int64) entity.IThreadSafeEntity {
		t.Helper()
		e, err := w.manager.Create(&entity.EntityCreateParam{IsCreate: true, Kind: k, UniqueID: unique})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	w.a = create(unitKind, 501).(*unit)
	w.b = create(unitKind, 502).(*unit)
	w.c = create(ledgerKind, 503).(*ledger)
	w.d = create(ledgerKind, 504).(*ledger)
	return w
}

type fixture struct {
	*world
	n                    *nest.NestMgr
	join, probe, balance nest.HandlerName
}

type handler func([]entity.IThreadSafeEntity, []any) (any, error)

func (f *fixture) on(name string, durability nest.DurabilityPolicy, h handler) nest.HandlerName {
	n := nest.NewHandlerName("peer_contract_" + name)
	f.n.MustRegisterHandlerWithMeta(n, func(es []entity.IThreadSafeEntity, ps []any, _ ...nest.HandlerOption) (any, error) { return h(es, ps) }, nest.HandlerMeta{Rollback: nest.RollbackState, Durability: durability})
	return n
}

func (w *world) engine(opts ...nest.NestOption) *fixture {
	f := &fixture{world: w, n: nest.NewEngine(append([]nest.NestOption{nest.NestOptionWithGetter(entity.NewManagerAccess(w.manager)), nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 4, QueueCap: 64}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{})}, opts...)...)}
	f.join = f.on("join", nest.DurabilityMemory, func(es []entity.IThreadSafeEntity, ps []any) (any, error) {
		if len(es) != 1 {
			return nil, fmt.Errorf("join: got %d explicit targets, want 1", len(es))
		}
		return nil, nest.JoinEntityGroup(ps[0].(int64))
	})
	f.probe = f.on("probe", nest.DurabilityMemory, func(es []entity.IThreadSafeEntity, ps []any) (any, error) {
		if len(es) != 1 || es[0].ID() != ps[0].(int64) {
			return nil, fmt.Errorf("probe explicit targets: %v", es)
		}
		id := ps[1].(int64)
		switch id {
		case w.a.ID():
			return visible(w.a)
		case w.b.ID():
			return visible(w.b)
		case w.c.ID():
			return visible(w.c)
		case w.d.ID():
			return visible(w.d)
		default:
			return nil, fmt.Errorf("invalid fixture target %d", id)
		}
	})
	f.balance = f.on("balance", nest.DurabilityMemory, func(es []entity.IThreadSafeEntity, _ []any) (any, error) {
		if len(es) != 1 {
			return nil, fmt.Errorf("balance: %d targets", len(es))
		}
		switch e := es[0].(type) {
		case *unit:
			return e.balance, nil
		case *ledger:
			return e.balance, nil
		}
		return nil, fmt.Errorf("unexpected %T", es[0])
	})
	return f
}

type peer interface {
	entity.IThreadSafeEntity
	comparable
}

func visible[T peer](want T) (bool, error) {
	got, err := nest.GetGroupMember[T](want.ID())
	var zero T
	switch {
	case err != nil && got == zero:
		return false, nil
	case err == nil && got == want:
		return true, nil
	default:
		return false, fmt.Errorf("GetGroupMember[%T](%d) = %v, %v; want exact member/nil or nil/error", want, want.ID(), got, err)
	}
}

func requireMember[T peer](want T) (T, error) {
	got, err := nest.GetGroupMember[T](want.ID())
	if err != nil || got != want {
		var zero T
		return zero, fmt.Errorf("GetGroupMember[%T](%d) = %v, %v; want exact member", want, want.ID(), got, err)
	}
	return got, nil
}

func (f *fixture) start(t *testing.T) {
	t.Helper()
	if err := f.n.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := f.n.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
}

func (f *fixture) must(t *testing.T, h nest.HandlerName, e entity.IThreadSafeEntity, ps ...any) any {
	t.Helper()
	v, err := f.n.Request(f.ctx, h, e.ID(), ps)
	if err != nil {
		t.Fatalf("%s(%d): %v", h, e.ID(), err)
	}
	return v
}
func (f *fixture) group(t *testing.T, id int64, es ...entity.IThreadSafeEntity) {
	t.Helper()
	for _, e := range es {
		f.must(t, f.join, e, id)
	}
}
func (f *fixture) checkView(t *testing.T, from, to entity.IThreadSafeEntity, want bool) {
	t.Helper()
	if got := f.must(t, f.probe, from, from.ID(), to.ID()); got != want {
		t.Fatalf("visibility %d -> %d = %v, want %t", from.ID(), to.ID(), got, want)
	}
}
func (f *fixture) checkBalance(t *testing.T, e entity.IThreadSafeEntity, want int) {
	t.Helper()
	if got := f.must(t, f.balance, e); got != want {
		t.Fatalf("balance(%d) = %v, want %d", e.ID(), got, want)
	}
}

// Check both directions for every distinct pair. Never assume that a member lookup
// for the current entity itself is supported.
func (f *fixture) checkTrio(t *testing.T, aIn bool) {
	t.Helper()
	for _, pair := range [][2]entity.IThreadSafeEntity{{f.a, f.b}, {f.a, f.c}, {f.b, f.c}} {
		want := aIn || pair[0] != f.a
		f.checkView(t, pair[0], pair[1], want)
		f.checkView(t, pair[1], pair[0], want)
	}
}
