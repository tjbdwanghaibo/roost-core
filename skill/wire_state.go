package skill

import (
	"encoding/json"
	"fmt"
	"strings"
)

type PersistentStateDefinition struct {
	Type     string
	Scope    string
	Default  Value
	Minimum  *int64
	Maximum  *int64
	Values   []string
	Lifetime StateLifetimeDefinition
}

type StateLifetimeDefinition struct {
	DurationTicks        Tick     `json:"duration_ticks"`
	MaximumDurationTicks Tick     `json:"maximum_duration_ticks"`
	OnWrite              string   `json:"on_write"`
	ClearOn              []string `json:"clear_on"`
}

func decodePersistentState(values map[string]json.RawMessage) (map[string]PersistentStateDefinition, error) {
	result := make(map[string]PersistentStateDefinition, len(values))
	for name, data := range values {
		if strings.HasPrefix(name, "shared.") {
			return nil, fmt.Errorf("%s: shared state cannot be declared by a skill", name)
		}
		var raw struct {
			Type     string                  `json:"type"`
			Scope    string                  `json:"scope"`
			Default  json.RawMessage         `json:"default"`
			Minimum  *int64                  `json:"minimum"`
			Maximum  *int64                  `json:"maximum"`
			Values   []string                `json:"values"`
			Lifetime StateLifetimeDefinition `json:"lifetime"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		defaultValue, err := decodeValue(raw.Default)
		if err != nil {
			return nil, fmt.Errorf("%s.default: %w", name, err)
		}
		result[name] = PersistentStateDefinition{Type: raw.Type, Scope: raw.Scope, Default: defaultValue, Minimum: raw.Minimum, Maximum: raw.Maximum, Values: append([]string(nil), raw.Values...), Lifetime: raw.Lifetime}
	}
	return result, nil
}
