package nest

import (
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"testing"
)

func TestAsyncContinuationCapturesCurrentRuntimeGeneration(t *testing.T) {
	old := fctx.RuntimeConfig()
	t.Cleanup(func() { fctx.SetRuntimeConfig(old) })
	parent := fctx.ContextSnapshot{Valid: true, Config: "generation-one", Meta: fctx.RequestMeta{PlayerID: 42}}
	fctx.SetRuntimeConfig("generation-two")
	next := asyncMessageContextSnapshot(parent)
	fctx.SetRuntimeConfig("generation-three")
	later := asyncMessageContextSnapshot(next)
	if next.Config != "generation-two" || later.Config != "generation-three" || next.Meta.PlayerID != 42 {
		t.Fatalf("continuation pinned old snapshot: next=%+v later=%+v", next, later)
	}
}
