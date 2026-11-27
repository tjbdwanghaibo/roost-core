package ai_test

import (
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/gameplay/ai"
	"github.com/tjbdwanghaibo/roost-core/gameplay/ai/decision"
)

func decisionPlan(t *testing.T, name string) *decision.Program {
	t.Helper()
	emit := func(id int64) []decision.Node {
		return []decision.Node{{Kind: "do", Actions: []decision.Action{{ID: id, Kind: "emit", Name: "signal", Arg: id, Limit: 1}}}}
	}
	p, err := decision.Compile(decision.Definition{Schema: decision.Schema, Name: name, Start: "ready", Stages: []decision.Stage{{Name: "ready", Tick: emit(1), Events: []decision.Listener{{ID: 7, Event: "hit", Enabled: true, Once: true, Body: emit(2)}}}}}, decision.Registry{Events: []string{"hit"}, Intents: []string{"signal"}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type decisionStore struct {
	version int64
	state   *decision.State
	out     []int64
	err     error
	reject  bool
	confuse bool
}

func (s *decisionStore) host() ai.DecisionHost {
	return ai.DecisionHost{
		Load: func(*ai.Context) (int64, *decision.State, error) { return s.version, s.state, nil },
		Input: func(_ *ai.Context, now time.Time, ev *ai.Event) (decision.Input, error) {
			in := decision.Input{Now: now.Unix()}
			if ev != nil && !s.confuse {
				if ev.Kind != 17 {
					return in, errors.New("unknown event")
				}
				in.Event = "hit"
			}
			return in, nil
		},
		Commit: func(_ *ai.Context, expected int64, next decision.State, out []decision.Intent) error {
			if s.reject || s.version != expected {
				return errors.New("CAS rejected")
			}
			for _, intent := range out {
				s.out = append(s.out, intent.Arg)
			}
			s.version++
			s.state = &next
			return nil
		},
		OnError: func(err error) { s.err = err },
	}
}
func TestDecisionStrategyCommitsBothOrNeither(t *testing.T) {
	p := decisionPlan(t, "camp")
	now := time.Unix(10, 0)
	ctl := ai.NewController(ai.ControllerHooks{Now: func() time.Time { return now }})
	store := new(decisionStore)
	s, err := ai.NewDecisionStrategy("choose", p, store.host())
	if err != nil {
		t.Fatal(err)
	}
	if err = ctl.SetStrategy(s); err != nil || store.version != 0 {
		t.Fatalf("Init wrote or failed: %d %v", store.version, err)
	}
	store.reject = true
	ctl.Tick(now)
	if store.err == nil || store.state != nil || len(store.out) != 0 {
		t.Fatal("rejection advanced state or effects")
	}
	store.reject = false
	store.err = nil
	ctl.Tick(now)
	if !ctl.OnEvent(ai.Event{Kind: 17}) || store.err != nil || store.version != 2 || len(store.out) != 2 || store.out[0] != 1 || store.out[1] != 2 {
		t.Fatalf("same-tick ordered tick/event: %+v", store)
	}
	ctl.Freeze()
	ctl.Tick(now)
	if ctl.OnEvent(ai.Event{Kind: 17}) || store.version != 2 {
		t.Fatal("freeze changed selector")
	}
	ctl.Recover()
	s2, _ := ai.NewDecisionStrategy("resume", p, store.host())
	if err := ctl.SetStrategy(s2); err != nil {
		t.Fatal(err)
	}
	ctl.Tick(now)
	ctl.OnEvent(ai.Event{Kind: 17})
	if store.version != 4 || len(store.out) != 2 {
		t.Fatalf("resume repeated limited/one-shot effects: %+v", store)
	}
	foreign, _ := ai.NewDecisionStrategy("different", decisionPlan(t, "different"), store.host())
	if err := ctl.SetStrategy(foreign); !errors.Is(err, ai.ErrStrategyInit) || ctl.Strategy() != s2 || store.version != 4 {
		t.Fatalf("wrong-plan Init replaced/wrote: %v", err)
	}
	store.confuse = true
	ctl.OnEvent(ai.Event{Kind: 17})
	if store.err == nil || store.version != 4 {
		t.Fatal("event converted to tick")
	}
}
func TestDecisionStrategyRejectsInvalidHostAndAmbiguousSnapshots(t *testing.T) {
	p := decisionPlan(t, "camp")
	s := new(decisionStore)
	host := s.host()
	host.OnError = nil
	if _, err := ai.NewDecisionStrategy("selector", p, host); err == nil {
		t.Fatal("missing error sink accepted")
	}
	host = s.host()
	host.Commit = nil
	if _, err := ai.NewDecisionStrategy("selector", p, host); err == nil {
		t.Fatal("missing output sink accepted")
	}
	adapter, err := ai.NewDecisionStrategy("selector", p, s.host())
	if err != nil {
		t.Fatal(err)
	}
	s.version = 1
	if adapter.Init(nil) == nil {
		t.Fatal("nonzero version treated as absent")
	}
	state := p.NewState()
	s.state = &state
	s.version = 0
	if adapter.Init(nil) == nil {
		t.Fatal("existing state treated as absent")
	}
}
