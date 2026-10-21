package ai

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// D21 第 5 期：事件投递（C1）、分阶段策略（C2）、决策轨迹（C5）。

const (
	evAttacked uint16 = 1
	evEnraged  uint16 = 9
)

// 事件经 Controller.OnEvent 投给策略，BehaviorStrategy 缓冲到下一拍，树里用 EventReceived 读；冻结时丢弃（结束通知
// 不受影响），不接收事件的策略返回 false；缓冲有上限，超出丢最早的。
func TestEventsReachTheTreeOnTheNextTickAndAreDroppedWhileFrozen(t *testing.T) {
	var log []string
	root := &Selector[treeCtx]{Children: []Node[treeCtx]{
		&Sequence[treeCtx]{Children: []Node[treeCtx]{
			&EventReceived[treeCtx]{Kind: evAttacked, Take: true},
			&FuncNode[treeCtx]{Run: func(*treeCtx) Status { log = append(log, "fight back"); return StatusSuccess }},
		}},
		&FuncNode[treeCtx]{Run: func(ctx *treeCtx) Status {
			log = append(log, fmt.Sprintf("idle with %d events", len(ctx.Events())))
			return StatusSuccess
		}},
	}}
	strategy := NewBehaviorStrategy(root, BehaviorStrategyOptions[treeData]{Data: &treeData{}, MaxEvents: 2})
	c := NewController(ControllerHooks{})
	if err := c.SetStrategy(strategy); err != nil {
		t.Fatal(err)
	}
	if !c.OnEvent(Event{Kind: evAttacked, Data: "orc"}) {
		t.Fatal("event not delivered")
	}
	if len(log) != 0 {
		t.Fatalf("the tree ran on delivery: %v", log)
	}
	c.Tick(time.Time{})
	c.Tick(time.Time{})
	c.Freeze()
	if c.OnEvent(Event{Kind: evAttacked}) {
		t.Fatal("an event was delivered to a frozen controller")
	}
	c.Recover()
	c.Tick(time.Time{})
	for _, kind := range []uint16{7, 8, evAttacked} { // 上限 2：最早的 7 被丢弃
		c.OnEvent(Event{Kind: kind})
	}
	c.Tick(time.Time{})
	want := []string{"fight back", "idle with 0 events", "idle with 0 events", "fight back"}
	if !slices.Equal(log, want) {
		t.Fatalf("tree saw %v, want %v", log, want)
	}
	strategy.OnEvent(nil, Event{Kind: 7})
	strategy.OnEvent(nil, Event{Kind: 8})
	strategy.OnEvent(nil, Event{Kind: 6})
	if fmt.Sprint(strategy.events) != "[{8 <nil>} {6 <nil>}]" {
		t.Fatalf("buffer kept %v, want the newest two", strategy.events)
	}
	plain := NewController(ControllerHooks{})
	_ = plain.SetStrategy(&controllerTestStrategy{name: "deaf"})
	if plain.OnEvent(Event{Kind: evAttacked}) {
		t.Fatal("a strategy without OnEvent received an event")
	}
}

// 分阶段（C2）：树里的 SetStage 在本拍求值结束后生效；策略外的 SetStage 立即生效并 Reset 旧树（运行中的
// TaskflowAction 经 OnInterrupt 收尾）；不存在的阶段报 ErrStageUnknown；OnStage 报告每次切换。
func TestStagedStrategySwitchesAfterTheTickAndInterruptsTheOldTree(t *testing.T) {
	var log []string
	launched := int64(0)
	chase := &TaskflowAction[treeData]{
		Launch:      func(*treeCtx) (int64, error) { launched++; return 100 + launched, nil },
		OnInterrupt: func(id int64) { log = append(log, fmt.Sprintf("interrupt %d", id)) },
	}
	calm := &Selector[treeCtx]{Children: []Node[treeCtx]{
		&Guard[treeCtx]{Check: func(ctx *treeCtx) bool { return !ctx.HasEvent(evEnraged) }, Child: chase},
		&Sequence[treeCtx]{Children: []Node[treeCtx]{
			&SetStage[treeCtx]{Stage: "enraged"},
			&FuncNode[treeCtx]{Run: func(ctx *treeCtx) Status {
				log = append(log, "still calm after asking") // 切换在本拍结束后才生效
				return StatusSuccess
			}},
		}},
	}}
	smash := &TaskflowAction[treeData]{
		Launch:      func(*treeCtx) (int64, error) { launched++; return 200 + launched, nil },
		OnInterrupt: func(id int64) { log = append(log, fmt.Sprintf("interrupt %d", id)) },
	}
	strategy, err := NewStagedStrategy(map[string]Node[treeCtx]{"calm": calm, "enraged": smash}, "calm", BehaviorStrategyOptions[treeData]{
		Data:    &treeData{},
		OnStage: func(from, to string) { log = append(log, from+" -> "+to) },
	})
	if err != nil {
		t.Fatal(err)
	}
	c := NewController(ControllerHooks{})
	if err := c.SetStrategy(strategy); err != nil {
		t.Fatal(err)
	}
	c.Tick(time.Time{}) // calm：发起追击（101）
	c.OnEvent(Event{Kind: evEnraged})
	c.Tick(time.Time{}) // 守卫打断追击，请求切到 enraged
	if strategy.Stage() != "enraged" {
		t.Fatalf("stage %q after the tick that asked for enraged", strategy.Stage())
	}
	c.Tick(time.Time{}) // enraged：发起重击（202）
	if err := strategy.SetStage("calm"); err != nil {
		t.Fatal(err)
	}
	want := []string{"interrupt 101", "still calm after asking", "calm -> enraged", "interrupt 202", "enraged -> calm"}
	if !slices.Equal(log, want) || strategy.Stage() != "calm" {
		t.Fatalf("log %v stage %q, want %v", log, strategy.Stage(), want)
	}
	if err := strategy.SetStage("dead"); !errors.Is(err, ErrStageUnknown) {
		t.Fatalf("unknown stage: %v", err)
	}
	if _, err := NewStagedStrategy(map[string]Node[treeCtx]{"calm": calm}, "missing", BehaviorStrategyOptions[treeData]{}); !errors.Is(err, ErrStageUnknown) {
		t.Fatalf("missing start stage: %v", err)
	}
	if err := strategy.Init(nil); err != nil || strategy.Stage() != "calm" {
		t.Fatalf("Init: %v stage %q", err, strategy.Stage())
	}
}

