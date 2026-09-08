package skill

import (
	"encoding/json"
	"fmt"
)

type ActivationDefinition interface {
	activationDefinition()
}

type ActiveActivationDefinition struct {
	Policy     CastPolicyDefinition
	CastWindow *CastWindowDefinition
	// Concurrent opts this skill out of caster cast-window exclusivity: by
	// default a root activation is rejected with ErrCasterBusy while the
	// caster has another cast in its windup, commit, or recovery window.
	Concurrent bool
}

func (ActiveActivationDefinition) activationDefinition() {}

type PassiveActivationDefinition struct {
	Type          string
	CooldownScope string
	EventFilter   EventFilterDefinition
	ProcPolicy    ProcPolicyDefinition
}

func (PassiveActivationDefinition) activationDefinition() {}

type EventFilterDefinition struct {
	RequiredTags []string `json:"required_tags"`
	ExcludedTags []string `json:"excluded_tags"`
	Elements     []string `json:"elements"`
	DamageTypes  []string `json:"damage_types"`
	Results      []string `json:"results"`
}

type ProcPolicyDefinition struct {
	MaxDepth         int  `json:"max_depth"`
	AllowSelfTrigger bool `json:"allow_self_trigger"`
	OncePerRootEvent bool `json:"once_per_root_event"`
}

type CastPolicyDefinition interface {
	castPolicyDefinition()
}

type TapPolicyDefinition struct{}

func (TapPolicyDefinition) castPolicyDefinition() {}

type TogglePolicyDefinition struct {
	PulseIntervalTicks Tick
	MaxDurationTicks   Tick
	SustainCosts       []Cost
}

func (TogglePolicyDefinition) castPolicyDefinition() {}

type ChargePolicyDefinition struct {
	MaxChargeTicks Tick
	MinChargeBP    int64
	AutoRelease    bool
}

func (ChargePolicyDefinition) castPolicyDefinition() {}

type AmmoPolicyDefinition struct {
	MaxStock      int64
	RechargeTicks Tick
	InitialStock  int64
}

func (AmmoPolicyDefinition) castPolicyDefinition() {}

type HoldPolicyDefinition struct {
	PulseIntervalTicks Tick
	MaxDurationTicks   Tick
	SustainCosts       []Cost
}

func (HoldPolicyDefinition) castPolicyDefinition() {}

