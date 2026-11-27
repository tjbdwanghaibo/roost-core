package decision

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestCompileRejectsInvalidShapeAndReferences(t *testing.T) {
	fixture := func() Definition {
		return base(Stage{Name: "idle", Tick: run(emit(1, 0, 0)), Events: []Listener{{ID: 2, Event: "hit"}}}, Stage{Name: "alt", Events: []Listener{{ID: 4, Event: "go"}}})
	}
	tc := map[string]func(*Definition){
		"schema": func(d *Definition) { d.Schema = "v2" }, "blank": func(d *Definition) { d.Name = " " }, "start": func(d *Definition) { d.Start = "missing" }, "stages": func(d *Definition) { d.Stages = nil }, "duplicate stage": func(d *Definition) { d.Stages[1].Name = "idle" }, "bad name": func(d *Definition) { d.Stages[0].Name = "idle/../alt" }, "listener event": func(d *Definition) { d.Stages[0].Events[0].Event = "external" }, "listener id": func(d *Definition) { d.Stages[0].Events[0].ID = 0 }, "reused listener": func(d *Definition) { d.Stages[1].Events[0].ID = 2 }, "global id": func(d *Definition) { d.Stages[0].Tick[0].Actions[0].ID = 2 }, "action id": func(d *Definition) { d.Stages[0].Tick[0].Actions[0].ID = -1 }, "duplicate action": func(d *Definition) { d.Stages[1].Tick = run(emit(1, 0, 0)) }, "unknown intent": func(d *Definition) { d.Stages[0].Tick[0].Actions[0].Name = "outside" }, "wrong stage": func(d *Definition) { d.Stages[0].Tick = run(Action{ID: 1, Kind: "stage", Name: "outside"}) }, "cross stage control": func(d *Definition) { d.Stages[0].Tick = run(Action{ID: 1, Kind: "open", Arg: 4}) }, "delay": func(d *Definition) { d.Stages[0].Tick[0].Actions[0].Delay = -1 }, "limit": func(d *Definition) { d.Stages[0].Tick[0].Actions[0].Limit = -1 }, "non emit limit": func(d *Definition) { d.Stages[0].Tick = run(Action{ID: 1, Kind: "stage", Name: "idle", Limit: 1}) }, "non emit delay": func(d *Definition) { d.Stages[0].Tick = run(Action{ID: 1, Kind: "open", Arg: 2, Delay: 1}) }, "stage arg": func(d *Definition) { d.Stages[0].Tick = run(Action{ID: 1, Kind: "stage", Name: "idle", Arg: 1}) }, "control name": func(d *Definition) { d.Stages[0].Tick = run(Action{ID: 1, Kind: "close", Arg: 2, Name: "idle"}) }, "unknown action": func(d *Definition) { d.Stages[0].Tick[0].Actions[0].Kind = "execute" }, "unknown node": func(d *Definition) { d.Stages[0].Tick[0].Kind = "execute" }, "unknown condition": func(d *Definition) { d.Stages[0].Events[0].Filter = &Test{"outside", "eq", 0} }, "unknown operator": func(d *Definition) { d.Stages[0].Events[0].Filter = &Test{"x", "effect", 0} }, "missing test": func(d *Definition) { d.Stages[0].Tick = []Node{{Kind: "check"}} }, "extra actions": func(d *Definition) {
			d.Stages[0].Tick = []Node{{Kind: "check", Test: &Test{"x", "eq", 0}, Actions: []Action{}}}
		}, "empty do": func(d *Definition) { d.Stages[0].Tick = []Node{{Kind: "do"}} }, "empty composition": func(d *Definition) { d.Stages[0].Tick = []Node{{Kind: "any"}} }, "extra test": func(d *Definition) { d.Stages[0].Tick[0].Kind = "all"; d.Stages[0].Tick[0].Test = &Test{"x", "eq", 0} }, "extra children": func(d *Definition) { d.Stages[0].Tick[0].Children = []Node{} },
	}
	for name, mutate := range tc {
		t.Run(name, func(t *testing.T) {
			d := fixture()
			mutate(&d)
			if p, err := Compile(d, registry()); p != nil || err == nil {
				t.Fatal("accepted invalid definition")
			}
		})
	}
	for _, r := range []Registry{{Events: []string{"hit", "hit"}}, {Intents: []string{"mark", ""}}, {Fields: []string{strings.Repeat("x", MaxNameBytes+1)}}, {Fields: []string{".field"}}} {
		if p, err := Compile(base(Stage{Name: "idle"}), r); p != nil || err == nil {
			t.Fatal("accepted incorrect registry")
		}
	}
}

