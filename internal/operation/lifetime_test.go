package operation

import (
	"sync"
	"testing"
)

func TestLifetimeStopDrainsOnlyAdmittedCalls(t *testing.T) {
	var lifetime Lifetime
	for range 8 {
		if !lifetime.Begin() {
			t.Fatal("admission refused")
		}
	}
	drained := lifetime.Stop()
	if lifetime.Begin() {
		t.Fatal("accepted after stop")
	}
	select {
	case <-drained:
		t.Fatal("premature drain")
	default:
	}
	var group sync.WaitGroup
	for range 8 {
		group.Go(lifetime.End)
	}
	group.Wait()
	<-drained
	if lifetime.Stop() != drained {
		t.Fatal("stop changed lifetime")
	}
}
