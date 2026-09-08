package skill

import "encoding/json"

type EffectResultDefinition struct {
	As      *string
	Success FlowDefinition
	Failure FlowDefinition
}

func decodeEffectResult(data []byte) (*EffectResultDefinition, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var raw struct {
		As      *string         `json:"as"`
		Success json.RawMessage `json:"success"`
		Failure json.RawMessage `json:"failure"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	success, err := decodeOptionalFlow(raw.Success)
	if err != nil {
		return nil, err
	}
	failure, err := decodeOptionalFlow(raw.Failure)
	if err != nil {
		return nil, err
	}
	if success == nil && failure == nil {
		return nil, errEffectResultBranchesRequired
	}
	return &EffectResultDefinition{As: raw.As, Success: success, Failure: failure}, nil
}
