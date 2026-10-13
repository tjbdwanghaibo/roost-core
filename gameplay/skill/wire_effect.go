package skill

import (
	"encoding/json"
	"fmt"
)

type EffectDefinition interface{ effectDefinition() }

type DamageEffectDefinition struct {
	Target, Amount      Value
	DamageType, Element string
	CombatTags          []string
	CanCritical         bool
	Visual              *VisualRef
}

func (DamageEffectDefinition) effectDefinition() {}

type HealEffectDefinition struct {
	Target, Amount Value
	Visual         *VisualRef
}

func (HealEffectDefinition) effectDefinition() {}

type ShieldEffectDefinition struct {
	Target, Amount Value
	DurationTicks  Tick
	Visual         *VisualRef
}

func (ShieldEffectDefinition) effectDefinition() {}

type AddStatusEffectDefinition struct {
	Target        Value
	Status        string
	DurationTicks Tick
	Stacks        int
	MaxStacks     *int
	Visual        *VisualRef
}

func (AddStatusEffectDefinition) effectDefinition() {}

type RemoveStatusEffectDefinition struct {
	Target Value
	Status string
	Visual *VisualRef
}

func (RemoveStatusEffectDefinition) effectDefinition() {}

type ModifyStatusInstanceEffectDefinition struct {
	Status                     Value
	Value, Target              *Value
	Operation, OwnershipPolicy string
}

func (ModifyStatusInstanceEffectDefinition) effectDefinition() {}

type AttributeModifierEffectDefinition struct {
	Target               Value
	Attribute, Operation string
	Value                Value
	DurationTicks        Tick
	StackPolicy          string
	MaxStacks            int
	Visual               *VisualRef
}

func (AttributeModifierEffectDefinition) effectDefinition() {}

type ResourceEffectDefinition struct {
	Target, Amount      Value
	Resource, Operation string
	Visual              *VisualRef
}

func (ResourceEffectDefinition) effectDefinition() {}

type SetMemoryEffectDefinition struct {
	Name  string
	Value Value
}

func (SetMemoryEffectDefinition) effectDefinition() {}

type AddMemoryEffectDefinition struct {
	Name  string
	Value Value
}

func (AddMemoryEffectDefinition) effectDefinition() {}

type ClearMemoryEffectDefinition struct{ Name string }

func (ClearMemoryEffectDefinition) effectDefinition() {}

type TeleportEffectDefinition struct {
	Target, Destination Value
	OnBlocked           string
	Visual              *VisualRef
}

func (TeleportEffectDefinition) effectDefinition() {}

type KnockbackEffectDefinition struct {
	Target, From, Distance Value
	Visual                 *VisualRef
}

func (KnockbackEffectDefinition) effectDefinition() {}

type PullEffectDefinition struct {
	Target, Toward, Distance Value
	Visual                   *VisualRef
}

func (PullEffectDefinition) effectDefinition() {}

type StopMovementEffectDefinition struct {
	Target Value
	Visual *VisualRef
}

func (StopMovementEffectDefinition) effectDefinition() {}

type ModifyStateEffectDefinition struct {
	State         string
	Owner         *Value
	Subject       *Value
	TeamOf        *Value
	Operation     string
	Value         *Value
	DurationTicks Tick
	ExpiryPolicy  string
}

func (ModifyStateEffectDefinition) effectDefinition() {}

type ModifyAbilityStateEffectDefinition struct {
	Owner         Value
	Ability       Value
	Property      string
	Operation     string
	Value         Value
	DurationTicks Tick
}

func (ModifyAbilityStateEffectDefinition) effectDefinition() {}

type NumericTrackDefinition struct {
	Property, Operation string
	Value               Value
	OverTicks           Tick
}

type ModifySpawnEffectDefinition struct {
	Spawn               Value
	Property, Operation string
	Value               Value
	OverTicks           Tick
}

func (ModifySpawnEffectDefinition) effectDefinition() {}

type SummonEffectDefinition struct {
	Template           string
	Position           Value
	Count              int
	DurationTicks      Tick
	AttributeOverrides map[string]Value
	ParameterBindings  map[string]Value
}

func (SummonEffectDefinition) effectDefinition() {}

type DismissEffectDefinition struct{ Target Value }

func (DismissEffectDefinition) effectDefinition() {}

type IssueEntityCommandEffectDefinition struct {
	Target       Value
	Command      string
	Position     *Value
	TargetEntity *Value
	Behavior     string
}

