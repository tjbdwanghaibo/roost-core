package nest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/internal/entitypermit"
)

const compTestPK entity.EntityKind = 232
const compTestAK entity.EntityKind = 233

type nestTestPrincipal struct{ *rollbackTestEntity }
type nestTestCompanion struct{ *rollbackTestEntity }

var compTestRegister sync.Once

type compNestFixture struct {
	*NestMgr
	em  *entity.EntityManager
	p   *nestTestPrincipal
	a   *nestTestCompanion
	ctx context.Context
}

func newCompNestFixture(t *testing.T, committer TransactionCommitter) *compNestFixture {
	t.Helper()
	compTestRegister.Do(func() {
		for _, k := range []entity.EntityKind{compTestPK, compTestAK} {
			entity.RegisterEntityBuilder(&entity.EntityBuilderParam{Kind: k, Category: entity.EntityCategoryPlayerScoped, NoPersist: true, Builder: func(p *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
				e := &rollbackTestEntity{EntityBase: entity.NewEntityBaseWithMutex(p.Id, p.Category, true, p.Mutex, p.Kind), dao: &rollbackTestDao{id: p.Id}}
				if p.Kind == compTestPK {
					return &nestTestPrincipal{e}, nil
				}
				return &nestTestCompanion{e}, nil
			}})
		}
		if err := entity.RegisterCompanionKind[*nestTestPrincipal, *nestTestCompanion](compTestPK, compTestAK); err != nil {
			panic(err)
		}
	})
	f := &compNestFixture{em: entity.NewEntityManager()}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	t.Cleanup(cancel)
	f.ctx = ctx
	p, err := f.em.Create(&entity.EntityCreateParam{IsCreate: true, Kind: compTestPK, UniqueID: 7321})
	if err != nil {
		t.Fatal(err)
	}
	f.p = p.(*nestTestPrincipal)
	a, err := f.em.Create(&entity.EntityCreateParam{IsCreate: true, Kind: compTestAK, CompanionOf: p.ID()})
	if err != nil {
		t.Fatal(err)
	}
	f.a = a.(*nestTestCompanion)
	f.NestMgr = NewEngine(NestOptionWithGetter(entity.NewManagerAccess(f.em)), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 2, QueueCap: 24}, WorkerPoolConfig{}, WorkerPoolConfig{}))
	return f
}
func (f *compNestFixture) start(t *testing.T) {
	t.Helper()
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		if err := f.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
}
func (f *compNestFixture) on(name string, meta HandlerMeta, fn func([]entity.IThreadSafeEntity) error) HandlerName {
	h := NewHandlerName(name)
	f.MustRegisterHandlerWithMeta(h, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) { return nil, fn(es) }, meta)
	return h
}

func TestCompanionStrictOutcomeAndIndependentDAOs(t *testing.T) {
	refused := errors.New("business refusal")
	for _, tc := range []struct {
		name             string
		business, commit error
		applied          bool
	}{{"success", nil, nil, true}, {"handler_failure", refused, nil, false}, {"wal_refusal", nil, refused, false}, {"unknown", nil, ErrCommitIndeterminate, false}} {
		t.Run(tc.name, func(t *testing.T) {
			c := &recordingCommitter{err: tc.commit}
			f := newCompNestFixture(t, c)
			h := f.on("companion_"+tc.name, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict}, func(es []entity.IThreadSafeEntity) error {
				if len(es) != 1 || es[0] != f.p {
					return errors.New("logical peer leaked into handler params")
				}
				a, err := entity.GetCompanion[*nestTestCompanion]()
				if err != nil {
					return err
				}
				if a != f.a {
					return errors.New("wrong peer")
				}
				for _, d := range []*rollbackTestDao{f.p.dao, a.dao} {
					d.Value++
					if err := MarkPersist(d, 1); err != nil {
						return err
					}
				}
				if err := entity.DetachCompanion(a.ID()); err != nil {
					return err
				}
				if got, err := entity.GetCompanion[*nestTestCompanion](); err != nil || got != a {
					return errors.New("changed view while handler was running")
				}
				return tc.business
			})
			f.start(t)
			_, err := f.Request(f.ctx, h, f.p.ID(), nil)
			if tc.applied != (err == nil) {
				t.Fatalf("outcome %v", err)
			}
			if attached := entity.SnapshotCompanionDomain(f.a).Root == f.p; attached == tc.applied {
				t.Fatalf("wrong relation: attached=%v after %v", attached, err)
			}
			if tc.applied {
				if len(c.record.Mutations) != 2 || c.record.Mutations[0].Key.ID == c.record.Mutations[1].Key.ID || f.a.dao.Tracker.Version() != 1 || f.p.dao.Tracker.Version() != 1 {
					t.Fatalf("independent documents not accepted: %#v", c.record)
				}
			} else if tc.commit != ErrCommitIndeterminate && (f.p.dao.Value != 0 || f.a.dao.Value != 0) {
				t.Fatal("explicit refusal failed to roll back both entities")
			}
			if tc.commit == ErrCommitIndeterminate && (!errors.Is(err, ErrCommitIndeterminate) || f.FenceError() == nil) {
				t.Fatalf("unknown not fenced: %v", err)
			}
		})
	}
}

