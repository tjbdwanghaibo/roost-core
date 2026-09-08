package skill

import (
	"encoding/json"
	"fmt"
)

type FlowDefinition interface{ flowDefinition() }

type SequenceFlowDefinition struct{ Steps []FlowDefinition }

func (SequenceFlowDefinition) flowDefinition() {}

type ParallelFlowDefinition struct{ Branches []FlowDefinition }

func (ParallelFlowDefinition) flowDefinition() {}

type IfFlowDefinition struct {
	Condition  Value
	Then, Else FlowDefinition
}

func (IfFlowDefinition) flowDefinition() {}

type RepeatFlowDefinition struct {
	Times         Value
	IntervalTicks Tick
	IndexAs       string
	Do            FlowDefinition
}

func (RepeatFlowDefinition) flowDefinition() {}

type WaitFlowDefinition struct {
	Ticks Tick
	Then  FlowDefinition
}

func (WaitFlowDefinition) flowDefinition() {}

type SelectFlowDefinition struct {
	Select  SelectDefinition
	Consume SelectConsumeDefinition
	OnEmpty FlowDefinition
}

func (SelectFlowDefinition) flowDefinition() {}

type EffectFlowDefinition struct {
	Effect  EffectDefinition
	Result  *EffectResultDefinition
	On      *ProcessCallbacksDefinition
	Process *ProcessDefinition
}

func (EffectFlowDefinition) flowDefinition() {}

type GotoFlowDefinition struct{ Phase string }

func (GotoFlowDefinition) flowDefinition() {}

type FinishFlowDefinition struct{ Reason string }

func (FinishFlowDefinition) flowDefinition() {}

type ProcessCallbacksDefinition struct {
	Tick, Hit, Collision, End, Cancel, Transition, TargetLost, Enter, Leave FlowDefinition
}

