package peergroupcontract_test

import (
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"testing"
)

// Two distinct same-kind subjects enter via both async APIs. Read the final
// value with a real Request after the callbacks ran; an uncommitted mutation
// or a duplicate replay must not satisfy the final exact count.
func TestPeerGroupBroadcastAndTryBatchUseCompleteTransaction(t *testing.T) {
	f := newWorld(t).engine()
	result := make(chan error, 8)
	h := f.on("fanout", nest.DurabilityMemory, func(es []entity.IThreadSafeEntity, _ []any) (any, error) {
		if len(es) != 1 {
			err := fmt.Errorf("expected one subject, got %d", len(es))
			result <- err
			return nil, err
		}
		v, err := requireMember(f.c)
		if err == nil {
			v.balance++
		}
		result <- err
		return nil, err
	})
	f.start(t)
	f.group(t, red, f.a, f.b, f.c)
	if err := f.n.DispatchBroadcast(f.ctx, h, []int64{f.a.ID(), f.b.ID()}, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.n.DispatchTryBatch(f.ctx, []nest.TryBatchTarget{{Handler: h, ID: f.b.ID()}, {Handler: h, ID: f.a.ID()}}, func(_ nest.TryBatchTarget, err error) { result <- fmt.Errorf("not executed: %w", err) }); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		select {
		case err := <-result:
			if err != nil {
				t.Fatal(err)
			}
		case <-f.ctx.Done():
			t.Fatal("incomplete fan-out", f.ctx.Err())
		}
	}
	f.checkBalance(t, f.c, 4)
}