func TestCompileBoundariesBeforeClone(t *testing.T) {
	check := Node{Kind: "check", Test: &Test{"x", "eq", 0}}
	d := base(Stage{Name: "idle"})
	deep := check
	for i := 1; i < MaxDepth; i++ {
		deep = Node{Kind: "all", Children: []Node{deep}}
	}
	d.Stages[0].Tick = []Node{deep}
	program(t, d)
	d.Stages[0].Tick = []Node{{Kind: "any", Children: []Node{deep}}}
	if p, err := Compile(d, registry()); p != nil || err == nil {
		t.Fatal("depth")
	}
	cycle := []Node{{Kind: "all"}}
	cycle[0].Children = cycle
	d.Stages[0].Tick = cycle
	if p, err := Compile(d, registry()); p != nil || err == nil {
		t.Fatal("cycle")
	}
	d.Stages[0].Tick = slices.Repeat([]Node{check}, MaxItems-1)
	p := program(t, d)
	raw, _ := json.Marshal(d)
	decoded, err := Decode(raw, registry())
	if err != nil || decoded.plan != p.plan {
		t.Fatal("boundary wire decode:", err)
	}
	d.Stages[0].Tick = append(d.Stages[0].Tick, check)
	if p, err := Compile(d, registry()); p != nil || err == nil {
		t.Fatal("size")
	}
	d = base(Stage{Name: "idle"})
	for i := 1; i < MaxStages; i++ {
		d.Stages = append(d.Stages, Stage{Name: fmt.Sprintf("s%d", i)})
	}
	program(t, d)
	d.Stages = append(d.Stages, Stage{Name: "extra"})
	if p, err := Compile(d, registry()); p != nil || err == nil {
		t.Fatal("stages")
	}
	d = base(Stage{Name: "idle"})
	for i := 0; i < MaxListenersPerStage; i++ {
		d.Stages[0].Events = append(d.Stages[0].Events, Listener{ID: int64(i + 1), Event: "hit"})
	}
	program(t, d)
	d.Stages[0].Events = append(d.Stages[0].Events, Listener{ID: 9999, Event: "hit"})
	if p, err := Compile(d, registry()); p != nil || err == nil {
		t.Fatal("listeners")
	}
	r := Registry{}
	for i := 0; i < MaxRegistryNames; i++ {
		r.Fields = append(r.Fields, fmt.Sprintf("f%d", i))
	}
	d = base(Stage{Name: "idle"})
	d.Name = strings.Repeat("n", MaxNameBytes)
	if _, err := Compile(d, r); err != nil {
		t.Fatal(err)
	}
	r.Fields = append(r.Fields, "extra")
	if p, err := Compile(d, r); p != nil || err == nil {
		t.Fatal("registry")
	}
	d.Name += "n"
	if p, err := Compile(d, registry()); p != nil || err == nil {
		t.Fatal("name size")
	}
}

func TestPlanIdentityIncludesSemanticsAndRegistryButNotRegistryOrder(t *testing.T) {
	d := base(Stage{Name: "idle", Tick: run(emit(1, 0, 1), emit(2, 2, 1))})
	r := registry()
	p := program(t, d)
	s := p.NewState()
	slices.Reverse(r.Events)
	q, err := Compile(d, r)
	if err != nil || q.ValidateState(s) != nil {
		t.Fatal("same allowlist different order")
	}
	raw, _ := json.Marshal(d)
	q, err = Decode(raw, r)
	if err != nil || q.ValidateState(s) != nil {
		t.Fatal("native JSON not equivalent")
	}
	r.Fields = append(r.Fields, "new")
	q, err = Compile(d, r)
	if err != nil || q.ValidateState(s) == nil {
		t.Fatal("registry changed")
	}
	slices.Reverse(d.Stages[0].Tick[0].Actions)
	q = program(t, d)
	if q.ValidateState(s) == nil {
		t.Fatal("order changed")
	}
	slices.Reverse(d.Stages[0].Tick[0].Actions)
	d.Stages[0].Tick[0].Actions[0].Arg++
	q = program(t, d)
	if q.ValidateState(s) == nil {
		t.Fatal("payload changed")
	}
	rejected(t, q, s, Input{})
}

