package skillsync

import (
	"errors"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/skill"
	"github.com/tjbdwanghaibo/roost-core/syncstream"
)

var ErrVisibilityEvaluatorRequired = errors.New("skillsync: entity visibility evaluator is required")

type EntityVisibilityEvaluator func(syncstream.Observer, skill.EntityID) (bool, error)

type VisibilityField string

const (
	VisibilityClock               VisibilityField = "clock"
	VisibilityCasts               VisibilityField = "casts"
	VisibilityCooldowns           VisibilityField = "cooldowns"
	VisibilityResources           VisibilityField = "resources"
	VisibilityAbilities           VisibilityField = "abilities"
	VisibilitySpawns              VisibilityField = "spawns"
	VisibilitySpawnSpatial        VisibilityField = "spawn_spatial"
	VisibilityPolicies            VisibilityField = "policies"
	VisibilityPersistentState     VisibilityField = "persistent_state"
	VisibilityPersistentValue     VisibilityField = "persistent_value"
	VisibilityPresentation        VisibilityField = "presentation"
	VisibilityPresentationSpatial VisibilityField = "presentation_spatial"
)

type FieldVisibilityEvaluator func(syncstream.Observer, VisibilityField, string) (bool, error)

type EntityVisibilityPolicy struct {
	Visible           EntityVisibilityEvaluator
	FieldVisible      FieldVisibilityEvaluator
	DefaultDenyFields bool
	RedactSpatial     bool
	RedactOpaque      bool
}

func (policy EntityVisibilityPolicy) visible(observer syncstream.Observer, entity skill.EntityID) (bool, error) {
	if entity == 0 {
		return true, nil
	}
	if policy.Visible == nil {
		return false, ErrVisibilityEvaluatorRequired
	}
	return policy.Visible(observer, entity)
}

func (policy EntityVisibilityPolicy) fieldVisible(observer syncstream.Observer, field VisibilityField, handle string) (bool, error) {
	if policy.FieldVisible != nil {
		return policy.FieldVisible(observer, field, handle)
	}
	return !policy.DefaultDenyFields, nil
}

func (policy EntityVisibilityPolicy) FilterStateSnapshot(observer syncstream.Observer, snapshot skill.RuntimeStateSnapshot) (skill.RuntimeStateSnapshot, error) {
	// Build an explicit projection. Newly added snapshot fields remain hidden
	// until this policy deliberately handles them.
	result := skill.RuntimeStateSnapshot{LatestStateEventSequence: snapshot.LatestStateEventSequence, LatestStateMutationSequence: snapshot.LatestStateMutationSequence, LatestPresentationSequence: snapshot.LatestPresentationSequence}
	clock, err := policy.fieldVisible(observer, VisibilityClock, "")
	if err != nil {
		return result, err
	}
	if clock {
		result.Tick, result.WorldRevision = snapshot.Tick, snapshot.WorldRevision
	}
	casts, err := policy.fieldVisible(observer, VisibilityCasts, "")
	if err != nil {
		return result, err
	}
	if casts {
		for _, value := range snapshot.Casts {
			allowed, err := policy.visible(observer, value.Caster)
			if err != nil {
				return result, err
			}
			if allowed {
				target, err := policy.visible(observer, value.PrimaryTarget)
				if err != nil {
					return result, err
				}
				if !target {
					value.PrimaryTarget = 0
				}
				result.Casts = append(result.Casts, value)
			}
		}
	}
	cooldowns, err := policy.fieldVisible(observer, VisibilityCooldowns, "")
	if err != nil {
		return result, err
	}
	if cooldowns {
		for _, value := range snapshot.Cooldowns {
			allowed, err := policy.visible(observer, value.Caster)
			if err != nil {
				return result, err
			}
			if allowed {
				result.Cooldowns = append(result.Cooldowns, value)
			}
		}
	}
	resources, err := policy.fieldVisible(observer, VisibilityResources, "")
	if err != nil {
		return result, err
	}
	if resources {
		for _, value := range snapshot.SkillResources {
			allowed, err := policy.visible(observer, value.Caster)
			if err != nil {
				return result, err
			}
			if allowed {
				result.SkillResources = append(result.SkillResources, value)
			}
		}
	}
	// ability 与增量一样按具体 handle 问 FieldVisible（mutationVisibilityField），快照与增量才得到同一可见集合。
	// 之前快照只问一次空 handle，按 handle 隐藏的 ability 经快照完整下发（NC-115）。
	for _, value := range snapshot.Abilities {
		fieldAllowed, err := policy.fieldVisible(observer, VisibilityAbilities, abilityHandleReference(value.Handle))
		if err != nil {
			return result, err
		}
		if !fieldAllowed {
			continue
		}
		allowed, err := policy.visible(observer, value.Owner)
		if err != nil {
			return result, err
		}
		if allowed {
			result.Abilities = append(result.Abilities, value)
		}
	}
	spawns, err := policy.fieldVisible(observer, VisibilitySpawns, "")
	if err != nil {
		return result, err
	}
	spawnSpatial, err := policy.fieldVisible(observer, VisibilitySpawnSpatial, "")
	if err != nil {
		return result, err
	}
	if spawns {
		for _, value := range snapshot.Spawns {
			allowed, err := policy.visible(observer, value.Owner)
			if err != nil {
				return result, err
			}
			if allowed {
				target, err := policy.visible(observer, value.LifecycleEntity)
				if err != nil {
					return result, err
				}
				if !target {
					value.LifecycleEntity = 0
				}
				carry, err := policy.visible(observer, value.Motion.CarryTarget)
				if err != nil {
					return result, err
				}
				if !carry {
					value.Motion.CarryTarget = 0
				}
				if policy.RedactSpatial || !spawnSpatial {
					value.Motion.Position, value.Motion.TrajectoryPosition, value.Motion.Origin = skill.Position{}, skill.Position{}, skill.Position{}
					value.Motion.Direction, value.Motion.FrameAnchor = skill.Direction{}, skill.Position{}
				}
				result.Spawns = append(result.Spawns, value)
			}
		}
	}
	policies, err := policy.fieldVisible(observer, VisibilityPolicies, "")
	if err != nil {
		return result, err
	}
	if policies {
		for _, value := range snapshot.ActivePolicies {
			allowed, err := policy.visible(observer, value.Caster)
			if err != nil {
				return result, err
			}
			if allowed {
				result.ActivePolicies = append(result.ActivePolicies, value)
			}
		}
	}
	for _, value := range snapshot.PersistentStates {
		stateAllowed, err := policy.fieldVisible(observer, VisibilityPersistentState, stateHandleReference(value.Handle))
		if err != nil {
			return result, err
		}
		if !stateAllowed {
			continue
		}
		owner, err := policy.visible(observer, value.Binding.Owner)
		if err != nil {
			return result, err
		}
		subject, err := policy.visible(observer, value.Binding.Subject)
		if err != nil {
			return result, err
		}
		if owner && subject {
			valueAllowed, err := policy.fieldVisible(observer, VisibilityPersistentValue, stateHandleReference(value.Handle))
			if err != nil {
				return result, err
			}
			if !valueAllowed {
				continue
			}
			value.Value, err = skill.RedactRuntimeValue(value.Value, skill.RuntimeValueRedactionOptions{EntityVisible: func(entity skill.EntityID) (bool, error) { return policy.visible(observer, entity) }, RedactSpatial: policy.RedactSpatial, RedactOpaque: policy.RedactOpaque})
			if err != nil {
				return result, err
			}
			result.PersistentStates = append(result.PersistentStates, value)
		}
	}
	return result, nil
}