func TestCompanionScopesAndFastPaths(t *testing.T) {
	f := newCompNestFixture(t, &recordingCommitter{})
	results := make(chan error, 4)
	meta := HandlerMeta{Rollback: RollbackState, Durability: DurabilityMemory}
	h := f.on("companion_fast", meta, func(es []entity.IThreadSafeEntity) error {
		check := func() error {
			if len(es) != 1 {
				return errors.New("bad arguments")
			}
			if es[0] == f.a {
				if p, e := entity.GetPrincipal[*nestTestPrincipal](); e != nil || p != f.p {
					return fmt.Errorf("missing principal: %v", e)
				}
				if _, e := entity.GetCompanion[*nestTestCompanion](); e == nil {
					return errors.New("child acquired itself")
				}
			} else {
				if a, e := entity.GetCompanion[*nestTestCompanion](); e != nil || a != f.a {
					return fmt.Errorf("missing companion: %v", e)
				}
			}
			if err := entity.WithGuardScope(entitypermit.System(), "inner", func(*entity.GuardScope) error {
				if _, e := entity.GetCompanion[*nestTestCompanion](); e == nil {
					return errors.New("inner guard inherited view")
				}
				if _, e := entity.GetPrincipal[*nestTestPrincipal](); e == nil {
					return errors.New("inner guard inherited principal")
				}
				return nil
			}); err != nil {
				return err
			}
			_, err := RunIsolatedTransaction(f.ctx, &recordingCommitter{}, "isolated", func() (any, error) {
				if _, e := entity.GetCompanion[*nestTestCompanion](); e == nil {
					return nil, errors.New("inner tx inherited companion")
				}
				if _, e := entity.GetPrincipal[*nestTestPrincipal](); e == nil {
					return nil, errors.New("inner tx inherited principal")
				}
				if _, e := CastMulti(NewCastTarget(es[0].ID())); e == nil {
					return nil, errors.New("nested Cast borrowed outer tx")
				}
				return nil, nil
			})
			return err
		}()
		results <- check
		return check
	})
	multi := f.on("companion_multi", meta, func(es []entity.IThreadSafeEntity) error {
		if len(es) != 3 || es[0] != f.a || es[1] != f.p || es[2] != f.a {
			return errors.New("argument order/duplicates changed")
		}
		if len(CurrentRollbackTx().snapshotted) != 2 {
			return errors.New("logical closure not exactly once")
		}
		if _, err := entity.GetCompanion[*nestTestCompanion](); err == nil {
			return errors.New("ambiguous multi has a subject")
		}
		return nil
	})
	f.start(t)
	if err := f.DispatchBroadcast(f.ctx, h, []int64{f.p.ID(), f.a.ID()}, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.DispatchTryBatch(f.ctx, []TryBatchTarget{{Handler: h, ID: f.p.ID()}}, func(_ TryBatchTarget, e error) { results <- e }); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-f.ctx.Done():
			t.Fatal("fast path stalled")
		}
	}
	if _, err := f.RequestMulti(f.ctx, multi, []int64{f.a.ID(), f.p.ID(), f.a.ID()}, nil); err != nil {
		t.Fatal(err)
	}
}
func TestCompanionPipelinedTransitionRejectedWithoutChangingDurability(t *testing.T) {
	c := newPipelinedTestCommitter(true)
	f := newCompNestFixture(t, c)
	h := f.on("companion_pipe", HandlerMeta{Rollback: RollbackState, Durability: DurabilityPipelined}, func(es []entity.IThreadSafeEntity) error { return entity.DetachCompanion(f.a.ID()) })
	f.start(t)
	if _, err := f.Request(f.ctx, h, f.a.ID(), nil); !errors.Is(err, ErrCompanionChangeUnsupported) {
		t.Fatalf("pipeline lifecycle error=%v", err)
	}
	if s := entity.SnapshotCompanionDomain(f.a); s.Root != f.p {
		t.Fatal("pipeline changed relationship")
	}
}
