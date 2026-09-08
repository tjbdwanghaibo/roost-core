package skill

import (
	"encoding/json"
	"fmt"
)

type runtimeValueJSON struct {
	Present       bool               `json:"present"`
	Kind          string             `json:"kind"`
	Quantity      string             `json:"quantity,omitempty"`
	Optional      bool               `json:"optional,omitempty"`
	Integer       *int64             `json:"integer,omitempty"`
	Boolean       *bool              `json:"boolean,omitempty"`
	Text          *string            `json:"text,omitempty"`
	Entity        *EntityID          `json:"entity,omitempty"`
	Position      *Position          `json:"position,omitempty"`
	Direction     *Direction         `json:"direction,omitempty"`
	Hit           *Hit               `json:"hit,omitempty"`
	Path          []Position         `json:"path,omitempty"`
	Ability       *AbilityRef        `json:"ability,omitempty"`
	Status        *StatusInstanceRef `json:"status,omitempty"`
	Entities      []EntityID         `json:"entities,omitempty"`
	Strings       []string           `json:"strings,omitempty"`
	Snapshot      *uint64            `json:"snapshot,omitempty"`
	Process       *ProcessID         `json:"process,omitempty"`
	ResultType    string             `json:"result_type,omitempty"`
	ResultOutcome *ResultOutcome     `json:"result_outcome,omitempty"`
	ResultFields  []RuntimeValue     `json:"result_fields,omitempty"`
}

func (value RuntimeValue) MarshalJSON() ([]byte, error) {
	wire := runtimeValueJSON{Present: value.present, Kind: valueKindName(value.typ.Base), Quantity: quantityKindName(value.typ.Quantity), Optional: value.typ.Optional}
	if value.typ.Quantity == quantityUnknown {
		wire.Quantity = ""
	}
	if value.present {
		switch value.typ.Base {
		case valueKindInt, valueKindAttribute, valueKindElement, valueKindGameplayTag:
			wire.Integer = &value.integer
		case valueKindBool:
			wire.Boolean = &value.boolean
		case valueKindString:
			wire.Text = &value.text
		case valueKindEntity:
			wire.Entity = &value.entity
		case valueKindPosition:
			wire.Position = &value.position
		case valueKindDirection:
			wire.Direction = &value.direction
		case valueKindHit:
			wire.Hit = &value.hit
		case valueKindPath:
			wire.Path = append([]Position(nil), value.path...)
		case valueKindAbility:
			wire.Ability = &value.ability
		case valueKindStatusInstance:
			wire.Status = &value.status
		case valueKindEntityList:
			wire.Entities = append([]EntityID(nil), value.entities...)
		case valueKindStringList:
			wire.Strings = append([]string(nil), value.strings...)
		case valueKindSnapshotToken:
			opaque := value.snapshot.OpaqueID()
			wire.Snapshot = &opaque
		case valueKindProcess:
			wire.Process = &value.process
		case valueKindEffectResult:
			wire.ResultType = string(value.effectResult.typ)
			outcome := value.effectResult.outcome
			wire.ResultOutcome = &outcome
			wire.ResultFields = append([]RuntimeValue(nil), value.effectResult.fields...)
		case valueKindNull:
		default:
			return nil, fmt.Errorf("skill: runtime value kind %q is not serializable", wire.Kind)
		}
	}
	return json.Marshal(wire)
}

func (value *RuntimeValue) UnmarshalJSON(data []byte) error {
	if value == nil {
		return fmt.Errorf("skill: nil RuntimeValue receiver")
	}
	var wire runtimeValueJSON
	if err := decodeStrictSingle(data, &wire); err != nil {
		return err
	}
	kind, ok := parseRuntimeValueKind(wire.Kind)
	if !ok {
		return fmt.Errorf("skill: unknown runtime value kind %q", wire.Kind)
	}
	quantity, ok := parseRuntimeQuantity(wire.Quantity)
	if !ok {
		return fmt.Errorf("skill: unknown runtime quantity %q", wire.Quantity)
	}
	decoded := RuntimeValue{present: wire.Present, typ: valueType{Base: kind, Optional: wire.Optional, Quantity: quantity}}
	if !wire.Present {
		*value = decoded
		return nil
	}
	switch kind {
	case valueKindInt, valueKindAttribute, valueKindElement, valueKindGameplayTag:
		if wire.Integer == nil {
			return ErrRuntimeValueMissing
		}
		decoded.integer = *wire.Integer
	case valueKindBool:
		if wire.Boolean == nil {
			return ErrRuntimeValueMissing
		}
		decoded.boolean = *wire.Boolean
	case valueKindString:
		if wire.Text == nil {
			return ErrRuntimeValueMissing
		}
		decoded.text = *wire.Text
	case valueKindEntity:
		if wire.Entity == nil {
			return ErrRuntimeValueMissing
		}
		decoded.entity = *wire.Entity
	case valueKindPosition:
		if wire.Position == nil {
			return ErrRuntimeValueMissing
		}
		decoded.position = *wire.Position
	case valueKindDirection:
		if wire.Direction == nil {
			return ErrRuntimeValueMissing
		}
		decoded.direction = *wire.Direction
	case valueKindHit:
		if wire.Hit == nil {
			return ErrRuntimeValueMissing
		}
		decoded.hit = *wire.Hit
	case valueKindPath:
		decoded.path = append([]Position(nil), wire.Path...)
	case valueKindAbility:
		if wire.Ability == nil {
			return ErrRuntimeValueMissing
		}
		decoded.ability = *wire.Ability
	case valueKindStatusInstance:
		if wire.Status == nil {
			return ErrRuntimeValueMissing
		}
		decoded.status = *wire.Status
	case valueKindEntityList:
		decoded.entities = append([]EntityID(nil), wire.Entities...)
	case valueKindStringList:
		decoded.strings = append([]string(nil), wire.Strings...)
	case valueKindSnapshotToken:
		if wire.Snapshot == nil || *wire.Snapshot == 0 {
			return ErrRuntimeValueMissing
		}
		decoded.snapshot = SnapshotToken{opaque: *wire.Snapshot}
	case valueKindProcess:
		if wire.Process == nil {
			return ErrRuntimeValueMissing
		}
		decoded.process = *wire.Process
	case valueKindEffectResult:
		if wire.ResultOutcome == nil {
			return ErrRuntimeValueMissing
		}
		decoded.effectResult = runtimeEffectResultValue{typ: resultType(wire.ResultType), outcome: *wire.ResultOutcome, fields: append([]RuntimeValue(nil), wire.ResultFields...)}
	case valueKindNull:
	default:
		return ErrRuntimeTypeMismatch
	}
	*value = decoded
	return nil
}

func parseRuntimeValueKind(name string) (valueKind, bool) {
	for kind := valueKindNull; kind <= valueKindEffectResult; kind++ {
		if valueKindName(kind) == name {
			return kind, true
		}
	}
	return valueKindInvalid, false
}

func parseRuntimeQuantity(name string) (quantityKind, bool) {
	if name == "" {
		return quantityUnknown, true
	}
	for kind := quantityUnknown; kind <= quantityResourceAmount; kind++ {
		if quantityKindName(kind) == name {
			return kind, true
		}
	}
	return quantityUnknown, false
}
