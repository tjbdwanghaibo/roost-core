package peergroupcontract_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

func TestPeerMembersSeeEachOtherAndLeaveTakesEffectAfterReturn(t *testing.T) {
	f := newWorld(t).engine()
	denied := errors.New("business aborted transfer")
	transfer := f.on("transfer", nest.DurabilityMemory, func(es []entity.IThreadSafeEntity, ps []any) (any, error) {
		if len(es) != 1 || es[0] != f.a {
			return nil, fmt.Errorf("transfer explicit targets: %v", es)
		}
		b, err := requireMember(f.b)
		if err != nil {
			return nil, err
		}
		c, err := requireMember(f.c)
		if err != nil {
			return nil, err
		}
		es[0].(*unit).balance += 5
		b.balance += 8
		c.balance += 13
		if err := nest.LeaveEntityGroup(); err != nil {
			return nil, err
		}
		// The leave is an intent for the next dispatch, not a change to this view.
		if _, err := requireMember(f.b); err != nil {
			return nil, err
		}
		if _, err := requireMember(f.c); err != nil {
			return nil, err
		}
		if ps[0].(bool) {
			return nil, denied
		}
		return nil, nil
	})
	reverseWrite := f.on("reverse_write", nest.DurabilityMemory, func(es []entity.IThreadSafeEntity, _ []any) (any, error) {
		if len(es) != 1 || es[0] != f.c {
			return nil, fmt.Errorf("reverse explicit targets: %v", es)
		}
		a, err := requireMember(f.a)
		if err != nil {
			return nil, err
		}
		b, err := requireMember(f.b)
		if err != nil {
			return nil, err
		}
		a.balance += 1
		b.balance += 2
		es[0].(*ledger).balance += 3
		return nil, nil
	})
	scope := f.on("scope", nest.DurabilityMemory, func(es []entity.IThreadSafeEntity, _ []any) (any, error) {
		if len(es) != 1 || es[0] != f.a {
			return nil, fmt.Errorf("scope explicit targets: %v", es)
		}
		if _, err := requireMember(f.b); err != nil {
			return nil, err
		}
		if wrong, err := nest.GetGroupMember[*ledger](f.b.ID()); wrong != nil || err == nil {
			return nil, fmt.Errorf("wrong concrete type: got %v, %v; want nil/error", wrong, err)
		}
		done := make(chan error, 1)
		go func() {
			b, err := nest.GetGroupMember[*unit](f.b.ID())
			if b != nil || err == nil {
				done <- fmt.Errorf("foreign goroutine: got %v, %v; want nil/error", b, err)
				return
			}
			done <- nil
		}()
		select {
		case err := <-done:
			return nil, err
		case <-f.ctx.Done():
			return nil, fmt.Errorf("foreign goroutine getter blocked: %w", f.ctx.Err())
		}
	})
	f.start(t)
	// The same type is deliberate: only the logical ID distinguishes A from B.
	f.group(t, red, f.b, f.c, f.a)
	f.group(t, blue, f.d)
	f.checkTrio(t, true)
	f.checkView(t, f.a, f.d, false)
	f.checkView(t, f.d, f.c, false)
	f.must(t, scope, f.a)
	f.must(t, reverseWrite, f.c)
	if _, err := f.n.Request(f.ctx, transfer, f.a.ID(), nest.Params{true}); !errors.Is(err, denied) {
		t.Fatalf("aborted transfer: %v", err)
	}
	f.checkTrio(t, true)
	f.checkBalance(t, f.a, 1)
	f.checkBalance(t, f.b, 2)
	f.checkBalance(t, f.c, 3)
	f.must(t, transfer, f.a, false)
	f.checkTrio(t, false)
	f.checkBalance(t, f.a, 6)
	f.checkBalance(t, f.b, 10)
	f.checkBalance(t, f.c, 16)
	f.group(t, red, f.a)
	f.checkTrio(t, true)
	f.must(t, reverseWrite, f.c)
	f.checkBalance(t, f.a, 7)
	f.checkBalance(t, f.b, 12)
	f.checkBalance(t, f.c, 19)
}
