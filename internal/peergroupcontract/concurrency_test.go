package peergroupcontract_test

import (
	"fmt"
	"runtime"
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// Deliberately expose a lost update if two business handlers are accidentally
// allowed to write the same entity at once. No application mutex or atomic counter.
func addOne(s *state) { n := s.balance; runtime.Gosched(); s.balance = n + 1 }

func TestReversedMultiAcrossTwoGroupsDuringChurnAndPeerWrites(t *testing.T) {
	f := newWorld(t).engine()
	multi := f.on("multi", nest.DurabilityMemory, func(es []entity.IThreadSafeEntity, _ []any) (any, error) {
		// B anchors red throughout A's transitions; C anchors blue. Thus EVERY
		// multi dispatch spans both groups, even if A is temporarily standalone.
		if len(es) != 3 || !slices.Contains(es, entity.IThreadSafeEntity(f.a)) || !slices.Contains(es, entity.IThreadSafeEntity(f.b)) || !slices.Contains(es, entity.IThreadSafeEntity(f.c)) {
			return nil, fmt.Errorf("multi lost or added explicit targets: %v", es)
		}
		for _, e := range es {
			switch x := e.(type) {
			case *unit:
				if x == f.a {
					addOne(x.state)
				}
			case *ledger:
				addOne(x.state)
			}
		}
		return nil, nil
	})
	peerWrite := f.on("write_via_peer", nest.DurabilityMemory, func(es []entity.IThreadSafeEntity, _ []any) (any, error) {
		if len(es) != 1 || es[0] != f.d {
			return nil, fmt.Errorf("peer write explicit targets: %v", es)
		}
		c, err := requireMember(f.c)
		if err != nil {
			return nil, err
		}
		addOne(c.state)
		return nil, nil
	})
	transition := f.on("churn", nest.DurabilityMemory, func(es []entity.IThreadSafeEntity, ps []any) (any, error) {
		if len(es) != 1 || es[0] != f.a {
			return nil, fmt.Errorf("churn explicit targets: %v", es)
		}
		leaving := ps[0].(bool)
		seen, err := visible(f.b)
		if err != nil {
			return nil, err
		}
		if seen != leaving {
			return nil, fmt.Errorf("before transition: see B=%t, leaving=%t", seen, leaving)
		}
		addOne(es[0].(*unit).state)
		if !leaving {
			return nil, nest.JoinEntityGroup(red)
		}
		if err := nest.LeaveEntityGroup(); err != nil {
			return nil, err
		}
		_, err = requireMember(f.b)
		return nil, err
	})
	f.start(t)
	f.group(t, red, f.a, f.b)
	f.group(t, blue, f.d, f.c)
	f.checkView(t, f.b, f.a, true)
	f.checkView(t, f.d, f.c, true)
	f.checkView(t, f.a, f.c, false)
	const rounds = 24
	clients := []func() error{
		func() error {
			_, err := f.n.RequestMulti(f.ctx, multi, []int64{f.a.ID(), f.b.ID(), f.c.ID()}, nil)
			return err
		},
		func() error {
			_, err := f.n.RequestMulti(f.ctx, multi, []int64{f.c.ID(), f.b.ID(), f.a.ID()}, nil)
			return err
		},
		func() error { _, err := f.n.Request(f.ctx, peerWrite, f.d.ID(), nil); return err },
		func() error {
			for _, leaving := range []bool{true, false} {
				if _, err := f.n.Request(f.ctx, transition, f.a.ID(), nest.Params{leaving}); err != nil {
					return err
				}
			}
			return nil
		},
	}
	ready, gate, done := make(chan struct{}, len(clients)), make(chan struct{}), make(chan error, len(clients))
	for i, run := range clients {
		go func() {
			ready <- struct{}{}
			<-gate
			for r := 0; r < rounds; r++ {
				if err := run(); err != nil {
					done <- fmt.Errorf("client %d, round %d: %w", i, r, err)
					return
				}
			}
			done <- nil
		}()
	}
	for range clients {
		<-ready
	}
	close(gate)
	for range clients {
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-f.ctx.Done():
			t.Fatal("concurrent requests failed to drain (possible deadlock):", f.ctx.Err())
		}
	}
	if t.Failed() {
		return
	}
	f.checkBalance(t, f.a, 4*rounds)
	f.checkBalance(t, f.c, 3*rounds)
	f.checkBalance(t, f.b, 0)
	f.checkBalance(t, f.d, 0)
	f.checkView(t, f.a, f.b, true)
	f.checkView(t, f.b, f.a, true)
	f.checkView(t, f.d, f.c, true)
	f.checkView(t, f.a, f.c, false)
}