func TestStrictDefinitionJSON(t *testing.T) {
	wrap := func(n string) string {
		return `{"schema":"` + Schema + `","name":"n","start":"idle","stages":[{"name":"idle","tick":[` + n + `]}]}`
	}
	node := `{"kind":"do","actions":[{"id":1,"kind":"emit","name":"mark"}]}`
	raw := wrap(node)
	for _, n := range []string{raw, " \n" + raw + "\r\n", wrap(`{"kind":"check","test":{"field":"x","op":"eq","value":0}}`)} {
		if _, err := Decode([]byte(n), registry()); err != nil {
			t.Fatal(err)
		}
	}
	for name, bad := range map[string]string{"trailing": raw + ` {}`, "junk": raw + `-`, "duplicate": strings.Replace(raw, `"id":1`, `"id":9,"\u0069d":1`, 1), "case": strings.Replace(raw, `"name":"n"`, `"Name":"n"`, 1), "unknown": strings.Replace(raw, `"actions":`, `"callback":`, 1), "missing value": wrap(`{"kind":"check","test":{"field":"x","op":"eq"}}`), "null": strings.Replace(raw, `"id":1`, `"id":null`, 1), "fraction": strings.Replace(raw, `"id":1`, `"id":1.0`, 1), "exponent": strings.Replace(raw, `"id":1`, `"id":1e0`, 1), "overflow": strings.Replace(raw, `"id":1`, `"id":9223372036854775808`, 1), "negative overflow": strings.Replace(raw, `"id":1`, `"id":-9223372036854775809`, 1), "wrong array": wrap(`{"kind":"do","actions":{}}`), "zero unrelated stage": wrap(`{"kind":"do","actions":[{"id":1,"kind":"stage","name":"idle","arg":0}]}`), "zero unrelated limit": wrap(`{"kind":"do","actions":[{"id":1,"kind":"stage","name":"idle","limit":0}]}`), "empty unrelated array": wrap(`{"kind":"check","test":{"field":"x","op":"eq","value":0},"children":[]}`), "bad UTF-8": strings.Replace(raw, "mark", "ma\xffrk", 1), "UTF-16 lone surrogate": strings.Replace(raw, "mark", `ma\ud800rk`, 1), "missing root": `{}`, "oversize": raw + strings.Repeat(" ", MaxJSONBytes), "very deep": wrap(strings.Repeat(`{"kind":"all","children":[`, 1000) + node + strings.Repeat(`]}`, 1000))} {
		t.Run(name, func(t *testing.T) {
			if p, err := Decode([]byte(bad), registry()); err == nil || p != nil {
				t.Fatal("accepted invalid JSON")
			}
		})
	}
}

