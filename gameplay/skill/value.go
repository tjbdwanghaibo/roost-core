package skill

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

type Value struct{ Node ValueDefinition }

type ValueDefinition interface{ valueDefinition() }
type NullValueDefinition struct{}

func (NullValueDefinition) valueDefinition() {}

type IntValueDefinition struct{ Value int64 }

func (IntValueDefinition) valueDefinition() {}

type BoolValueDefinition struct{ Value bool }

func (BoolValueDefinition) valueDefinition() {}

type StringValueDefinition struct{ Value string }

func (StringValueDefinition) valueDefinition() {}

type ReferenceValueDefinition struct{ Reference string }

func (ReferenceValueDefinition) valueDefinition() {}

type ExpressionValueDefinition struct {
	Op   string
	Args []Value
}

func (ExpressionValueDefinition) valueDefinition() {}

type AttributeReadValueDefinition struct {
	Entity              Value
	Attribute, Snapshot string
}

func (AttributeReadValueDefinition) valueDefinition() {}

type StateReadValueDefinition struct {
	State    string
	Owner    *Value
	Subject  *Value
	TeamOf   *Value
	Snapshot string
}

func (StateReadValueDefinition) valueDefinition() {}

type AbilityStateReadValueDefinition struct {
	Owner    Value
	Ability  Value
	Property string
	Snapshot string
}

func (AbilityStateReadValueDefinition) valueDefinition() {}

func decodeValue(data []byte) (Value, error) {
	trimmed := bytes.TrimSpace(data)
	if bytes.Equal(trimmed, []byte("null")) {
		return Value{Node: NullValueDefinition{}}, nil
	}
	if len(trimmed) == 0 {
		return Value{}, fmt.Errorf("value is required")
	}
	switch trimmed[0] {
	case '"':
		var value string
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return Value{}, err
		}
		if strings.HasPrefix(value, "$") {
			return Value{Node: ReferenceValueDefinition{Reference: value}}, nil
		}
		return Value{Node: StringValueDefinition{Value: value}}, nil
	case 't', 'f':
		var value bool
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return Value{}, err
		}
		return Value{Node: BoolValueDefinition{Value: value}}, nil
	case '{':
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &probe); err != nil {
			return Value{}, err
		}
		if _, ok := probe["op"]; ok {
			var raw struct {
				Op   string            `json:"op"`
				Args []json.RawMessage `json:"args"`
			}
			if err := decodeStrictSingle(trimmed, &raw); err != nil {
				return Value{}, err
			}
			args := make([]Value, len(raw.Args))
			for i := range raw.Args {
				value, err := decodeValue(raw.Args[i])
				if err != nil {
					return Value{}, fmt.Errorf("args[%d]: %w", i, err)
				}
				args[i] = value
			}
			return Value{Node: ExpressionValueDefinition{Op: raw.Op, Args: args}}, nil
		}
		if body, ok := probe["read_attribute"]; ok && len(probe) == 1 {
			var raw struct {
				Entity    json.RawMessage `json:"entity"`
				Attribute string          `json:"attribute"`
				Snapshot  string          `json:"snapshot"`
			}
			if err := decodeStrictSingle(body, &raw); err != nil {
				return Value{}, err
			}
			entity, err := decodeValue(raw.Entity)
			if err != nil {
				return Value{}, err
			}
			return Value{Node: AttributeReadValueDefinition{Entity: entity, Attribute: raw.Attribute, Snapshot: raw.Snapshot}}, nil
		}
		if body, ok := probe["read_state"]; ok && len(probe) == 1 {
			var raw struct {
				State    string          `json:"state"`
				Owner    json.RawMessage `json:"owner"`
				Subject  json.RawMessage `json:"subject"`
				TeamOf   json.RawMessage `json:"team_of"`
				Snapshot string          `json:"snapshot"`
			}
			if err := decodeStrictSingle(body, &raw); err != nil {
				return Value{}, err
			}
			owner, err := decodeOptionalValue(raw.Owner)
			if err != nil {
				return Value{}, err
			}
			subject, err := decodeOptionalValue(raw.Subject)
			if err != nil {
				return Value{}, err
			}
			teamOf, err := decodeOptionalValue(raw.TeamOf)
			if err != nil {
				return Value{}, err
			}
			return Value{Node: StateReadValueDefinition{State: raw.State, Owner: owner, Subject: subject, TeamOf: teamOf, Snapshot: raw.Snapshot}}, nil
		}
		if body, ok := probe["read_ability_state"]; ok && len(probe) == 1 {
			var raw struct {
				Owner    json.RawMessage `json:"owner"`
				Ability  json.RawMessage `json:"ability"`
				Property string          `json:"property"`
				Snapshot string          `json:"snapshot"`
			}
			if err := decodeStrictSingle(body, &raw); err != nil {
				return Value{}, err
			}
			values, err := decodeValueList(raw.Owner, raw.Ability)
			if err != nil {
				return Value{}, err
			}
			return Value{Node: AbilityStateReadValueDefinition{Owner: values[0], Ability: values[1], Property: raw.Property, Snapshot: raw.Snapshot}}, nil
		}
		return Value{}, fmt.Errorf("unsupported value object")
	default:
		var value int64
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return Value{}, err
		}
		return Value{Node: IntValueDefinition{Value: value}}, nil
	}
}

func decodeOptionalValue(data json.RawMessage) (*Value, error) {
	if len(data) == 0 {
		return nil, nil
	}
	value, err := decodeValue(data)
	if err != nil {
		return nil, err
	}
	return &value, nil
}
