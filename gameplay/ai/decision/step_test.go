package decision

import (
	"encoding/json"
	"math"
	"reflect"
	"slices"
	"sync"
	"testing"
)

func base(stages ...Stage) Definition {
	return Definition{Schema: Schema, Name: "sample", Start: "idle", Stages: stages}
}
func registry() Registry {
	return Registry{Events: []string{"hit", "go", "back"}, Fields: []string{"x", "unused"}, Intents: []string{"mark"}}
}
func emit(id, delay, limit int64) Action {
	return Action{ID: id, Kind: "emit", Name: "mark", Arg: -id, Delay: delay, Limit: limit}
}
func run(actions ...Action) []Node { return []Node{{Kind: "do", Actions: actions}} }
func program(t *testing.T, d Definition) *Program {
	t.Helper()
	p, err := Compile(d, registry())
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func checked(t *testing.T, p *Program, s State, in Input, want ...int64) State {
	t.Helper()
	before, _ := json.Marshal(s)
	n, out, err := p.Step(s, in)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(s)
	if string(before) != string(after) {
		t.Fatal("Step mutated its input")
	}
	var ids []int64
	for _, a := range out {
		if a.Name != "mark" || a.Arg != -a.NodeID {
			t.Fatalf("bad intent payload: %+v", a)
		}
		ids = append(ids, a.NodeID)
	}
	if !slices.Equal(ids, want) {
		t.Fatalf("input %+v: emitted %v, want %v", in, ids, want)
	}
	if err = p.ValidateState(n); err != nil {
		t.Fatal("produced invalid state:", err, n)
	}
	raw, _ := json.Marshal(n)
	restored, err := p.DecodeState(raw)
	if err != nil || !reflect.DeepEqual(n, restored) {
		t.Fatalf("round trip: %s => %+v, %v", raw, restored, err)
	}
	return n
}
func rejected(t *testing.T, p *Program, s State, in Input) {
	t.Helper()
	before, _ := json.Marshal(s)
	next, out, err := p.Step(s, in)
	after, _ := json.Marshal(s)
	if err == nil || out != nil || !reflect.DeepEqual(next, State{}) || string(before) != string(after) {
		t.Fatalf("failed transition was not atomic: %+v, %+v, %v", next, out, err)
	}
}

func TestEventSnapshotAndDeferredControlOrder(t *testing.T) {
	p := program(t, base(Stage{Name: "idle", Events: []Listener{
		{ID: 1, Event: "hit", Enabled: true, Once: true, Filter: &Test{"x", "gt", 0}, Body: run(emit(11, 0, 0))},
		{ID: 2, Event: "hit", Enabled: true, Body: run(emit(12, 0, 0), Action{ID: 20, Kind: "close", Arg: 3}, Action{ID: 21, Kind: "open", Arg: 4}, Action{ID: 22, Kind: "stage", Name: "idle"})},
		{ID: 3, Event: "hit", Enabled: true, Body: run(emit(13, 0, 0))},
		{ID: 4, Event: "hit", Body: run(emit(14, 0, 0))},
		{ID: 5, Event: "go", Enabled: true, Body: run(Action{ID: 23, Kind: "stage", Name: "alt"}, Action{ID: 24, Kind: "open", Arg: 3})},
		{ID: 6, Event: "go", Enabled: true, Body: run(emit(15, 0, 0))},
	}}, Stage{Name: "alt", Tick: run(emit(16, 0, 0)), Events: []Listener{{ID: 7, Event: "back", Enabled: true, Body: run(Action{ID: 25, Kind: "stage", Name: "idle"})}}}))
	s := p.NewState()
	rejected(t, p, s, Input{Event: "hit"}) // 缺字段是错误，不能用隐式零消耗 once。
	s = checked(t, p, s, Input{Event: "hit", Fields: map[string]int64{"x": 0}}, 12, 13)
	if !slices.Equal(s.Active, []bool{false, true, false, true, true, true}) {
		t.Fatal(s.Active)
	}
	s = checked(t, p, s, Input{Event: "hit", Fields: map[string]int64{"x": 9}}, 12, 14)
	s = checked(t, p, s, Input{Event: "go"}, 15)
	if s.Stage != "alt" || len(s.Active) != 1 || s.LastTick != -1 {
		t.Fatal(s)
	}
	s = checked(t, p, s, Input{}, 16)
	s = checked(t, p, s, Input{Event: "back"})
	if !slices.Equal(s.Active, p.NewState().Active) {
		t.Fatal("re-entry did not reset subscriptions")
	}
	checked(t, p, s, Input{Event: "hit", Fields: map[string]int64{"x": 9}}, 11, 12, 13)
}

func TestOnceCloseIsQueuedAfterItsBody(t *testing.T) {
	p := program(t, base(Stage{Name: "idle", Events: []Listener{{ID: 1, Event: "hit", Enabled: true, Once: true, Body: run(Action{ID: 2, Kind: "open", Arg: 1}, emit(3, 0, 0))}, {ID: 4, Event: "hit", Enabled: true, Body: run(Action{ID: 5, Kind: "open", Arg: 1})}}}))
	s := checked(t, p, p.NewState(), Input{Event: "hit"}, 3)
	if !s.Active[0] {
		t.Fatal("later listener's open should follow once's close")
	}
	checked(t, p, s, Input{Event: "hit"}, 3)
}

func TestTickOrderDeadlineDedupAndLifetimeAcrossStages(t *testing.T) {
	p := program(t, base(Stage{Name: "idle", Tick: run(emit(11, 5, 2), emit(12, 2, 1), emit(13, 5, 1), emit(14, 0, 2)), Events: []Listener{{ID: 1, Event: "go", Enabled: true, Body: run(Action{ID: 2, Kind: "stage", Name: "alt"})}}}, Stage{Name: "alt", Tick: run(emit(20, 0, 0)), Events: []Listener{{ID: 3, Event: "back", Enabled: true, Body: run(Action{ID: 4, Kind: "stage", Name: "idle"})}}}))
	s := checked(t, p, p.NewState(), Input{}, 14)
	s = checked(t, p, s, Input{Now: 1}, 14)
	if !slices.Equal(s.Pending, []Pending{{11, 5}, {12, 2}, {13, 5}}) || !slices.Equal(s.Counts, []int64{0, 0, 0, 2}) {
		t.Fatal(s)
	}
	s = checked(t, p, s, Input{Now: 2, Event: "go"})
	s = checked(t, p, s, Input{Now: 6, Event: "hit"})
	if s.LastTick != 1 {
		t.Fatal(s)
	}
	rejected(t, p, s, Input{Now: 5})
	s = checked(t, p, s, Input{Now: 6}, 20, 12, 11, 13)
	s = checked(t, p, s, Input{Now: 6, Event: "back"})
	s = checked(t, p, s, Input{Now: 6})
	s = checked(t, p, s, Input{Now: 11}, 11)
	checked(t, p, s, Input{Now: math.MaxInt64})
}

func TestTickCannotRearmDueActionUntilNextInput(t *testing.T) {
	p := program(t, base(Stage{Name: "idle", Tick: run(emit(1, 1, 0), emit(2, 0, 0))}))
	s := checked(t, p, p.NewState(), Input{}, 2)
	s = checked(t, p, s, Input{Now: 1}, 2, 1)
	if len(s.Pending) != 0 {
		t.Fatal(s.Pending)
	}
	s = checked(t, p, s, Input{Now: 1}, 2)
	if !slices.Equal(s.Pending, []Pending{{1, 2}}) {
		t.Fatal(s.Pending)
	}
}

func TestCompositionOrderedShortCircuitAndEveryRoot(t *testing.T) {
	ck := func(f string, v int64) Node { return Node{Kind: "check", Test: &Test{f, "eq", v}} }
	p := program(t, base(Stage{Name: "idle", Tick: []Node{
		{Kind: "all", Children: []Node{ck("x", 0), ck("unused", 0)}, Actions: []Action{emit(1, 0, 0)}},
		{Kind: "any", Children: []Node{ck("x", 1), ck("unused", 0)}, Actions: []Action{emit(2, 0, 0)}},
		{Kind: "any", Actions: []Action{emit(3, 0, 0)}}, {Kind: "all", Actions: []Action{emit(4, 0, 0)}},
		{Kind: "all", Children: []Node{run(emit(5, 0, 0))[0], ck("x", 0)}, Actions: []Action{emit(6, 0, 0)}},
		run(emit(7, 0, 0))[0],
	}}))
	checked(t, p, p.NewState(), Input{Fields: map[string]int64{"x": 1}}, 2, 4, 5, 7)
}
func TestComparisonEdges(t *testing.T) {
	for _, tc := range []struct {
		op   string
		x, y int64
		yes  bool
	}{{"eq", 0, 0, true}, {"ne", math.MinInt64, math.MaxInt64, true}, {"lt", math.MinInt64, math.MaxInt64, true}, {"le", 0, 0, true}, {"ge", 0, 0, true}, {"gt", math.MinInt64, math.MaxInt64, false}} {
		t.Run(tc.op, func(t *testing.T) {
			p := program(t, base(Stage{Name: "idle", Tick: []Node{{Kind: "all", Children: []Node{{Kind: "check", Test: &Test{"x", tc.op, tc.y}}}, Actions: []Action{emit(1, 0, 0)}}}}))
			var want []int64
			if tc.yes {
				want = []int64{1}
			}
			checked(t, p, p.NewState(), Input{Fields: map[string]int64{"x": tc.x}}, want...)
		})
	}
}

func TestOverflowAndErrorsDiscardPartialResults(t *testing.T) {
	p := program(t, base(Stage{Name: "idle", Tick: run(emit(1, 0, math.MaxInt64), emit(2, 3, 1))}))
	s := p.NewState()
	rejected(t, p, s, Input{Now: math.MaxInt64 - 2})
	s = checked(t, p, s, Input{Now: math.MaxInt64 - 3}, 1)
	s.Counts[0] = math.MaxInt64 - 1
	s = checked(t, p, s, Input{Now: math.MaxInt64}, 1, 2)
	checked(t, p, s, Input{Now: math.MaxInt64})
	p = program(t, base(Stage{Name: "idle", Tick: append(run(emit(1, 0, 1), emit(2, 2, 1)), Node{Kind: "check", Test: &Test{"x", "eq", 0}})}))
	s = p.NewState()
	rejected(t, p, s, Input{})
	rejected(t, p, s, Input{Now: -1})
	rejected(t, p, s, Input{Event: "unknown"})
	rejected(t, p, s, Input{Fields: map[string]int64{"other": 0}})
}

func TestProgramIsolationConcurrentReplay(t *testing.T) {
	reg := registry()
	d := base(Stage{Name: "idle", Events: []Listener{{ID: 1, Event: "hit", Enabled: true, Once: true, Filter: &Test{"x", "eq", 2}, Body: run(emit(2, 0, 1), emit(3, 1, 1))}}})
	p, err := Compile(d, reg)
	if err != nil {
		t.Fatal(err)
	}
	reg.Fields[0] = "bad"
	d.Stages[0].Events[0].Filter.Value = 99
	d.Stages[0].Events[0].Body[0].Actions[0].Arg = 88
	d.Stages[0].Events[0].Enabled = false
	s := p.NewState()
	other := p.NewState()
	other.Active[0] = false
	other.Counts[0] = 9
	in := Input{Now: 7, Event: "hit", Fields: map[string]int64{"x": 2}}
	var wg sync.WaitGroup
	for range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 8 {
				n, out, err := p.Step(s, in)
				if err != nil || !slices.Equal(out, []Intent{{2, "mark", -2}}) || !slices.Equal(n.Pending, []Pending{{3, 8}}) || n.Active[0] {
					t.Errorf("non-deterministic step: %+v, %+v, %v", n, out, err)
					return
				}
				n.Active[0] = true
				n.Counts[0] = 100
				n.Pending[0].Due = 0
				out[0].Arg = 14
			}
		}()
	}
	wg.Wait()
	if in.Fields["x"] != 2 || !s.Active[0] || s.Counts[0] != 0 || s.Pending != nil {
		t.Fatal("state/fields aliased")
	}
}
