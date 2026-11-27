package companioncontract_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// Deliberately hold the transaction at the authority boundary AFTER the business
// handler returned. Never infer commitment from a successful DetachCompanion call.
type authority struct {
	called   chan nest.CommitRecord
	decision chan error
}

func (a *authority) Commit(ctx context.Context, r nest.CommitRecord) error {
	select {
	case a.called <- r:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case e := <-a.decision:
		return e
	case <-ctx.Done():
		return ctx.Err()
	}
}
func TestStrictCompanionAdmissionIsOneRecordAndRejectRestoresBoth(t *testing.T) {
	for _, target := range []string{"principal", "companion"} {
		t.Run(target, func(t *testing.T) { checkStrictAdmission(t, target) })
	}
}

func checkStrictAdmission(t *testing.T, target string) {
	a := &authority{called: make(chan nest.CommitRecord, 1), decision: make(chan error, 1)}
	f := fresh(t, nest.NestOptionWithTransactionCommitter(a))
	h := nest.NewHandlerName("public_companion_strict")
	f.n.MustRegisterHandlerWithMeta(h, func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		peer, e := entity.GetCompanion[*activity]()
		if e != nil {
			return nil, e
		}
		if len(es) != 1 || es[0] != f.p || peer != f.a {
			return nil, errors.New("unexpected implicit/explicit targets")
		}
		f.p.amount, peer.amount = 13, 21
		f.p.record, peer.record = true, true
		return nil, entity.DetachCompanion(peer.ID())
	}, nest.HandlerMeta{Rollback: nest.RollbackState, Durability: nest.DurabilityStrict})
	probe := f.on("public_companion_verify", func(_ []entity.IThreadSafeEntity) (any, error) {
		p, e := entity.GetPrincipal[*player]()
		if e != nil {
			return nil, e
		}
		return [2]int{p.amount, f.a.amount}, nil
	})
	observer := f.observer(t)
	id := f.p.ID()
	if target == "companion" {
		id = f.a.ID()
	}
	readTarget := observer.on("public_contending_read", func(es []entity.IThreadSafeEntity) (any, error) {
		if len(es) != 1 || es[0].ID() != id {
			return nil, fmt.Errorf("unexpected target: %v", es)
		}
		switch x := es[0].(type) {
		case *player:
			return x.amount, nil
		case *activity:
			return x.amount, nil
		default:
			return nil, fmt.Errorf("unexpected target type %T", x)
		}
	})
	f.start(t)
	observer.start(t)
	reject := errors.New("known rejection")
	issue := func(want error) {
		t.Helper()
		errch := make(chan error, 1)
		go func() { _, e := f.n.Request(f.ctx, h, f.p.ID(), nil); errch <- e }()
		var r nest.CommitRecord
		select {
		case r = <-a.called:
		case e := <-errch:
			t.Fatalf("no commit record: %v", e)
		case <-f.ctx.Done():
			t.Fatal(f.ctx.Err())
		}
		// Do not Fatal before returning the authority decision: avoid a hung shutdown on error.
		valid := map[int64]string{}
		for _, m := range r.Mutations {
			valid[m.Key.ID] = string(m.Data)
		}
		if len(r.Mutations) != 2 || valid[f.p.ID()] != "13" || valid[f.a.ID()] != "21" {
			t.Errorf("one logical write vanished from durable record: %+v", valid)
		}
		contender, err := observer.contend(readTarget, id)
		a.decision <- want // 放行后才报告探针失败，保证提交者不会挂住。
		if err != nil {
			t.Fatal(err)
		}
		value := 0
		if want == nil {
			if target == "principal" {
				value = 13
			} else {
				value = 21
			}
		}
		if res := <-contender; res.err != nil || res.value != value {
			t.Fatalf("%s read after decision: %+v, want %d", target, res, value)
		}
		select {
		case e := <-errch:
			if !errors.Is(e, want) {
				t.Fatalf("decision: got %v want %v", e, want)
			}
		case <-f.ctx.Done():
			t.Fatal(f.ctx.Err())
		}
	}
	issue(reject)
	if v, e := f.n.Request(f.ctx, probe, f.a.ID(), nil); e != nil || v != [2]int{0, 0} {
		t.Fatalf("rejection damaged relationship or values: %v %v", v, e)
	}
	issue(nil)
	if _, e := f.n.Request(f.ctx, probe, f.a.ID(), nil); !errors.Is(e, entity.ErrCompanionDetached) {
		t.Fatal(fmt.Errorf("accepted split was not visible: %v", e))
	}
}