func (policy EntityVisibilityPolicy) FilterStateMutation(observer syncstream.Observer, mutation skill.StateMutation) (skill.StateMutation, bool, error) {
	field, handle := mutationVisibilityField(mutation)
	// 新 mutation 必须先定义可见性投影；不能用默认字段策略放行未知载荷。
	if field == "" {
		return mutation, false, nil
	}
	fieldAllowed, err := policy.fieldVisible(observer, field, handle)
	if err != nil || !fieldAllowed {
		return mutation, false, err
	}
	clock, err := policy.fieldVisible(observer, VisibilityClock, "")
	if err != nil {
		return mutation, false, err
	}
	if !clock {
		mutation.Tick, mutation.WorldRevision = 0, 0
	}
	entity := mutation.Caster
	if entity == 0 {
		entity = mutation.Owner
	}
	if entity == 0 && mutation.Cast != nil {
		entity = mutation.Cast.Caster
	}
	if entity == 0 && mutation.Spawn != nil {
		entity = mutation.Spawn.Owner
	}
	if entity == 0 && mutation.Persistent != nil {
		entity = mutation.Persistent.Binding.Owner
	}
	// persistent remove 不带 Persistent，按 mutation 自身的 Binding 判断，与 upsert / 快照一样要求
	// owner 与 subject 都可见。之前 remove 推不出实体、按“无主”放行，连同不可见实体的 Binding 下发（NC-115）。
	if mutation.Kind == skill.StateMutationPersistentRemove && mutation.Persistent == nil {
		if entity == 0 {
			entity = mutation.Binding.Owner
		}
		subject, err := policy.visible(observer, mutation.Binding.Subject)
		if err != nil || !subject {
			return mutation, false, err
		}
	}
	allowed, err := policy.visible(observer, entity)
	if err != nil || !allowed {
		return mutation, false, err
	}
	if mutation.Cast != nil {
		target, err := policy.visible(observer, mutation.Cast.PrimaryTarget)
		if err != nil {
			return mutation, false, err
		}
		if !target {
			copyValue := *mutation.Cast
			copyValue.PrimaryTarget = 0
			mutation.Cast = &copyValue
		}
	}
	if mutation.Spawn != nil {
		target, err := policy.visible(observer, mutation.Spawn.LifecycleEntity)
		if err != nil {
			return mutation, false, err
		}
		if !target {
			copyValue := *mutation.Spawn
			copyValue.LifecycleEntity = 0
			mutation.Spawn = &copyValue
		}
		copyValue := *mutation.Spawn
		carry, err := policy.visible(observer, copyValue.Motion.CarryTarget)
		if err != nil {
			return mutation, false, err
		}
		if !carry {
			copyValue.Motion.CarryTarget = 0
		}
		spatial, err := policy.fieldVisible(observer, VisibilitySpawnSpatial, "")
		if err != nil {
			return mutation, false, err
		}
		if policy.RedactSpatial || !spatial {
			copyValue.Motion.Position, copyValue.Motion.TrajectoryPosition, copyValue.Motion.Origin = skill.Position{}, skill.Position{}, skill.Position{}
			copyValue.Motion.Direction, copyValue.Motion.FrameAnchor = skill.Direction{}, skill.Position{}
		}
		mutation.Spawn = &copyValue
	}
	if mutation.Persistent != nil {
		subject, err := policy.visible(observer, mutation.Persistent.Binding.Subject)
		if err != nil || !subject {
			return mutation, false, err
		}
		valueAllowed, err := policy.fieldVisible(observer, VisibilityPersistentValue, stateHandleReference(mutation.Persistent.Handle))
		if err != nil || !valueAllowed {
			return mutation, false, err
		}
		copyValue := *mutation.Persistent
		copyValue.Value, err = skill.RedactRuntimeValue(copyValue.Value, skill.RuntimeValueRedactionOptions{EntityVisible: func(entity skill.EntityID) (bool, error) { return policy.visible(observer, entity) }, RedactSpatial: policy.RedactSpatial, RedactOpaque: policy.RedactOpaque})
		if err != nil {
			return mutation, false, err
		}
		mutation.Persistent = &copyValue
	}
	return mutation, true, nil
}