func decodeFlow(data []byte) (FlowDefinition, error) {
	var header struct {
		Flow string `json:"flow"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, err
	}
	switch header.Flow {
	case "sequence":
		var raw struct {
			Flow  string            `json:"flow"`
			Steps []json.RawMessage `json:"steps"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		steps, err := decodeFlows(raw.Steps)
		if err != nil {
			return nil, err
		}
		return SequenceFlowDefinition{Steps: steps}, nil
	case "parallel":
		var raw struct {
			Flow     string            `json:"flow"`
			Branches []json.RawMessage `json:"branches"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		branches, err := decodeFlows(raw.Branches)
		if err != nil {
			return nil, err
		}
		return ParallelFlowDefinition{Branches: branches}, nil
	case "if":
		var raw struct {
			Flow      string          `json:"flow"`
			Condition json.RawMessage `json:"condition"`
			Then      json.RawMessage `json:"then"`
			Else      json.RawMessage `json:"else"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		condition, err := decodeValue(raw.Condition)
		if err != nil {
			return nil, fmt.Errorf("condition: %w", err)
		}
		thenFlow, err := decodeRequiredFlow(raw.Then, "then")
		if err != nil {
			return nil, err
		}
		elseFlow, err := decodeOptionalFlow(raw.Else)
		if err != nil {
			return nil, err
		}
		return IfFlowDefinition{Condition: condition, Then: thenFlow, Else: elseFlow}, nil
	case "repeat":
		var raw struct {
			Flow          string          `json:"flow"`
			Times         json.RawMessage `json:"times"`
			IntervalTicks Tick            `json:"interval_ticks"`
			IndexAs       string          `json:"index_as"`
			Do            json.RawMessage `json:"do"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		times, err := decodeValue(raw.Times)
		if err != nil {
			return nil, fmt.Errorf("times: %w", err)
		}
		doFlow, err := decodeRequiredFlow(raw.Do, "do")
		if err != nil {
			return nil, err
		}
		return RepeatFlowDefinition{Times: times, IntervalTicks: raw.IntervalTicks, IndexAs: raw.IndexAs, Do: doFlow}, nil
	case "wait":
		var raw struct {
			Flow  string          `json:"flow"`
			Ticks Tick            `json:"ticks"`
			Then  json.RawMessage `json:"then"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		thenFlow, err := decodeRequiredFlow(raw.Then, "then")
		if err != nil {
			return nil, err
		}
		return WaitFlowDefinition{Ticks: raw.Ticks, Then: thenFlow}, nil
	case "select":
		var raw struct {
			Flow    string          `json:"flow"`
			Select  json.RawMessage `json:"select"`
			Consume json.RawMessage `json:"consume"`
			OnEmpty json.RawMessage `json:"on_empty"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		selection, err := decodeSelect(raw.Select)
		if err != nil {
			return nil, fmt.Errorf("select: %w", err)
		}
		consume, err := decodeSelectConsume(raw.Consume)
		if err != nil {
			return nil, fmt.Errorf("consume: %w", err)
		}
		onEmpty, err := decodeOptionalFlow(raw.OnEmpty)
		if err != nil {
			return nil, fmt.Errorf("on_empty: %w", err)
		}
		return SelectFlowDefinition{Select: selection, Consume: consume, OnEmpty: onEmpty}, nil
	case "effect":
		var raw struct {
			Flow    string          `json:"flow"`
			Effect  json.RawMessage `json:"effect"`
			Result  json.RawMessage `json:"result"`
			On      json.RawMessage `json:"on"`
			Process json.RawMessage `json:"process"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		effect, err := decodeEffect(raw.Effect)
		if err != nil {
			return nil, fmt.Errorf("effect: %w", err)
		}
		result, err := decodeEffectResult(raw.Result)
		if err != nil {
			return nil, fmt.Errorf("result: %w", err)
		}
		callbacks, err := decodeProcessCallbacks(raw.On)
		if err != nil {
			return nil, fmt.Errorf("on: %w", err)
		}
		var process *ProcessDefinition
		if len(raw.Process) != 0 && string(raw.Process) != "null" {
			process, err = decodeProcess(raw.Process)
			if err != nil {
				return nil, fmt.Errorf("process: %w", err)
			}
		}
		return EffectFlowDefinition{Effect: effect, Result: result, On: callbacks, Process: process}, nil
	case "goto":
		var raw struct {
			Flow  string `json:"flow"`
			Phase string `json:"phase"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return GotoFlowDefinition{Phase: raw.Phase}, nil
	case "finish":
		var raw struct {
			Flow   string `json:"flow"`
			Reason string `json:"reason"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return FinishFlowDefinition{Reason: raw.Reason}, nil
	default:
		return nil, fmt.Errorf("unsupported flow %q", header.Flow)
	}
}

func decodeFlows(raw []json.RawMessage) ([]FlowDefinition, error) {
	result := make([]FlowDefinition, len(raw))
	for i := range raw {
		flow, err := decodeFlow(raw[i])
		if err != nil {
			return nil, fmt.Errorf("[%d]: %w", i, err)
		}
		result[i] = flow
	}
	return result, nil
}

func decodeRequiredFlow(raw json.RawMessage, name string) (FlowDefinition, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%s is required", name)
	}
	flow, err := decodeFlow(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return flow, nil
}

func decodeOptionalFlow(raw json.RawMessage) (FlowDefinition, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	return decodeFlow(raw)
}

func decodeProcessCallbacks(data []byte) (*ProcessCallbacksDefinition, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var raw struct {
		Tick       json.RawMessage `json:"tick"`
		Hit        json.RawMessage `json:"hit"`
		Collision  json.RawMessage `json:"collision"`
		End        json.RawMessage `json:"end"`
		Cancel     json.RawMessage `json:"cancel"`
		Transition json.RawMessage `json:"transition"`
		TargetLost json.RawMessage `json:"target_lost"`
		Enter      json.RawMessage `json:"enter"`
		Leave      json.RawMessage `json:"leave"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	result := &ProcessCallbacksDefinition{}
	items := []struct {
		raw    json.RawMessage
		target *FlowDefinition
	}{{raw.Tick, &result.Tick}, {raw.Hit, &result.Hit}, {raw.Collision, &result.Collision}, {raw.End, &result.End}, {raw.Cancel, &result.Cancel}, {raw.Transition, &result.Transition}, {raw.TargetLost, &result.TargetLost}, {raw.Enter, &result.Enter}, {raw.Leave, &result.Leave}}
	for _, item := range items {
		flow, err := decodeOptionalFlow(item.raw)
		if err != nil {
			return nil, err
		}
		*item.target = flow
	}
	return result, nil
}