func decodeCastWindow(data []byte) (*CastWindowDefinition, error) {
	var raw struct {
		WindupTicks             Tick            `json:"windup_ticks"`
		WindupTicksExpression   json.RawMessage `json:"windup_ticks_expression"`
		WindupTicksMin          Tick            `json:"windup_ticks_min"`
		WindupTicksMax          Tick            `json:"windup_ticks_max"`
		CommitTick              Tick            `json:"commit_tick"`
		RecoveryTicks           Tick            `json:"recovery_ticks"`
		RecoveryTicksExpression json.RawMessage `json:"recovery_ticks_expression"`
		RecoveryTicksMin        Tick            `json:"recovery_ticks_min"`
		RecoveryTicksMax        Tick            `json:"recovery_ticks_max"`
		Movement                string          `json:"movement"`
		Turning                 string          `json:"turning"`
		InterruptTags           []string        `json:"interrupt_tags"`
		RefundBeforeCommit      bool            `json:"refund_before_commit"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return nil, err
	}
	result := &CastWindowDefinition{
		WindupTicks: raw.WindupTicks, WindupTicksMin: raw.WindupTicksMin, WindupTicksMax: raw.WindupTicksMax,
		CommitTick: raw.CommitTick, RecoveryTicks: raw.RecoveryTicks,
		RecoveryTicksMin: raw.RecoveryTicksMin, RecoveryTicksMax: raw.RecoveryTicksMax,
		Movement: raw.Movement, Turning: raw.Turning, InterruptTags: raw.InterruptTags, RefundBeforeCommit: raw.RefundBeforeCommit,
	}
	if len(raw.WindupTicksExpression) != 0 {
		value, err := decodeValue(raw.WindupTicksExpression)
		if err != nil {
			return nil, fmt.Errorf("windup_ticks_expression: %w", err)
		}
		result.WindupTicksExpression = value
	}
	if len(raw.RecoveryTicksExpression) != 0 {
		value, err := decodeValue(raw.RecoveryTicksExpression)
		if err != nil {
			return nil, fmt.Errorf("recovery_ticks_expression: %w", err)
		}
		result.RecoveryTicksExpression = value
	}
	return result, nil
}

func decodeActivation(data []byte) (ActivationDefinition, error) {
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, err
	}
	if header.Type == "active" {
		var raw struct {
			Type       string          `json:"type"`
			Policy     json.RawMessage `json:"policy"`
			CastWindow json.RawMessage `json:"cast_window"`
			Concurrent bool            `json:"concurrent"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		policy, err := decodeCastPolicy(raw.Policy)
		if err != nil {
			return nil, fmt.Errorf("activation.policy: %w", err)
		}
		var castWindow *CastWindowDefinition
		if len(raw.CastWindow) != 0 {
			value, err := decodeCastWindow(raw.CastWindow)
			if err != nil {
				return nil, fmt.Errorf("activation.cast_window: %w", err)
			}
			castWindow = value
		}
		return ActiveActivationDefinition{Policy: policy, CastWindow: castWindow, Concurrent: raw.Concurrent}, nil
	}
	if isPassiveActivation(header.Type) {
		var raw struct {
			Type          string                 `json:"type"`
			CooldownScope string                 `json:"cooldown_scope"`
			EventFilter   *EventFilterDefinition `json:"event_filter"`
			ProcPolicy    *ProcPolicyDefinition  `json:"proc_policy"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		result := PassiveActivationDefinition{Type: raw.Type, CooldownScope: raw.CooldownScope}
		if raw.EventFilter != nil {
			result.EventFilter = *raw.EventFilter
		}
		if raw.ProcPolicy != nil {
			result.ProcPolicy = *raw.ProcPolicy
		}
		return result, nil
	}
	return nil, fmt.Errorf("unsupported activation type %q", header.Type)
}

func isPassiveActivation(value string) bool {
	switch value {
	case "passive_on_hit", "passive_on_damaged", "passive_on_kill", "passive_on_status", "passive_on_resource":
		return true
	default:
		return false
	}
}

func decodeCastPolicy(data []byte) (CastPolicyDefinition, error) {
	if len(data) == 0 {
		return TapPolicyDefinition{}, nil
	}
	var header struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, err
	}
	switch header.Mode {
	case "tap":
		var raw struct {
			Mode string `json:"mode"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		return TapPolicyDefinition{}, nil
	case "toggle", "hold":
		var raw struct {
			Mode               string            `json:"mode"`
			PulseIntervalTicks Tick              `json:"pulse_interval_ticks"`
			MaxDurationTicks   Tick              `json:"max_duration_ticks"`
			SustainCosts       []json.RawMessage `json:"sustain_costs"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		costs, err := decodeCosts(raw.SustainCosts)
		if err != nil {
			return nil, err
		}
		if raw.Mode == "toggle" {
			return TogglePolicyDefinition{PulseIntervalTicks: raw.PulseIntervalTicks, MaxDurationTicks: raw.MaxDurationTicks, SustainCosts: costs}, nil
		}
		return HoldPolicyDefinition{PulseIntervalTicks: raw.PulseIntervalTicks, MaxDurationTicks: raw.MaxDurationTicks, SustainCosts: costs}, nil
	case "charge":
		var raw struct {
			Mode           string `json:"mode"`
			MaxChargeTicks Tick   `json:"max_charge_ticks"`
			MinChargeBP    *int64 `json:"min_charge_bp"`
			AutoRelease    *bool  `json:"auto_release"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		result := ChargePolicyDefinition{MaxChargeTicks: raw.MaxChargeTicks, AutoRelease: true}
		if raw.MinChargeBP != nil {
			result.MinChargeBP = *raw.MinChargeBP
		}
		if raw.AutoRelease != nil {
			result.AutoRelease = *raw.AutoRelease
		}
		return result, nil
	case "ammo":
		var raw struct {
			Mode          string `json:"mode"`
			MaxStock      int64  `json:"max_stock"`
			RechargeTicks Tick   `json:"recharge_ticks"`
			InitialStock  *int64 `json:"initial_stock"`
		}
		if err := decodeStrictSingle(data, &raw); err != nil {
			return nil, err
		}
		result := AmmoPolicyDefinition{MaxStock: raw.MaxStock, RechargeTicks: raw.RechargeTicks, InitialStock: raw.MaxStock}
		if raw.InitialStock != nil {
			result.InitialStock = *raw.InitialStock
		}
		return result, nil
	default:
		return nil, fmt.Errorf("unsupported cast policy %q", header.Mode)
	}
}
