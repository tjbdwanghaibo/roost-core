package skill

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const SchemaV2 = "roost.skill/v2"

var (
	errEffectResultBranchesRequired = errors.New("effect result requires success or failure")
	ErrParseLimitExceeded           = errors.New("skill: JSON input exceeds configured limits")
)

// ParseLimits bounds work and memory before semantic decoding starts. The
// defaults are intentionally generous for authored skills while still making
// untrusted/generated JSON safe to accept at a service boundary.
type ParseLimits struct {
	MaxBytes            int
	MaxDepth            int
	MaxTokens           int
	MaxStringBytes      int
	MaxContainerEntries int
}

func DefaultParseLimits() ParseLimits {
	return ParseLimits{MaxBytes: 1 << 20, MaxDepth: 64, MaxTokens: 100000, MaxStringBytes: 64 << 10, MaxContainerEntries: 4096}
}

func normalizeParseLimits(limits ParseLimits) ParseLimits {
	defaults := DefaultParseLimits()
	if limits.MaxBytes <= 0 {
		limits.MaxBytes = defaults.MaxBytes
	}
	if limits.MaxDepth <= 0 {
		limits.MaxDepth = defaults.MaxDepth
	}
	if limits.MaxTokens <= 0 {
		limits.MaxTokens = defaults.MaxTokens
	}
	if limits.MaxStringBytes <= 0 {
		limits.MaxStringBytes = defaults.MaxStringBytes
	}
	if limits.MaxContainerEntries <= 0 {
		limits.MaxContainerEntries = defaults.MaxContainerEntries
	}
	return limits
}

func Parse(data []byte) (*Definition, error) {
	return ParseWithLimits(data, DefaultParseLimits())
}

func ParseWithLimits(data []byte, limits ParseLimits) (*Definition, error) {
	limits = normalizeParseLimits(limits)
	if len(data) > limits.MaxBytes {
		return nil, fmt.Errorf("%w: bytes %d > %d", ErrParseLimitExceeded, len(data), limits.MaxBytes)
	}
	if err := rejectDuplicateKeysWithLimits(data, limits); err != nil {
		return nil, err
	}
	return parseDefinition(data)
}

func ParseGenerated(data []byte) (GeneratedResult, error) {
	return ParseGeneratedWithLimits(data, DefaultParseLimits())
}

func ParseGeneratedWithLimits(data []byte, limits ParseLimits) (GeneratedResult, error) {
	limits = normalizeParseLimits(limits)
	if len(data) > limits.MaxBytes {
		return GeneratedResult{}, fmt.Errorf("%w: bytes %d > %d", ErrParseLimitExceeded, len(data), limits.MaxBytes)
	}
	if err := rejectDuplicateKeysWithLimits(data, limits); err != nil {
		return GeneratedResult{}, err
	}
	var root map[string]json.RawMessage
	if err := decodeStrictSingle(data, &root); err != nil {
		return GeneratedResult{}, err
	}
	if _, hasError := root["error"]; hasError {
		var rejection Rejection
		if err := decodeStrictSingle(data, &rejection); err != nil {
			return GeneratedResult{}, err
		}
		if err := validateRejection(rejection); err != nil {
			return GeneratedResult{}, err
		}
		return GeneratedResult{Rejection: &rejection}, nil
	}
	definition, err := parseDefinition(data)
	if err != nil {
		return GeneratedResult{}, err
	}
	return GeneratedResult{Definition: definition}, nil
}