func (IssueEntityCommandEffectDefinition) effectDefinition() {}

// decodeEffect 按 type 分派到各效果类型的解码函数。每个解码函数严格解码一种效果（拒绝未知字段与尾随数据），
// 并把其中的表达式字段解码成 Value。
func decodeEffect(data []byte) (EffectDefinition, error) {
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, err
	}
	switch header.Type {
	case "capture_snapshot":
		return decodeCaptureSnapshotEffect(data)
	case "restore_snapshot":
		return decodeRestoreSnapshotEffect(data)
	case "damage":
		return decodeDamageEffect(data)
	case "heal":
		return decodeHealEffect(data)
	case "shield":
		return decodeShieldEffect(data)
	case "add_status":
		return decodeAddStatusEffect(data)
	case "remove_status":
		return decodeRemoveStatusEffect(data)
	case "modify_status_instance":
		return decodeModifyStatusInstanceEffect(data)
	case "attribute_modifier":
		return decodeAttributeModifierEffect(data)
	case "resource":
		return decodeResourceEffect(data)
	case "set_memory", "add_memory":
		return decodeMemoryWriteEffect(data)
	case "clear_memory":
		return decodeClearMemoryEffect(data)
	case "teleport":
		return decodeTeleportEffect(data)
	case "knockback":
		return decodeKnockbackEffect(data)
	case "pull":
		return decodePullEffect(data)
	case "stop_movement":
		return decodeStopMovementEffect(data)
	case "modify_state":
		return decodeModifyStateEffect(data)
	case "modify_ability_state":
		return decodeModifyAbilityStateEffect(data)
	case "modify_spawn":
		return decodeModifySpawnEffect(data)
	case "summon":
		return decodeSummonEffect(data)
	case "dismiss":
		return decodeDismissEffect(data)
	case "issue_entity_command":
		return decodeIssueEntityCommandEffect(data)
	default:
		return nil, fmt.Errorf("unsupported effect %q", header.Type)
	}
}