func TestRestoreRejectsCorruption(t *testing.T) {
	p := program(t, base(Stage{Name: "idle", Tick: run(emit(3, 4, 2), emit(4, 0, 1)), Events: []Listener{{ID: 1, Event: "hit", Enabled: true}, {ID: 2, Event: "go"}}}, Stage{Name: "isolated"}))
	good := checked(t, p, p.NewState(), Input{Now: 6}, 4)
	raw, _ := json.Marshal(good)
	for name, change := range map[string]func(*State){"plan": func(s *State) { s.Plan = "foreign" }, "unknown stage": func(s *State) { s.Stage = "gone" }, "unreachable stage": func(s *State) { s.Stage = "isolated"; s.Active = nil }, "negative time": func(s *State) { s.Now = -1 }, "tick ahead": func(s *State) { s.LastTick = s.Now + 1 }, "tick below sentinel": func(s *State) { s.LastTick = -2 }, "wrong subscription size": func(s *State) { s.Active = nil }, "closed fixed": func(s *State) { s.Active[0] = false }, "opened fixed": func(s *State) { s.Active[1] = true }, "short counts": func(s *State) { s.Counts = s.Counts[:1] }, "negative count": func(s *State) { s.Counts[0] = -1 }, "past limit": func(s *State) { s.Counts[1] = 2 }, "count before first tick": func(s *State) { s.LastTick = -1; s.Pending = nil }, "count too young": func(s *State) { s.Now = 1; s.LastTick = 1; s.Pending = nil; s.Counts[0] = 1 }, "pending count cap": func(s *State) { s.Now = 20; s.LastTick = 20; s.Pending[0].Due = 24; s.Counts[0] = 2 }, "unknown pending id": func(s *State) { s.Pending[0].NodeID = 99 }, "immediate id": func(s *State) { s.Pending[0].NodeID = 4 }, "duplicate pending": func(s *State) { s.Pending = append(s.Pending, s.Pending[0]) }, "generated in future": func(s *State) { s.Pending[0].Due = 11 }, "generated before zero": func(s *State) { s.LastTick = 0; s.Pending[0].Due = 2 }, "overdue after tick": func(s *State) { s.Pending[0].Due = s.LastTick }, "negative deadline": func(s *State) { s.Pending[0].Due = -1 }, "tick origin after last tick": func(s *State) { s.Now = 8; s.LastTick = 4; s.Pending[0].Due = 12 }} {
		t.Run(name, func(t *testing.T) {
			s, err := p.DecodeState(raw)
			if err != nil {
				t.Fatal(err)
			}
			change(&s)
			if err = p.ValidateState(s); err == nil {
				t.Fatal("accepted corrupted snapshot", s)
			}
			rejected(t, p, s, Input{Now: 20})
			b, _ := json.Marshal(s)
			s, err = p.DecodeState(b)
			if err == nil || !reflect.DeepEqual(s, State{}) {
				t.Fatal("restored invalid JSON state")
			}
		})
	}
	for _, bad := range [][]byte{append(bytes.Clone(raw), '0'), bytes.Replace(raw, []byte(`"now":6`), []byte(`"now":6,"now":6`), 1), bytes.Replace(raw, []byte(`"now":6,`), nil, 1), bytes.Replace(raw, []byte(`"now":`), []byte(`"Now":`), 1), bytes.Replace(raw, []byte(`"now":6`), []byte(`"now":6e0`), 1), bytes.Replace(raw, []byte(`"node_id":3,`), nil, 1), bytes.Replace(raw, []byte(`"due":10`), []byte(`"due":10,"payload":1`), 1), []byte(`null`), bytes.Repeat([]byte{' '}, MaxJSONBytes+1)} {
		if _, err := p.DecodeState(bad); err == nil {
			t.Fatalf("accepted malformed JSON state: %.100s", bad)
		}
		var dest = good
		if err := json.Unmarshal(bad, &dest); err == nil || !reflect.DeepEqual(dest, good) {
			t.Fatal("failed explicit unmarshal mutated destination")
		}
	}
}

func TestRestoreOptionalEmptySlices(t *testing.T) {
	p := program(t, base(Stage{Name: "idle"}))
	s := p.NewState()
	for _, v := range []State{s, {Plan: s.Plan, Stage: s.Stage, LastTick: -1, Active: []bool{}, Counts: []int64{}, Pending: []Pending{}}} {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		got, err := p.DecodeState(raw)
		if err != nil || !reflect.DeepEqual(got, s) {
			t.Fatal(got, err)
		}
		explicit := bytes.Replace(raw, []byte("}"), []byte(`,"active":[],"counts":[],"pending":[]}`), 1)
		got, err = p.DecodeState(explicit)
		if err != nil || len(got.Active)+len(got.Counts)+len(got.Pending) != 0 {
			t.Fatal(got, err)
		}
	}
}

func FuzzNativeWire(f *testing.F) {
	seed, _ := json.Marshal(base(Stage{Name: "idle", Tick: run(emit(1, 2, 1))}))
	f.Add(seed)
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		p, err := Decode(raw, registry())
		if err != nil {
			if p != nil {
				t.Fatal("partial compile")
			}
			return
		}
		s := p.NewState()
		if err := p.ValidateState(s); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(s)
		if _, err = p.DecodeState(b); err != nil {
			t.Fatal(err)
		}
	})
}