const stagedDoc = `{
  "schema": "roost.ai/v1",
  "start": "calm",
  "stages": {
    "calm": {"node": "selector", "children": [
      {"node": "guard", "condition": {"node": "inverter", "child": {"node": "event", "kind": 9}},
       "child": {"node": "action", "kind": "mark", "args": {"text": "calm"}}},
      {"node": "set_stage", "stage": "enraged"}
    ]},
    "enraged": {"node": "action", "kind": "mark", "args": {"text": "enraged"}}
  }
}`

func markRegistry(t *testing.T, log *[]string) *Registry[treeCtx] {
	t.Helper()
	registry := NewRegistry[treeCtx](nil)
	if err := registry.RegisterAction("mark", func(args json.RawMessage) (Node[treeCtx], error) {
		var parsed struct{ Text string }
		if err := json.Unmarshal(args, &parsed); err != nil {
			return nil, err
		}
		return &FuncNode[treeCtx]{Run: func(*treeCtx) Status { *log = append(*log, parsed.Text); return StatusSuccess }}, nil
	}); err != nil {
		t.Fatal(err)
	}
	return registry
}

// JSON（C2 / C1）：stages 顶层、set_stage 与 event 内置叶子；起始阶段与 set_stage 的目标都要存在，ParseTree 不接受
// set_stage，取走事件的 event 不能做守卫谓词。
func TestParseStagesBuildsStagedTreesWithEventAndSetStageLeaves(t *testing.T) {
	var log []string
	stages, err := ParseStages([]byte(stagedDoc), markRegistry(t, &log))
	if err != nil {
		t.Fatal(err)
	}
	strategy, err := NewStagedStrategy(stages.Roots, stages.Start, BehaviorStrategyOptions[treeData]{Data: &treeData{}})
	if err != nil {
		t.Fatal(err)
	}
	c := NewController(ControllerHooks{})
	_ = c.SetStrategy(strategy)
	c.Tick(time.Time{})
	c.OnEvent(Event{Kind: evEnraged})
	c.Tick(time.Time{})
	c.Tick(time.Time{})
	if fmt.Sprint(log) != "[calm enraged]" || strategy.Stage() != "enraged" {
		t.Fatalf("log %v stage %q", log, strategy.Stage())
	}
	for name, doc := range map[string]string{
		"unknown target": strings.Replace(stagedDoc, `"stage": "enraged"`, `"stage": "dead"`, 1),
		"unknown start":  strings.Replace(stagedDoc, `"start": "calm"`, `"start": "dead"`, 1),
		"taking guard":   strings.Replace(stagedDoc, `"kind": 9}`, `"kind": 9, "take": true}`, 1),
	} {
		if _, err := ParseStages([]byte(doc), markRegistry(t, &log)); !errors.Is(err, ErrTreeInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := ParseTree([]byte(`{"schema": "roost.ai/v1", "root": {"node": "set_stage", "stage": "x"}}`), markRegistry(t, &log)); !errors.Is(err, ErrTreeInvalid) {
		t.Errorf("ParseTree accepted set_stage: %v", err)
	}
}

// 决策轨迹（C5）：注册表开了 TraceNodes，解析出的每个节点都把 (JSON 路径, 结果) 报给策略的 Trace；不开时没有轨迹。
func TestTraceReportsEveryNodeWhenTheRegistryTracesNodes(t *testing.T) {
	var log, trace []string
	registry := markRegistry(t, &log)
	registry.TraceNodes()
	root, err := ParseTree([]byte(`{"schema": "roost.ai/v1", "root": {"node": "sequence", "children": [
		{"node": "action", "kind": "mark", "args": {"text": "a"}}, {"node": "event", "kind": 1}]}}`), registry)
	if err != nil {
		t.Fatal(err)
	}
	strategy := NewBehaviorStrategy(root, BehaviorStrategyOptions[treeData]{Data: &treeData{}, Trace: func(path string, status Status) {
		trace = append(trace, fmt.Sprintf("%s=%d", path, status))
	}})
	strategy.Tick(&Context{}, time.Time{})
	want := []string{"$.root.children[0]=2", "$.root.children[1]=3", "$.root=3"}
	if !slices.Equal(trace, want) {
		t.Fatalf("trace %v, want %v", trace, want)
	}
	plain, err := ParseTree([]byte(`{"schema": "roost.ai/v1", "root": {"node": "action", "kind": "mark", "args": {"text": "b"}}}`), markRegistry(t, &log))
	if err != nil {
		t.Fatal(err)
	}
	trace = nil
	NewBehaviorStrategy(plain, BehaviorStrategyOptions[treeData]{Data: &treeData{}, Trace: func(path string, _ Status) { trace = append(trace, path) }}).Tick(&Context{}, time.Time{})
	if len(trace) != 0 {
		t.Fatalf("an untraced tree traced %v", trace)
	}
}
