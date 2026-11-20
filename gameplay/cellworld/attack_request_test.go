package cellworld

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	fentity "github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

type attackTestLedger struct {
	high  int64
	slots [AttackRequestWindow]AttackReceipt
}

type numberedTestPlayer struct {
	*testPlayer
	ledger undoCell[attackTestLedger]
}

func (p *numberedTestPlayer) CellAttackSlot(slot int64) (int64, AttackReceipt) {
	return p.ledger.v.high, p.ledger.v.slots[slot]
}
func (p *numberedTestPlayer) SetCellAttackSlot(slot, high int64, receipt AttackReceipt) {
	s := p.ledger.v
	s.high, s.slots[slot] = high, receipt
	p.ledger.set(s)
}

// No watchdog needed: both cells and the Base stay up throughout this test.
func (*numberedTestPlayer) StartBase() error { return nil }

func TestNumberedAttackReplayAfterUnknownAndTargetDedupExpiry(t *testing.T) {
	spec, lb := loopbackSpec(t, 2, 1)
	const window = 600 * time.Millisecond
	spec.cellMod = func(cfg *NodeConfig) {
		cfg.AttackRetry, cfg.AttackDeadline, cfg.DedupWindow = 60*time.Millisecond, 400*time.Millisecond, window
	}
	var issued atomic.Int32
	lb.setFault(func(_ string, m *wireMsg) (bool, bool, time.Duration) {
		if m.Kind == kindAttack {
			issued.Add(1)
		}
		return m.Kind == kindDamageReply, false, 0
	})
	w := startWorld(t, spec)
	// Old implementations do not acquire an in-memory fallback ledger.
	old := w.base.player(2)
	if _, err := w.base.node.AttackWithRequest(context.Background(), old.ID(), npcID(11), 7, 1); !errors.Is(err, ErrAttackLedgerMissing) {
		t.Fatalf("missing durable ledger: %v", err)
	}
	p := &numberedTestPlayer{testPlayer: &testPlayer{EntityBase: fentity.NewEntityBase(playerEntityID(1), fentity.EntityCategoryPlayer, true, kindTPlayer), dao: &undoCell[BaseState]{}, group: &undoCell[Group]{}}}
	w.base.manager.Add(p)
	if err := w.base.sync.OpenSession(1); err != nil {
		t.Fatal(err)
	}
	target := npcID(11)
	a, b := CellID{0, 0}, CellID{1, 0}
	w.spawn(b, EntityState{ID: target, X: 10300, Y: 5000, HP: 100})
	if err := w.base.node.Enter(context.Background(), p.ID(), actorID(1), spatial.Point{X: 9700, Y: 5000}, 100); err != nil {
		t.Fatal(err)
	}
	w.waitView(1, settleWithin, "first view", func(v View) bool { return v.Cell == w.grid.SID(a) })
	w.waitKnown(a, target)
	attack := func() HitID {
		t.Helper()
		hit, err := w.base.node.AttackWithRequest(context.Background(), p.ID(), target, 7, 1)
		if err != nil {
			t.Fatal(err)
		}
		return hit
	}
	first := attack()
	results := w.waitResults(1, settleWithin)
	if len(results) != 1 || results[0].Outcome != OutcomeUnknown || results[0].Hit != first || w.applications()[first] != 1 {
		t.Fatalf("expected applied damage but unknown verdict: %+v %+v", results, w.applications())
	}
	time.Sleep(window) // target's applied-hit entry has now aged out; even the same HitID could damage it again
	if got := attack(); got != first {
		t.Fatalf("replay=%+v first=%+v", got, first)
	}
	ctx, cancel := context.WithTimeout(context.Background(), settleWithin)
	defer cancel()
	if err := w.base.node.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if issued.Load() != 1 {
		t.Fatalf("Base issued %d attacks", issued.Load())
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.results) != 1 || w.results[0].Outcome != OutcomeUnknown || w.hits[first] != 1 {
		t.Fatalf("replay changed an unknown verdict or reapplied damage: %+v hits=%+v", w.results, w.hits)
	}
}
