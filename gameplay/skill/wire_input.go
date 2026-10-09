package skill

import (
	"encoding/json"
	"fmt"
)

type InputSchemaDefinition interface{ inputSchemaDefinition() }

type NoneInputSchemaDefinition struct{}

func (NoneInputSchemaDefinition) inputSchemaDefinition() {}

type DirectionInputSchemaDefinition struct{ MaximumRange *int64 }

func (DirectionInputSchemaDefinition) inputSchemaDefinition() {}

type PositionInputSchemaDefinition struct {
	MaximumRange *int64
	ClampPolicy  string
}

func (PositionInputSchemaDefinition) inputSchemaDefinition() {}

type EntityInputSchemaDefinition struct{ MaximumRange *int64 }

func (EntityInputSchemaDefinition) inputSchemaDefinition() {}

type DirectionPositionInputSchemaDefinition struct {
	MaximumRange *int64
	ClampPolicy  string
}

func (DirectionPositionInputSchemaDefinition) inputSchemaDefinition() {}

type EntityPositionInputSchemaDefinition struct {
	MaximumRange *int64
	ClampPolicy  string
}

func (EntityPositionInputSchemaDefinition) inputSchemaDefinition() {}

type TwoPointInputSchemaDefinition struct {
	MaximumRange, MinimumLength, MaximumLength int64
	ClampPolicy                                string
}

func (TwoPointInputSchemaDefinition) inputSchemaDefinition() {}

type DragInputSchemaDefinition struct {
	MaximumRange, MinimumLength, MaximumLength int64
	ClampPolicy                                string
}

func (DragInputSchemaDefinition) inputSchemaDefinition() {}

type PathInputSchemaDefinition struct {
	MaximumPoints                            int
	MaximumTotalLength, MinimumSegmentLength int64
	SimplificationPolicy, ClampPolicy        string
}

func (PathInputSchemaDefinition) inputSchemaDefinition() {}

func decodeInputSchema(data []byte) (InputSchemaDefinition, error) {
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, err
	}
	switch header.Type {
	case "none":
		var raw struct {
			Type string `json:"type"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return NoneInputSchemaDefinition{}, nil
	case "direction":
		var raw struct {
			Type         string `json:"type"`
			MaximumRange *int64 `json:"maximum_range"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return DirectionInputSchemaDefinition{MaximumRange: raw.MaximumRange}, nil
	case "position":
		var raw struct {
			Type         string `json:"type"`
			MaximumRange *int64 `json:"maximum_range"`
			ClampPolicy  string `json:"clamp_policy"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return PositionInputSchemaDefinition{MaximumRange: raw.MaximumRange, ClampPolicy: raw.ClampPolicy}, nil
	case "entity":
		var raw struct {
			Type         string `json:"type"`
			MaximumRange *int64 `json:"maximum_range"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return EntityInputSchemaDefinition{MaximumRange: raw.MaximumRange}, nil
	case "direction_position":
		var raw struct {
			Type         string `json:"type"`
			MaximumRange *int64 `json:"maximum_range"`
			ClampPolicy  string `json:"clamp_policy"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return DirectionPositionInputSchemaDefinition{MaximumRange: raw.MaximumRange, ClampPolicy: raw.ClampPolicy}, nil
	case "entity_position":
		var raw struct {
			Type         string `json:"type"`
			MaximumRange *int64 `json:"maximum_range"`
			ClampPolicy  string `json:"clamp_policy"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return EntityPositionInputSchemaDefinition{MaximumRange: raw.MaximumRange, ClampPolicy: raw.ClampPolicy}, nil
	case "two_point", "drag":
		var raw struct {
			Type          string `json:"type"`
			MaximumRange  int64  `json:"maximum_range"`
			MinimumLength int64  `json:"minimum_length"`
			MaximumLength int64  `json:"maximum_length"`
			ClampPolicy   string `json:"clamp_policy"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		if raw.Type == "two_point" {
			return TwoPointInputSchemaDefinition{MaximumRange: raw.MaximumRange, MinimumLength: raw.MinimumLength, MaximumLength: raw.MaximumLength, ClampPolicy: raw.ClampPolicy}, nil
		}
		return DragInputSchemaDefinition{MaximumRange: raw.MaximumRange, MinimumLength: raw.MinimumLength, MaximumLength: raw.MaximumLength, ClampPolicy: raw.ClampPolicy}, nil
	case "path":
		var raw struct {
			Type                 string `json:"type"`
			MaximumPoints        int    `json:"maximum_points"`
			MaximumTotalLength   int64  `json:"maximum_total_length"`
			MinimumSegmentLength int64  `json:"minimum_segment_length"`
			SimplificationPolicy string `json:"simplification_policy"`
			ClampPolicy          string `json:"clamp_policy"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return PathInputSchemaDefinition{MaximumPoints: raw.MaximumPoints, MaximumTotalLength: raw.MaximumTotalLength, MinimumSegmentLength: raw.MinimumSegmentLength, SimplificationPolicy: raw.SimplificationPolicy, ClampPolicy: raw.ClampPolicy}, nil
	default:
		return nil, fmt.Errorf("unsupported input schema %q", header.Type)
	}
}