func (policy EntityVisibilityPolicy) FilterPresentation(observer syncstream.Observer, event skill.PresentationEvent) (skill.PresentationEvent, bool, error) {
	fieldAllowed, err := policy.fieldVisible(observer, VisibilityPresentation, "")
	if err != nil || !fieldAllowed {
		return event, false, err
	}
	clock, err := policy.fieldVisible(observer, VisibilityClock, "")
	if err != nil {
		return event, false, err
	}
	if !clock {
		event.Tick, event.WorldRevision = 0, 0
	}
	allowedSource, err := policy.visible(observer, event.Source)
	if err != nil || !allowedSource {
		return event, false, err
	}
	allowedAnchorSource, err := policy.visible(observer, event.Anchor.Source)
	if err != nil || !allowedAnchorSource {
		return event, false, err
	}
	anchorTarget, err := policy.visible(observer, event.Anchor.Target)
	if err != nil {
		return event, false, err
	}
	if !anchorTarget {
		event.Anchor.Target = 0
	}
	primaryTarget, err := policy.visible(observer, event.PrimaryTarget)
	if err != nil {
		return event, false, err
	}
	if !primaryTarget {
		event.PrimaryTarget = 0
	}
	spatial, err := policy.fieldVisible(observer, VisibilityPresentationSpatial, "")
	if err != nil {
		return event, false, err
	}
	if policy.RedactSpatial || !spatial {
		event.Anchor.Position, event.Anchor.Direction, event.Anchor.Path = nil, nil, nil
	}
	return event, true, nil
}

func mutationVisibilityField(mutation skill.StateMutation) (VisibilityField, string) {
	switch mutation.Kind {
	case skill.StateMutationClock:
		return VisibilityClock, ""
	case skill.StateMutationCastUpsert, skill.StateMutationCastRemove:
		return VisibilityCasts, ""
	case skill.StateMutationCooldownUpsert, skill.StateMutationCooldownRemove:
		return VisibilityCooldowns, ""
	case skill.StateMutationResourceUpsert, skill.StateMutationResourceRemove:
		return VisibilityResources, ""
	case skill.StateMutationAbilityUpsert, skill.StateMutationAbilityRemove:
		return VisibilityAbilities, abilityHandleReference(mutation.AbilityHandle)
	case skill.StateMutationSpawnUpsert, skill.StateMutationSpawnRemove:
		return VisibilitySpawns, ""
	case skill.StateMutationPolicyUpsert, skill.StateMutationPolicyRemove:
		return VisibilityPolicies, ""
	case skill.StateMutationPersistentUpsert, skill.StateMutationPersistentRemove:
		handle := mutation.StateHandle
		if handle == (skill.StateHandle{}) && mutation.Persistent != nil {
			handle = mutation.Persistent.Handle
		}
		return VisibilityPersistentState, stateHandleReference(handle)
	default:
		return "", ""
	}
}

// abilityHandleReference 是 ability 字段可见性的 handle 键，快照与增量共用。
func abilityHandleReference(handle skill.AbilityHandle) string { return fmt.Sprint(handle) }

func stateHandleReference(handle skill.StateHandle) string {
	return fmt.Sprintf("%s:%d:%d", handle.GameplayDigest, handle.Slot, handle.Shared)
}