func parseDefinition(data []byte) (*Definition, error) {
	var raw struct {
		Schema              string                     `json:"schema"`
		ID                  string                     `json:"id"`
		Name                string                     `json:"name"`
		Description         string                     `json:"description"`
		Presentation        *SkillPresentation         `json:"presentation"`
		GameplayTags        []string                   `json:"gameplay_tags"`
		Activation          json.RawMessage            `json:"activation"`
		InputSchema         json.RawMessage            `json:"input_schema"`
		CooldownTicks       *Tick                      `json:"cooldown_ticks"`
		GlobalCooldownTicks Tick                       `json:"global_cooldown_ticks"`
		Costs               *[]json.RawMessage         `json:"costs"`
		Memory              map[string]json.RawMessage `json:"memory"`
		PersistentState     map[string]json.RawMessage `json:"persistent_state"`
		InitialPhase        string                     `json:"initial_phase"`
		Phases              []json.RawMessage          `json:"phases"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	if raw.Schema != SchemaV2 {
		return nil, fmt.Errorf("schema must be %q", SchemaV2)
	}
	if raw.ID == "" || raw.Name == "" || raw.Description == "" {
		return nil, errors.New("id, name, and description are required")
	}
	if len(raw.Activation) == 0 {
		return nil, errors.New("activation is required")
	}
	if len(raw.InputSchema) == 0 {
		return nil, errors.New("input_schema is required")
	}
	if raw.CooldownTicks == nil {
		return nil, errors.New("cooldown_ticks is required")
	}
	if raw.Costs == nil {
		return nil, errors.New("costs is required")
	}
	if raw.Memory == nil {
		return nil, errors.New("memory is required")
	}
	if raw.InitialPhase == "" {
		return nil, errors.New("initial_phase is required")
	}
	if len(raw.Phases) == 0 {
		return nil, errors.New("phases must not be empty")
	}
	activation, err := decodeActivation(raw.Activation)
	if err != nil {
		return nil, fmt.Errorf("activation: %w", err)
	}
	inputSchema, err := decodeInputSchema(raw.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("input_schema: %w", err)
	}
	costs, err := decodeCosts(*raw.Costs)
	if err != nil {
		return nil, fmt.Errorf("costs: %w", err)
	}
	memory, err := decodeMemory(raw.Memory)
	if err != nil {
		return nil, fmt.Errorf("memory: %w", err)
	}
	persistentState, err := decodePersistentState(raw.PersistentState)
	if err != nil {
		return nil, fmt.Errorf("persistent_state: %w", err)
	}
	phases, err := decodePhases(raw.Phases)
	if err != nil {
		return nil, fmt.Errorf("phases: %w", err)
	}
	return &Definition{Schema: raw.Schema, ID: raw.ID, Name: raw.Name, Description: raw.Description, Presentation: raw.Presentation, GameplayTags: raw.GameplayTags, Activation: activation, InputSchema: inputSchema, CooldownTicks: *raw.CooldownTicks, GlobalCooldownTicks: raw.GlobalCooldownTicks, Costs: costs, Memory: memory, PersistentState: persistentState, InitialPhase: raw.InitialPhase, Phases: phases}, nil
}

func decodeStrictSingle(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err != nil {
			return err
		}
		return errors.New("multiple JSON values are not allowed")
	}
	return nil
}

func decodeCosts(raw []json.RawMessage) ([]Cost, error) {
	result := make([]Cost, len(raw))
	for i := range raw {
		var item struct {
			Resource string          `json:"resource"`
			Amount   json.RawMessage `json:"amount"`
		}
		if err := decodeStrictSingle(raw[i], &item); err != nil {
			return nil, fmt.Errorf("[%d]: %w", i, err)
		}
		amount, err := decodeValue(item.Amount)
		if err != nil {
			return nil, fmt.Errorf("[%d].amount: %w", i, err)
		}
		result[i] = Cost{Resource: item.Resource, Amount: amount}
	}
	return result, nil
}

func decodeMemory(raw map[string]json.RawMessage) (map[string]MemoryDeclaration, error) {
	result := make(map[string]MemoryDeclaration, len(raw))
	for name, data := range raw {
		var item struct {
			Type    string          `json:"type"`
			Default json.RawMessage `json:"default"`
		}
		if err := decodeStrictSingle(data, &item); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if !isMemoryType(item.Type) {
			return nil, fmt.Errorf("%s: unsupported memory type %q", name, item.Type)
		}
		value, err := decodeValue(item.Default)
		if err != nil {
			return nil, fmt.Errorf("%s.default: %w", name, err)
		}
		result[name] = MemoryDeclaration{Type: item.Type, Default: value}
	}
	return result, nil
}

func isMemoryType(value string) bool {
	switch value {
	case "int", "bool", "entity", "position":
		return true
	default:
		return false
	}
}

func decodePhases(raw []json.RawMessage) ([]PhaseDefinition, error) {
	result := make([]PhaseDefinition, len(raw))
	for i := range raw {
		var item struct {
			ID           string          `json:"id"`
			TimeoutTicks Tick            `json:"timeout_ticks"`
			On           json.RawMessage `json:"on"`
		}
		if err := decodeStrictSingle(raw[i], &item); err != nil {
			return nil, fmt.Errorf("[%d]: %w", i, err)
		}
		if item.ID == "" {
			return nil, fmt.Errorf("[%d].id is required", i)
		}
		events, err := decodePhaseEvents(item.On)
		if err != nil {
			return nil, fmt.Errorf("[%d].on: %w", i, err)
		}
		if events.Enter == nil {
			return nil, fmt.Errorf("[%d].on.enter is required", i)
		}
		result[i] = PhaseDefinition{ID: item.ID, TimeoutTicks: item.TimeoutTicks, On: events}
	}
	return result, nil
}

func decodePhaseEvents(data []byte) (PhaseEventsDefinition, error) {
	var raw struct {
		Enter            json.RawMessage `json:"enter"`
		Recast           json.RawMessage `json:"recast"`
		Cancel           json.RawMessage `json:"cancel"`
		DirectionChanged json.RawMessage `json:"direction_changed"`
		TargetChanged    json.RawMessage `json:"target_changed"`
		Timeout          json.RawMessage `json:"timeout"`
		Release          json.RawMessage `json:"release"`
		Pulse            json.RawMessage `json:"pulse"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return PhaseEventsDefinition{}, err
	}
	result := PhaseEventsDefinition{}
	items := []struct {
		raw    json.RawMessage
		target *FlowDefinition
	}{{raw.Enter, &result.Enter}, {raw.Recast, &result.Recast}, {raw.Cancel, &result.Cancel}, {raw.DirectionChanged, &result.DirectionChanged}, {raw.TargetChanged, &result.TargetChanged}, {raw.Timeout, &result.Timeout}, {raw.Release, &result.Release}, {raw.Pulse, &result.Pulse}}
	for _, item := range items {
		flow, err := decodeOptionalFlow(item.raw)
		if err != nil {
			return PhaseEventsDefinition{}, err
		}
		*item.target = flow
	}
	return result, nil
}

func validateRejection(value Rejection) error {
	if value.Schema != SchemaV2 {
		return fmt.Errorf("schema must be %q", SchemaV2)
	}
	if value.Error.Code != "UNSUPPORTED_CAPABILITY" {
		return fmt.Errorf("unsupported rejection code %q", value.Error.Code)
	}
	if value.Error.Message == "" {
		return errors.New("rejection message is required")
	}
	return nil
}
