package cellworld

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

type recordedMeta map[nest.HandlerName]nest.HandlerMeta

func (r recordedMeta) RegisterHandlerWithMeta(n nest.HandlerName, _ nest.BaseHandler, m nest.HandlerMeta) error {
	r[n] = m
	return nil
}

// Check the production registrations, not just defaults or comments: the last
// compensation source may stop retrying on the death acknowledgement. Entry
// must likewise survive before its generation is announced outside this Base.
func TestCellworldDurabilityOfDeathEntryAndOrdinaryInput(t *testing.T) {
	got := recordedMeta{}
	if err := NewHandlers().Register(got); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[nest.HandlerName]nest.DurabilityPolicy{
		hDeath: nest.DurabilityStrict, hBaseEnter: nest.DurabilityStrict, hBaseAttackRequest: nest.DurabilityStrict,
		hInput: nest.DurabilityAsync, hBaseCmd: nest.DurabilityAsync,
	} {
		m, ok := got[name]
		if !ok || m.Rollback != nest.RollbackUndo || m.Durability != want || m.AsyncCompletion {
			t.Errorf("%s registration = %+v, present=%v, want undo/%v with ordinary completion", name.String(), m, ok, want)
		}
	}
}
