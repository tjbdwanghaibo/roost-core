package skill

import (
	"encoding/json"
	"fmt"
)

type SelectDefinition struct {
	From    Value
	Kind    string
	Shape   ShapeDefinition
	Filters []FilterDefinition
	Order   *SelectOrderDefinition
	Limit   int
}

type SelectOrderDefinition struct {
	By        string `json:"by"`
	Direction string `json:"direction"`
}

type SelectConsumeDefinition interface{ selectConsumeDefinition() }
type SelectOneConsumeDefinition struct {
	As   string
	Then FlowDefinition
}

func (SelectOneConsumeDefinition) selectConsumeDefinition() {}

type SelectEachConsumeDefinition struct {
	As string
	Do FlowDefinition
}

func (SelectEachConsumeDefinition) selectConsumeDefinition() {}

type ShapeDefinition interface{ shapeDefinition() }
type SingleShapeDefinition struct{}

func (SingleShapeDefinition) shapeDefinition() {}

type CircleShapeDefinition struct{ Radius Value }

func (CircleShapeDefinition) shapeDefinition() {}

type RingShapeDefinition struct{ InnerRadius, OuterRadius Value }

func (RingShapeDefinition) shapeDefinition() {}

type ConeShapeDefinition struct{ Range, AngleDeg, Direction Value }

func (ConeShapeDefinition) shapeDefinition() {}

type LineShapeDefinition struct{ Length, Width, Direction Value }

func (LineShapeDefinition) shapeDefinition() {}

type RectangleShapeDefinition struct{ Length, Width, Direction Value }

func (RectangleShapeDefinition) shapeDefinition() {}

type RaycastShapeDefinition struct {
	Length, Direction Value
	Collision         []string
}

func (RaycastShapeDefinition) shapeDefinition() {}

type ChainShapeDefinition struct {
	HopRange         Value
	MaxTargets       int
	AllowRepeat      bool
	HopIntervalTicks Tick
}

func (ChainShapeDefinition) shapeDefinition() {}

type PathShapeDefinition struct{ Points Value }

func (PathShapeDefinition) shapeDefinition() {}

type NearestValidShapeDefinition struct {
	SearchRadius Value
	Collision    []string
}

func (NearestValidShapeDefinition) shapeDefinition() {}

type AbilitySetShapeDefinition struct{}

func (AbilitySetShapeDefinition) shapeDefinition() {}

type StatusSetShapeDefinition struct{}

func (StatusSetShapeDefinition) shapeDefinition() {}

type OwnedEntitiesShapeDefinition struct{}

func (OwnedEntitiesShapeDefinition) shapeDefinition() {}

type FilterDefinition interface{ filterDefinition() }
type FlagFilterDefinition struct{ Type string }

func (FlagFilterDefinition) filterDefinition() {}

type RelationFilterDefinition struct{ Value string }

func (RelationFilterDefinition) filterDefinition() {}

type StatusFilterDefinition struct{ Type, Status string }

func (StatusFilterDefinition) filterDefinition() {}

type AttributeCompareFilterDefinition struct {
	Attribute, Op string
	Value         Value
}

func (AttributeCompareFilterDefinition) filterDefinition() {}

type GameplayTagFilterDefinition struct{ Type, Tag string }

func (GameplayTagFilterDefinition) filterDefinition() {}

type LineOfSightFilterDefinition struct{ Collision []string }

func (LineOfSightFilterDefinition) filterDefinition() {}

type AbilityTagFilterDefinition struct{ Tag string }

func (AbilityTagFilterDefinition) filterDefinition() {}

type AbilitySlotFilterDefinition struct{ Slot int }

func (AbilitySlotFilterDefinition) filterDefinition() {}

type OwnedSourceSkillFilterDefinition struct{ Skill string }
type OwnedSourceCastFilterDefinition struct{ Cast CastID }
type OwnedSpawnTickFilterDefinition struct {
	Type string
	Tick Tick
}
type OwnedUnitTemplateFilterDefinition struct{ Template string }
type OwnedEntityTagFilterDefinition struct{ Tag string }

type StatusInstanceFilterDefinition struct {
	Type, Status, Text, Op string
	Value                  *Value
}

