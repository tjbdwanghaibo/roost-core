package ai_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/gameplay/ai"
	"github.com/tjbdwanghaibo/roost-core/gameplay/ai/decision"
)

// 同一段输入，区分两种刻意保留的产品语义：编辑器式决策立即响应事件，行为树策略在下一拍消费缓冲。
// 对照各观察点而不是只比较最终阶段；错误地把一种解释器当作另一种，会在第二行而非终局被发现。
func TestDecisionAndBehaviorObserveTheSameInputAtDifferentBoundaries(t *testing.T) {
	type bc = ai.BehaviorContext[struct{}]
	var btLog []string
	leaf := func(s string) ai.Node[bc] {
		return &ai.FuncNode[bc]{Run: func(*bc) ai.Status { btLog = append(btLog, s); return ai.StatusSuccess }}
	}
	bt, err := ai.NewStagedStrategy(map[string]ai.Node[bc]{
		"idle": &ai.Selector[bc]{Children: []ai.Node[bc]{
			&ai.Sequence[bc]{Children: []ai.Node[bc]{&ai.EventReceived[bc]{Kind: 1, Take: true}, leaf("mark"), &ai.SetStage[bc]{Stage: "alert"}}}, leaf("scan"),
		}},
		"alert": leaf("chase"),
	}, "idle", ai.BehaviorStrategyOptions[struct{}]{Data: &struct{}{}})
	if err != nil {
		t.Fatal(err)
	}
	ctrl := ai.NewController(ai.ControllerHooks{})
	defer ctrl.Close("test done")
	if err := ctrl.SetStrategy(bt); err != nil {
		t.Fatal(err)
	}
	emit := func(id int64, name string) decision.Node {
		return decision.Node{Kind: "do", Actions: []decision.Action{{ID: id, Kind: "emit", Name: name}}}
	}
	d := decision.Definition{Schema: decision.Schema, Name: "timing", Start: "idle", Stages: []decision.Stage{
		{Name: "idle", Tick: []decision.Node{emit(1, "scan")}, Events: []decision.Listener{{ID: 2, Event: "signal", Once: true, Enabled: true, Body: []decision.Node{{Kind: "do", Actions: []decision.Action{{ID: 3, Kind: "emit", Name: "mark"}, {ID: 4, Kind: "stage", Name: "alert"}}}}}}},
		{Name: "alert", Tick: []decision.Node{emit(5, "chase")}},
	}}
	p, err := decision.Compile(d, decision.Registry{Events: []string{"signal"}, Intents: []string{"scan", "mark", "chase"}})
	if err != nil {
		t.Fatal(err)
	}
	s := p.NewState()
	for _, tc := range []struct {
		at               int64
		event            string
		decision, bt     string
		decStage, bStage string
	}{
		{1, "", "scan", "scan", "idle", "idle"},
		{2, "signal", "mark", "", "alert", "idle"},
		{3, "", "chase", "mark", "alert", "alert"},
		{4, "", "chase", "chase", "alert", "alert"},
	} {
		btLog = nil
		if tc.event == "" {
			ctrl.Tick(time.UnixMilli(tc.at))
		} else if !ctrl.OnEvent(ai.Event{Kind: 1}) {
			t.Fatal("behavior strategy rejected event")
		}
		next, intents, err := p.Step(s, decision.Input{Now: tc.at, Event: tc.event})
		if err != nil {
			t.Fatal(err)
		}
		s = next
		var wantBT []string
		if tc.bt != "" {
			wantBT = []string{tc.bt}
		}
		if len(intents) != 1 || intents[0].Name != tc.decision || s.Stage != tc.decStage || bt.Stage() != tc.bStage || !reflect.DeepEqual(btLog, wantBT) {
			t.Fatalf("input at %d event=%q: decision=(%s,%v), behavior=(%s,%v)", tc.at, tc.event, s.Stage, intents, bt.Stage(), btLog)
		}
		t.Logf("at=%d event=%q decision=%s/%s behavior=%s/%v", tc.at, tc.event, s.Stage, intents[0].Name, bt.Stage(), btLog)
	}
}