// decodeDamageEffect 解码 damage 效果。
func decodeDamageEffect(data []byte) (EffectDefinition, error) {
	var raw struct {
		Type        string          `json:"type"`
		Target      json.RawMessage `json:"target"`
		Amount      json.RawMessage `json:"amount"`
		DamageType  string          `json:"damage_type"`
		Element     string          `json:"element"`
		CombatTags  []string        `json:"combat_tags"`
		CanCritical *bool           `json:"can_critical"`
		Visual      *VisualRef      `json:"visual"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	values, err := decodeValueList(raw.Target, raw.Amount)
	if err != nil {
		return nil, err
	}
	result := DamageEffectDefinition{Target: values[0], Amount: values[1], DamageType: raw.DamageType, Element: raw.Element, CombatTags: raw.CombatTags, Visual: raw.Visual}
	if raw.CanCritical != nil {
		result.CanCritical = *raw.CanCritical
	}
	return result, nil
}

// decodeHealEffect 解码 heal 效果。
func decodeHealEffect(data []byte) (EffectDefinition, error) {
	var raw struct {
		Type   string          `json:"type"`
		Target json.RawMessage `json:"target"`
		Amount json.RawMessage `json:"amount"`
		Visual *VisualRef      `json:"visual"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	values, err := decodeValueList(raw.Target, raw.Amount)
	if err != nil {
		return nil, err
	}
	return HealEffectDefinition{Target: values[0], Amount: values[1], Visual: raw.Visual}, nil
}

// decodeShieldEffect 解码 shield 效果。
func decodeShieldEffect(data []byte) (EffectDefinition, error) {
	var raw struct {
		Type          string          `json:"type"`
		Target        json.RawMessage `json:"target"`
		Amount        json.RawMessage `json:"amount"`
		DurationTicks Tick            `json:"duration_ticks"`
		Visual        *VisualRef      `json:"visual"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	values, err := decodeValueList(raw.Target, raw.Amount)
	if err != nil {
		return nil, err
	}
	return ShieldEffectDefinition{Target: values[0], Amount: values[1], DurationTicks: raw.DurationTicks, Visual: raw.Visual}, nil
}

// decodeAddStatusEffect 解码 add_status 效果。
func decodeAddStatusEffect(data []byte) (EffectDefinition, error) {
	var raw struct {
		Type          string          `json:"type"`
		Target        json.RawMessage `json:"target"`
		Status        string          `json:"status"`
		DurationTicks Tick            `json:"duration_ticks"`
		Stacks        int             `json:"stacks"`
		MaxStacks     *int            `json:"max_stacks"`
		Visual        *VisualRef      `json:"visual"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	target, err := decodeValue(raw.Target)
	if err != nil {
		return nil, err
	}
	return AddStatusEffectDefinition{Target: target, Status: raw.Status, DurationTicks: raw.DurationTicks, Stacks: raw.Stacks, MaxStacks: raw.MaxStacks, Visual: raw.Visual}, nil
}

// decodeRemoveStatusEffect 解码 remove_status 效果。
func decodeRemoveStatusEffect(data []byte) (EffectDefinition, error) {
	var raw struct {
		Type   string          `json:"type"`
		Target json.RawMessage `json:"target"`
		Status string          `json:"status"`
		Visual *VisualRef      `json:"visual"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	target, err := decodeValue(raw.Target)
	if err != nil {
		return nil, err
	}
	return RemoveStatusEffectDefinition{Target: target, Status: raw.Status, Visual: raw.Visual}, nil
}

// decodeModifyStatusInstanceEffect 解码 modify_status_instance 效果。
func decodeModifyStatusInstanceEffect(data []byte) (EffectDefinition, error) {
	var raw struct {
		Type            string          `json:"type"`
		Status          json.RawMessage `json:"status"`
		Operation       string          `json:"operation"`
		Value           json.RawMessage `json:"value"`
		Target          json.RawMessage `json:"target"`
		OwnershipPolicy string          `json:"ownership_policy"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	status, err := decodeValue(raw.Status)
	if err != nil {
		return nil, err
	}
	var value, target *Value
	if len(raw.Value) != 0 {
		decoded, decodeErr := decodeValue(raw.Value)
		err = decodeErr
		if err != nil {
			return nil, err
		}
		value = &decoded
	}
	if len(raw.Target) != 0 {
		decoded, decodeErr := decodeValue(raw.Target)
		err = decodeErr
		if err != nil {
			return nil, err
		}
		target = &decoded
	}
	return ModifyStatusInstanceEffectDefinition{Status: status, Operation: raw.Operation, Value: value, Target: target, OwnershipPolicy: raw.OwnershipPolicy}, nil
}

// decodeAttributeModifierEffect 解码 attribute_modifier 效果。
func decodeAttributeModifierEffect(data []byte) (EffectDefinition, error) {
	var raw struct {
		Type          string          `json:"type"`
		Target        json.RawMessage `json:"target"`
		Attribute     string          `json:"attribute"`
		Operation     string          `json:"operation"`
		Value         json.RawMessage `json:"value"`
		DurationTicks Tick            `json:"duration_ticks"`
		StackPolicy   string          `json:"stack_policy"`
		MaxStacks     int             `json:"max_stacks"`
		Visual        *VisualRef      `json:"visual"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	values, err := decodeValueList(raw.Target, raw.Value)
	if err != nil {
		return nil, err
	}
	return AttributeModifierEffectDefinition{Target: values[0], Attribute: raw.Attribute, Operation: raw.Operation, Value: values[1], DurationTicks: raw.DurationTicks, StackPolicy: raw.StackPolicy, MaxStacks: raw.MaxStacks, Visual: raw.Visual}, nil
}

// decodeResourceEffect 解码 resource 效果。
func decodeResourceEffect(data []byte) (EffectDefinition, error) {
	var raw struct {
		Type      string          `json:"type"`
		Target    json.RawMessage `json:"target"`
		Amount    json.RawMessage `json:"amount"`
		Resource  string          `json:"resource"`
		Operation string          `json:"operation"`
		Visual    *VisualRef      `json:"visual"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	values, err := decodeValueList(raw.Target, raw.Amount)
	if err != nil {
		return nil, err
	}
	return ResourceEffectDefinition{Target: values[0], Amount: values[1], Resource: raw.Resource, Operation: raw.Operation, Visual: raw.Visual}, nil
}

// decodeMemoryWriteEffect 解码 set_memory / add_memory 效果（set_memory 与 add_memory 同形，按 type 区分）。
func decodeMemoryWriteEffect(data []byte) (EffectDefinition, error) {
	var raw struct {
		Type  string          `json:"type"`
		Name  string          `json:"name"`
		Value json.RawMessage `json:"value"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	value, err := decodeValue(raw.Value)
	if err != nil {
		return nil, err
	}
	if raw.Type == "set_memory" {
		return SetMemoryEffectDefinition{Name: raw.Name, Value: value}, nil
	}
	return AddMemoryEffectDefinition{Name: raw.Name, Value: value}, nil
}

// decodeClearMemoryEffect 解码 clear_memory 效果。
func decodeClearMemoryEffect(data []byte) (EffectDefinition, error) {
	var raw struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	return ClearMemoryEffectDefinition{Name: raw.Name}, nil
}

// decodeTeleportEffect 解码 teleport 效果。
func decodeTeleportEffect(data []byte) (EffectDefinition, error) {
	var raw struct {
		Type        string          `json:"type"`
		Target      json.RawMessage `json:"target"`
		Destination json.RawMessage `json:"destination"`
		OnBlocked   string          `json:"on_blocked"`
		Visual      *VisualRef      `json:"visual"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	values, err := decodeValueList(raw.Target, raw.Destination)
	if err != nil {
		return nil, err
	}
	return TeleportEffectDefinition{Target: values[0], Destination: values[1], OnBlocked: raw.OnBlocked, Visual: raw.Visual}, nil
}

// decodeKnockbackEffect 解码 knockback 效果。
func decodeKnockbackEffect(data []byte) (EffectDefinition, error) {
	var raw struct {
		Type     string          `json:"type"`
		Target   json.RawMessage `json:"target"`
		From     json.RawMessage `json:"from"`
		Distance json.RawMessage `json:"distance"`
		Visual   *VisualRef      `json:"visual"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	target, err := decodeValue(raw.Target)
	if err != nil {
		return nil, err
	}
	distance, err := decodeValue(raw.Distance)
	if err != nil {
		return nil, err
	}
	from, err := decodeValue(raw.From)
	if err != nil {
		return nil, err
	}
	return KnockbackEffectDefinition{Target: target, From: from, Distance: distance, Visual: raw.Visual}, nil
}

// decodePullEffect 解码 pull 效果。
func decodePullEffect(data []byte) (EffectDefinition, error) {
	var raw struct {
		Type     string          `json:"type"`
		Target   json.RawMessage `json:"target"`
		Toward   json.RawMessage `json:"toward"`
		Distance json.RawMessage `json:"distance"`
		Visual   *VisualRef      `json:"visual"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	target, err := decodeValue(raw.Target)
	if err != nil {
		return nil, err
	}
	distance, err := decodeValue(raw.Distance)
	if err != nil {
		return nil, err
	}
	toward, err := decodeValue(raw.Toward)
	if err != nil {
		return nil, err
	}
	return PullEffectDefinition{Target: target, Toward: toward, Distance: distance, Visual: raw.Visual}, nil
}

// decodeStopMovementEffect 解码 stop_movement 效果。
func decodeStopMovementEffect(data []byte) (EffectDefinition, error) {
	var raw struct {
		Type   string          `json:"type"`
		Target json.RawMessage `json:"target"`
		Visual *VisualRef      `json:"visual"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	target, err := decodeValue(raw.Target)
	if err != nil {
		return nil, err
	}
	return StopMovementEffectDefinition{Target: target, Visual: raw.Visual}, nil
}

// decodeModifyStateEffect 解码 modify_state 效果。
func decodeModifyStateEffect(data []byte) (EffectDefinition, error) {
	var raw struct {
		Type          string          `json:"type"`
		State         string          `json:"state"`
		Owner         json.RawMessage `json:"owner"`
		Subject       json.RawMessage `json:"subject"`
		TeamOf        json.RawMessage `json:"team_of"`
		Operation     string          `json:"operation"`
		Value         json.RawMessage `json:"value"`
		DurationTicks Tick            `json:"duration_ticks"`
		ExpiryPolicy  string          `json:"expiry_policy"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	owner, err := decodeOptionalValue(raw.Owner)
	if err != nil {
		return nil, err
	}
	subject, err := decodeOptionalValue(raw.Subject)
	if err != nil {
		return nil, err
	}
	teamOf, err := decodeOptionalValue(raw.TeamOf)
	if err != nil {
		return nil, err
	}
	value, err := decodeOptionalValue(raw.Value)
	if err != nil {
		return nil, err
	}
	return ModifyStateEffectDefinition{State: raw.State, Owner: owner, Subject: subject, TeamOf: teamOf, Operation: raw.Operation, Value: value, DurationTicks: raw.DurationTicks, ExpiryPolicy: raw.ExpiryPolicy}, nil
}

// decodeModifyAbilityStateEffect 解码 modify_ability_state 效果。
func decodeModifyAbilityStateEffect(data []byte) (EffectDefinition, error) {
	var raw struct {
		Type          string          `json:"type"`
		Owner         json.RawMessage `json:"owner"`
		Ability       json.RawMessage `json:"ability"`
		Property      string          `json:"property"`
		Operation     string          `json:"operation"`
		Value         json.RawMessage `json:"value"`
		DurationTicks Tick            `json:"duration_ticks"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	values, err := decodeValueList(raw.Owner, raw.Ability, raw.Value)
	if err != nil {
		return nil, err
	}
	return ModifyAbilityStateEffectDefinition{Owner: values[0], Ability: values[1], Property: raw.Property, Operation: raw.Operation, Value: values[2], DurationTicks: raw.DurationTicks}, nil
}

// decodeModifySpawnEffect 解码 modify_spawn 效果。
func decodeModifySpawnEffect(data []byte) (EffectDefinition, error) {
	var raw struct {
		Type      string          `json:"type"`
		Spawn     json.RawMessage `json:"spawn"`
		Property  string          `json:"property"`
		Operation string          `json:"operation"`
		Value     json.RawMessage `json:"value"`
		OverTicks Tick            `json:"over_ticks"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	values, err := decodeValueList(raw.Spawn, raw.Value)
	if err != nil {
		return nil, err
	}
	return ModifySpawnEffectDefinition{Spawn: values[0], Property: raw.Property, Operation: raw.Operation, Value: values[1], OverTicks: raw.OverTicks}, nil
}

// decodeSummonEffect 解码 summon 效果。
func decodeSummonEffect(data []byte) (EffectDefinition, error) {
	var raw struct {
		Type               string                     `json:"type"`
		Template           string                     `json:"template"`
		Position           json.RawMessage            `json:"position"`
		Count              int                        `json:"count"`
		DurationTicks      Tick                       `json:"duration_ticks"`
		AttributeOverrides map[string]json.RawMessage `json:"attribute_overrides"`
		ParameterBindings  map[string]json.RawMessage `json:"parameter_bindings"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	position, err := decodeValue(raw.Position)
	if err != nil {
		return nil, err
	}
	overrides, err := decodeWireValueMap(raw.AttributeOverrides)
	if err != nil {
		return nil, fmt.Errorf("attribute_overrides: %w", err)
	}
	parameters, err := decodeWireValueMap(raw.ParameterBindings)
	if err != nil {
		return nil, fmt.Errorf("parameter_bindings: %w", err)
	}
	return SummonEffectDefinition{Template: raw.Template, Position: position, Count: raw.Count, DurationTicks: raw.DurationTicks, AttributeOverrides: overrides, ParameterBindings: parameters}, nil
}

// decodeDismissEffect 解码 dismiss 效果。
func decodeDismissEffect(data []byte) (EffectDefinition, error) {
	var raw struct {
		Type   string          `json:"type"`
		Target json.RawMessage `json:"target"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	target, err := decodeValue(raw.Target)
	if err != nil {
		return nil, err
	}
	return DismissEffectDefinition{Target: target}, nil
}

// decodeIssueEntityCommandEffect 解码 issue_entity_command 效果。
func decodeIssueEntityCommandEffect(data []byte) (EffectDefinition, error) {
	var raw struct {
		Type         string          `json:"type"`
		Target       json.RawMessage `json:"target"`
		Command      string          `json:"command"`
		Position     json.RawMessage `json:"position"`
		TargetEntity json.RawMessage `json:"target_entity"`
		Behavior     string          `json:"behavior"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	target, err := decodeValue(raw.Target)
	if err != nil {
		return nil, err
	}
	position, err := decodeOptionalWireValue(raw.Position)
	if err != nil {
		return nil, err
	}
	targetEntity, err := decodeOptionalWireValue(raw.TargetEntity)
	if err != nil {
		return nil, err
	}
	return IssueEntityCommandEffectDefinition{Target: target, Command: raw.Command, Position: position, TargetEntity: targetEntity, Behavior: raw.Behavior}, nil
}

func decodeWireValueMap(raw map[string]json.RawMessage) (map[string]Value, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	result := make(map[string]Value, len(raw))
	for key, encoded := range raw {
		if key == "" {
			return nil, fmt.Errorf("binding name is required")
		}
		value, err := decodeValue(encoded)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		result[key] = value
	}
	return result, nil
}

func decodeOptionalWireValue(raw json.RawMessage) (*Value, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	value, err := decodeValue(raw)
	if err != nil {
		return nil, err
	}
	return &value, nil
}