func (OwnedSourceSkillFilterDefinition) filterDefinition()  {}
func (OwnedSourceCastFilterDefinition) filterDefinition()   {}
func (OwnedSpawnTickFilterDefinition) filterDefinition()    {}
func (OwnedUnitTemplateFilterDefinition) filterDefinition() {}
func (OwnedEntityTagFilterDefinition) filterDefinition()    {}
func (StatusInstanceFilterDefinition) filterDefinition()    {}

func decodeSelect(data []byte) (SelectDefinition, error) {
	var raw struct {
		From    json.RawMessage        `json:"from"`
		Kind    string                 `json:"kind"`
		Shape   json.RawMessage        `json:"shape"`
		Filters []json.RawMessage      `json:"filters"`
		Order   *SelectOrderDefinition `json:"order"`
		Limit   int                    `json:"limit"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return SelectDefinition{}, err
	}
	from, err := decodeValue(raw.From)
	if err != nil {
		return SelectDefinition{}, fmt.Errorf("from: %w", err)
	}
	if !isSelectKind(raw.Kind) {
		return SelectDefinition{}, fmt.Errorf("unsupported select kind %q", raw.Kind)
	}
	shape, err := decodeShape(raw.Shape)
	if err != nil {
		return SelectDefinition{}, fmt.Errorf("shape: %w", err)
	}
	filters := make([]FilterDefinition, len(raw.Filters))
	for i := range raw.Filters {
		filter, err := decodeFilter(raw.Filters[i])
		if err != nil {
			return SelectDefinition{}, fmt.Errorf("filters[%d]: %w", i, err)
		}
		filters[i] = filter
	}
	return SelectDefinition{From: from, Kind: raw.Kind, Shape: shape, Filters: filters, Order: raw.Order, Limit: raw.Limit}, nil
}

func isSelectKind(value string) bool {
	switch value {
	case "entity", "position", "hit", "path", "ability", "status_instance":
		return true
	default:
		return false
	}
}

func decodeSelectConsume(data []byte) (SelectConsumeDefinition, error) {
	var header struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, err
	}
	switch header.Mode {
	case "one":
		var raw struct {
			Mode string          `json:"mode"`
			As   string          `json:"as"`
			Then json.RawMessage `json:"then"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		thenFlow, err := decodeRequiredFlow(raw.Then, "then")
		if err != nil {
			return nil, err
		}
		return SelectOneConsumeDefinition{As: raw.As, Then: thenFlow}, nil
	case "each":
		var raw struct {
			Mode string          `json:"mode"`
			As   string          `json:"as"`
			Do   json.RawMessage `json:"do"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		doFlow, err := decodeRequiredFlow(raw.Do, "do")
		if err != nil {
			return nil, err
		}
		return SelectEachConsumeDefinition{As: raw.As, Do: doFlow}, nil
	default:
		return nil, fmt.Errorf("unsupported consume mode %q", header.Mode)
	}
}

func decodeShape(data []byte) (ShapeDefinition, error) {
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, err
	}
	switch header.Type {
	case "single", "ability_set", "status_set", "owned_entities":
		var raw struct {
			Type string `json:"type"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		switch raw.Type {
		case "single":
			return SingleShapeDefinition{}, nil
		case "ability_set":
			return AbilitySetShapeDefinition{}, nil
		case "status_set":
			return StatusSetShapeDefinition{}, nil
		default:
			return OwnedEntitiesShapeDefinition{}, nil
		}
	case "circle":
		var raw struct {
			Type   string          `json:"type"`
			Radius json.RawMessage `json:"radius"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		radius, err := decodeValue(raw.Radius)
		if err != nil {
			return nil, err
		}
		return CircleShapeDefinition{Radius: radius}, nil
	case "ring":
		var raw struct {
			Type  string          `json:"type"`
			Inner json.RawMessage `json:"inner_radius"`
			Outer json.RawMessage `json:"outer_radius"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		inner, err := decodeValue(raw.Inner)
		if err != nil {
			return nil, err
		}
		outer, err := decodeValue(raw.Outer)
		if err != nil {
			return nil, err
		}
		return RingShapeDefinition{InnerRadius: inner, OuterRadius: outer}, nil
	case "cone":
		var raw struct {
			Type      string          `json:"type"`
			Range     json.RawMessage `json:"range"`
			Angle     json.RawMessage `json:"angle_deg"`
			Direction json.RawMessage `json:"direction"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		values, err := decodeValueList(raw.Range, raw.Angle, raw.Direction)
		if err != nil {
			return nil, err
		}
		return ConeShapeDefinition{Range: values[0], AngleDeg: values[1], Direction: values[2]}, nil
	case "line", "rectangle":
		var raw struct {
			Type      string          `json:"type"`
			Length    json.RawMessage `json:"length"`
			Width     json.RawMessage `json:"width"`
			Direction json.RawMessage `json:"direction"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		values, err := decodeValueList(raw.Length, raw.Width, raw.Direction)
		if err != nil {
			return nil, err
		}
		if raw.Type == "line" {
			return LineShapeDefinition{Length: values[0], Width: values[1], Direction: values[2]}, nil
		}
		return RectangleShapeDefinition{Length: values[0], Width: values[1], Direction: values[2]}, nil
	case "raycast":
		var raw struct {
			Type      string          `json:"type"`
			Length    json.RawMessage `json:"length"`
			Direction json.RawMessage `json:"direction"`
			Collision []string        `json:"collision"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		values, err := decodeValueList(raw.Length, raw.Direction)
		if err != nil {
			return nil, err
		}
		return RaycastShapeDefinition{Length: values[0], Direction: values[1], Collision: raw.Collision}, nil
	case "chain":
		var raw struct {
			Type             string          `json:"type"`
			HopRange         json.RawMessage `json:"hop_range"`
			MaxTargets       int             `json:"max_targets"`
			AllowRepeat      bool            `json:"allow_repeat"`
			HopIntervalTicks Tick            `json:"hop_interval_ticks"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		hop, err := decodeValue(raw.HopRange)
		if err != nil {
			return nil, err
		}
		return ChainShapeDefinition{HopRange: hop, MaxTargets: raw.MaxTargets, AllowRepeat: raw.AllowRepeat, HopIntervalTicks: raw.HopIntervalTicks}, nil
	case "path":
		var raw struct {
			Type   string          `json:"type"`
			Points json.RawMessage `json:"points"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		points, err := decodeValue(raw.Points)
		if err != nil {
			return nil, err
		}
		return PathShapeDefinition{Points: points}, nil
	case "nearest_valid":
		var raw struct {
			Type         string          `json:"type"`
			SearchRadius json.RawMessage `json:"search_radius"`
			Collision    []string        `json:"collision"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		radius, err := decodeValue(raw.SearchRadius)
		if err != nil {
			return nil, err
		}
		return NearestValidShapeDefinition{SearchRadius: radius, Collision: raw.Collision}, nil
	default:
		return nil, fmt.Errorf("unsupported shape %q", header.Type)
	}
}

func decodeFilter(data []byte) (FilterDefinition, error) {
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, err
	}
	switch header.Type {
	case "alive", "not_caster", "visible", "targetable", "self_ability", "not_self_ability", "ability_enabled", "ability_on_cooldown", "ability_has_ammo":
		var raw struct {
			Type string `json:"type"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return FlagFilterDefinition{Type: raw.Type}, nil
	case "line_of_sight":
		var raw struct {
			Type      string   `json:"type"`
			Collision []string `json:"collision"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return LineOfSightFilterDefinition{Collision: raw.Collision}, nil
	case "relation":
		var raw struct {
			Type  string `json:"type"`
			Value string `json:"value"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return RelationFilterDefinition{Value: raw.Value}, nil
	case "has_status", "missing_status":
		var raw struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return StatusFilterDefinition{Type: raw.Type, Status: raw.Status}, nil
	case "attribute_compare":
		var raw struct {
			Type      string          `json:"type"`
			Attribute string          `json:"attribute"`
			Op        string          `json:"op"`
			Value     json.RawMessage `json:"value"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		value, err := decodeValue(raw.Value)
		if err != nil {
			return nil, err
		}
		return AttributeCompareFilterDefinition{Attribute: raw.Attribute, Op: raw.Op, Value: value}, nil
	case "has_gameplay_tag", "missing_gameplay_tag":
		var raw struct {
			Type string `json:"type"`
			Tag  string `json:"tag"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return GameplayTagFilterDefinition{Type: raw.Type, Tag: raw.Tag}, nil
	case "ability_tag":
		var raw struct {
			Type string `json:"type"`
			Tag  string `json:"tag"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return AbilityTagFilterDefinition{Tag: raw.Tag}, nil
	case "ability_slot":
		var raw struct {
			Type string `json:"type"`
			Slot int    `json:"slot"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return AbilitySlotFilterDefinition{Slot: raw.Slot}, nil
	case "source_skill":
		var raw struct {
			Type  string `json:"type"`
			Skill string `json:"skill"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return OwnedSourceSkillFilterDefinition{Skill: raw.Skill}, nil
	case "source_cast":
		var raw struct {
			Type string `json:"type"`
			Cast CastID `json:"cast"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return OwnedSourceCastFilterDefinition{Cast: raw.Cast}, nil
	case "spawned_before", "spawned_after":
		var raw struct {
			Type string `json:"type"`
			Tick Tick   `json:"tick"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return OwnedSpawnTickFilterDefinition{Type: raw.Type, Tick: raw.Tick}, nil
	case "unit_template":
		var raw struct {
			Type     string `json:"type"`
			Template string `json:"template"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return OwnedUnitTemplateFilterDefinition{Template: raw.Template}, nil
	case "entity_tag":
		var raw struct {
			Type string `json:"type"`
			Tag  string `json:"tag"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return OwnedEntityTagFilterDefinition{Tag: raw.Tag}, nil
	case "status_id":
		var raw struct{ Type, Status string }
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return StatusInstanceFilterDefinition{Type: raw.Type, Status: raw.Status}, nil
	case "status_category":
		var raw struct {
			Type     string `json:"type"`
			Category string `json:"category"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return StatusInstanceFilterDefinition{Type: raw.Type, Text: raw.Category}, nil
	case "status_tag":
		var raw struct {
			Type string `json:"type"`
			Tag  string `json:"tag"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return StatusInstanceFilterDefinition{Type: raw.Type, Text: raw.Tag}, nil
	case "status_polarity":
		var raw struct {
			Type     string `json:"type"`
			Polarity string `json:"polarity"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return StatusInstanceFilterDefinition{Type: raw.Type, Text: raw.Polarity}, nil
	case "status_source_skill":
		var raw struct {
			Type  string `json:"type"`
			Skill string `json:"skill"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return StatusInstanceFilterDefinition{Type: raw.Type, Text: raw.Skill}, nil
	case "status_dispellable", "status_transferable", "status_copyable":
		var raw struct {
			Type string `json:"type"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return StatusInstanceFilterDefinition{Type: raw.Type}, nil
	case "status_source", "status_owner":
		var raw struct {
			Type   string          `json:"type"`
			Source json.RawMessage `json:"source"`
			Owner  json.RawMessage `json:"owner"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		encoded := raw.Source
		if raw.Type == "status_owner" {
			encoded = raw.Owner
		}
		value, err := decodeValue(encoded)
		if err != nil {
			return nil, err
		}
		return StatusInstanceFilterDefinition{Type: raw.Type, Value: &value}, nil
	case "status_stack_compare", "status_duration_compare":
		var raw struct {
			Type, Op string
			Value    json.RawMessage `json:"value"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		value, err := decodeValue(raw.Value)
		if err != nil {
			return nil, err
		}
		return StatusInstanceFilterDefinition{Type: raw.Type, Op: raw.Op, Value: &value}, nil
	default:
		return nil, fmt.Errorf("unsupported filter %q", header.Type)
	}
}

func decodeValueList(values ...json.RawMessage) ([]Value, error) {
	result := make([]Value, len(values))
	for i := range values {
		value, err := decodeValue(values[i])
		if err != nil {
			return nil, fmt.Errorf("value[%d]: %w", i, err)
		}
		result[i] = value
	}
	return result, nil
}
