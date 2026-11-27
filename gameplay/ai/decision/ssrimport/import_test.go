package ssrimport_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/gameplay/ai/decision"
	"github.com/tjbdwanghaibo/roost-core/gameplay/ai/decision/ssrimport"
)

// Synthetic worker semantics only; these numbers are not a production table.
func binding() ssrimport.Bindings {
	return ssrimport.Bindings{
		Events:     map[uint32]ssrimport.EventBinding{11: {Name: "pulse", Field: "value"}, 12: {Name: "advance"}, 13: {Name: "reset"}},
		Conditions: map[uint32]string{21: "gate"},
		Actions:    map[uint32]ssrimport.ActionBinding{31: {Kind: "emit", Name: "mark"}, 32: {Kind: "stage"}},
	}
}
func registry() decision.Registry {
	return decision.Registry{Events: []string{"pulse", "advance", "reset"}, Fields: []string{"gate", "value"}, Intents: []string{"mark"}}
}
func source() ssrimport.Source {
	return ssrimport.Source{Dialect: ssrimport.AIEngine, Name: "synthetic", Start: "alpha"}
}
func read(t *testing.T, file string) []byte {
	t.Helper()
	b, e := os.ReadFile("testdata/" + file)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func decode(t *testing.T, file string, dst any) {
	t.Helper()
	if err := json.Unmarshal(read(t, file), dst); err != nil {
		t.Fatal(err)
	}
}

type effect struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Arg  int64  `json:"arg"`
}
type row struct {
	Stage   string   `json:"stage"`
	Active  []int64  `json:"active"`
	Effects []effect `json:"effects"`
}

func TestImportedProgramMatchesSourceTrace(t *testing.T) {
	p, err := ssrimport.Import(read(t, "synthetic.editor.json"), source(), binding(), registry())
	if err != nil {
		t.Fatal(err)
	}
	var inputs []decision.Input
	decode(t, "inputs.json", &inputs)
	var trace []row
	decode(t, "source-trace.json", &trace)
	if len(inputs) != 10 || len(inputs) != len(trace) {
		t.Fatal("incomplete oracle trace")
	}
	ids := map[string][]int64{"alpha": {8, 9}, "beta": {16}} // Native State.Active is in configured listener order.
	s := p.NewState()
	for i, in := range inputs {
		next, intents, err := p.Step(s, in)
		if err != nil {
			t.Fatalf("input %d: %v", i, err)
		}
		if err = p.ValidateState(next); err != nil {
			t.Fatal(err)
		}
		got := row{Stage: next.Stage, Active: []int64{}, Effects: []effect{}}
		if len(next.Active) != len(ids[next.Stage]) {
			t.Fatalf("input %d: malformed active flags in %q", i, next.Stage)
		}
		for j, on := range next.Active {
			if on {
				got.Active = append(got.Active, ids[next.Stage][j])
			}
		}
		sort.Slice(got.Active, func(i, j int) bool { return got.Active[i] < got.Active[j] })
		for _, v := range intents {
			got.Effects = append(got.Effects, effect{v.NodeID, v.Name, v.Arg})
		}
		if !reflect.DeepEqual(got, trace[i]) {
			t.Fatalf("input %d (%+v): got %+v; source %+v", i, in, got, trace[i])
		}
		s = next
	}
}

func TestRejectInsteadOfGuess(t *testing.T) {
	base := string(read(t, "synthetic.editor.json"))
	tests := []struct{ name, from, to string }{
		{"unknown type", `"type":4`, `"type":99`},
		{"parallel BT shape", `"type":4`, `"type":5,"args":{"sucNum":1,"failNum":1}`},
		{"duplicate ID", `"id":3,`, `"id":2,`},
		{"duplicate stage", `"unique_name":"beta"`, `"unique_name":"alpha"`},
		{"unmapped event", `"cid":"11"`, `"cid":"110"`},
		{"unmapped condition", `"cid":"21"`, `"cid":"210"`},
		{"unmapped action", `"cid":"31"`, `"cid":"310"`},
		{"noncanonical cid", `"cid":"11"`, `"cid":"011"`},
		{"negative cid", `"cid":"11"`, `"cid":"-1"`},
		{"cid overflow", `"cid":"11"`, `"cid":"4294967296"`},
		{"numeric CID", `"cid":"31"`, `"cid":31`},
		{"unexpected structural cid", `"id":2,"type":4`, `"id":2,"type":4,"cid":"900"`},
		{"missing child", `[3,4,7]`, `[3,4,77]`},
		{"shared child", `[3,4,7]`, `[3,4,6]`},
		{"repeated child", `[3,4,7]`, `[3,4,7,7]`},
		{"orphan", `[3,4,7]`, `[3,4]`},
		{"disconnected self cycle", `"node": [`, `"node": [{"id":99,"type":4,"child":[99]},`},
		{"root parent", `[3,4,7]`, `[3,4,7,1]`},
		{"duplicate keys", `"id":1,`, `"id":7,"id":1,`},
		{"case-folded keys", `"type":0`, `"Type":0`},
		{"unknown key", `"id":3,`, `"extra":true,"id":3,`},
		{"duplicate keys inside arg", `"value":"20"`, `"value":"0","value":"20"`},
		{"unhandled arg field", `"value":"20"`, `"value":"20","optional":"x"`},
		{"null mode", `"mode":true`, `"mode":null`},
		{"missing root marker", `"root":true`, `"root":false`},
		{"leaf owns children", `"id":3,"type":3`, `"id":3,"child":[4],"type":3`},
		{"unknown syntax", `"left_args":[]`, `"left_args":[{"type":"int","value":"2"}]`},
		{"unknown op", `"op":"eq"`, `"op":"not"`},
		{"missing op", `"op":"eq",`, ``},
		{"null literal", `"value":"20"`, `"value":null`},
		{"nonint literal", `"type":"int","value":"20"`, `"type":"bool","value":"true"`},
		{"invalid int", `"value":"20"`, `"value":"two"`},
		{"multiargument action", `{"type":"int","value":"20"}`, `{"type":"int","value":"20"},{"type":"int","value":"20"}`},
		{"extra event parameter", `"cid":"12","enable":true,"mode":false,"args":[]`, `"cid":"12","enable":true,"mode":false,"args":[{"type":"int","value":"1","op":"eq","enable":true}]`},
		{"disabled comparison", `"op":"eq","enable":true`, `"op":"eq","enable":false`},
		{"unordered same event", `"cid":"12","enable":true,"mode":false,"args":[]`, `"cid":"11","enable":true,"mode":false,"args":[{"type":"int","value":"7","op":"eq","enable":true}]`},
		{"delayed emit", `"id":7,"type":3`, `"id":7,"type":3,"delay":2`},
		{"lifetime limit", `"id":7,"type":3`, `"id":7,"type":3,"time":2`},
		{"missing stage", `"value":"beta"`, `"value":"missing"`},
		{"self stage", `"value":"beta"`, `"value":"alpha"`},
		{"multiple transitions per stage", `"id":13,"type":3,"cid":"31","args":[{"type":"int","value":"60"}]`, `"id":13,"type":3,"cid":"32","args":[{"type":"string","value":"beta"}]`},
		{"parameter declarations", `"parameter": []`, `"parameter": [{"name":"v","type":"int","value":"1"}]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(base, tc.from) {
				t.Fatal("bad test replacement")
			}
			bad := strings.Replace(base, tc.from, tc.to, 1)
			if p, err := ssrimport.Import([]byte(bad), source(), binding(), registry()); err == nil || p != nil {
				t.Fatal("accepted unsupported editor configuration")
			}
		})
	}
}

func TestInvalidPlacementInOtherwiseConnectedTree(t *testing.T) {
	const stage = `{"id":1,"type":0,"root":true,"unique_name":"alpha","child":[2]}`
	const test = `{"id":2,"type":1,"cid":"21","args":{"left_args":[],"right_args":[{"type":"int","value":"1"}],"op":"eq"}}`
	const ev = `{"id":2,"type":5,"cid":"12","args":[],"child":[3]}`
	const nested = `{"id":3,"type":5,"cid":"13","args":[],"child":[4]}`
	const mark = `{"id":4,"type":3,"cid":"31","args":[{"type":"int","value":"1"}]}`
	for name, nodes := range map[string][]string{"root-condition": {stage, test}, "event-event": {stage, ev, nested, mark}, "composite-event": {stage, `{"id":2,"type":4,"child":[3]}`, nested, mark}} {
		t.Run(name, func(t *testing.T) {
			raw := `{"node":[` + strings.Join(nodes, ",") + `]}`
			if p, err := ssrimport.Import([]byte(raw), source(), binding(), registry()); err == nil || p != nil {
				t.Fatal("accepted invalid node placement")
			}
		})
	}
}

func TestCallerContractFailsClosed(t *testing.T) {
	raw := read(t, "synthetic.editor.json")
	for _, d := range []string{"", "ssr.bt_engine", "ssr.trigger_engine"} {
		src := source()
		src.Dialect = d
		if p, err := ssrimport.Import(raw, src, binding(), registry()); err == nil || p != nil {
			t.Fatalf("accepted dialect %q", d)
		}
	}
	for _, kind := range []string{"call", "set", "open", "close"} {
		b := binding()
		b.Actions[31] = ssrimport.ActionBinding{Kind: kind, Name: "mark"}
		if p, err := ssrimport.Import(raw, source(), b, registry()); err == nil || p != nil {
			t.Fatalf("accepted worker %q", kind)
		}
	}
	b := binding()
	b.Events[13] = ssrimport.EventBinding{Name: "advance"}
	if p, err := ssrimport.Import(raw, source(), b, registry()); err == nil || p != nil {
		t.Fatal("accepted event alias across stages")
	}
	reg := registry()
	reg.Intents = nil
	if p, err := ssrimport.Import(raw, source(), binding(), reg); err == nil || p != nil {
		t.Fatal("accepted intent absent from registry")
	}
	src := source()
	src.Start = "missing"
	if p, err := ssrimport.Import(raw, src, binding(), registry()); err == nil || p != nil {
		t.Fatal("inferred missing start")
	}
}
func TestBoundedAndStrictEnvelope(t *testing.T) {
	for _, raw := range [][]byte{nil, []byte("{} {}"), bytes.Repeat([]byte{' '}, (1<<20)+1), []byte("{\"node\":[null]}"), []byte("{\"node\": [" + strings.Repeat("[", 100) + "1" + strings.Repeat("]", 100) + "]}"), bytes.Join([][]byte{read(t, "synthetic.editor.json"), []byte("null")}, nil)} {
		if p, err := ssrimport.Import(raw, source(), binding(), registry()); err == nil || p != nil {
			t.Fatal("accepted malformed or unbounded document")
		}
	}
}

func TestNoChildrenCompositesWithAttachedActions(t *testing.T) {
	for _, tc := range []struct{ typ, expected int }{{2, 0}, {4, 1}} {
		t.Run(fmt.Sprint(tc.typ), func(t *testing.T) {
			raw := fmt.Sprintf(`{"node":[{"id":1,"type":0,"root":true,"unique_name":"alpha","child":[2]},{"id":2,"type":%d,"child":[3]},{"id":3,"type":3,"cid":"31","args":[{"type":"int","value":"100"}]}]}`, tc.typ)
			p, err := ssrimport.Import([]byte(raw), source(), binding(), registry())
			if err != nil {
				t.Fatal(err)
			}
			_, got, err := p.Step(p.NewState(), decision.Input{Now: 1})
			if err != nil || len(got) != tc.expected {
				t.Fatalf("intents %v error %v", got, err)
			}
		})
	}
}
