package skill

import "errors"

var ErrRuntimeValueVisibilityRequired = errors.New("skill: runtime value entity visibility evaluator is required")

type RuntimeValueRedactionOptions struct {
	EntityVisible func(EntityID) (bool, error)
	RedactSpatial bool
	RedactOpaque  bool
}

// RedactRuntimeValue recursively removes entity, spatial, and opaque references
// from a detached RuntimeValue. Hidden scalar references become typed missing
// values; hidden members are removed from entity lists.
func RedactRuntimeValue(value RuntimeValue, options RuntimeValueRedactionOptions) (RuntimeValue, error) {
	if !value.present {
		return cloneRuntimeValue(value), nil
	}
	visible := func(entity EntityID) (bool, error) {
		if entity == 0 {
			return true, nil
		}
		if options.EntityVisible == nil {
			return false, ErrRuntimeValueVisibilityRequired
		}
		return options.EntityVisible(entity)
	}
	missing := func() RuntimeValue { return MissingRuntimeValue(value.typ) }
	switch value.typ.Base {
	case valueKindEntity:
		allowed, err := visible(value.entity)
		if err != nil {
			return RuntimeValue{}, err
		}
		if !allowed {
			return missing(), nil
		}
	case valueKindEntityList:
		entities := make([]EntityID, 0, len(value.entities))
		for _, entity := range value.entities {
			allowed, err := visible(entity)
			if err != nil {
				return RuntimeValue{}, err
			}
			if allowed {
				entities = append(entities, entity)
			}
		}
		value.entities = entities
	case valueKindAbility:
		allowed, err := visible(value.ability.Owner)
		if err != nil {
			return RuntimeValue{}, err
		}
		if !allowed {
			return missing(), nil
		}
	case valueKindStatusInstance:
		allowed, err := visible(value.status.Target)
		if err != nil {
			return RuntimeValue{}, err
		}
		if !allowed {
			return missing(), nil
		}
	case valueKindHit:
		allowed, err := visible(value.hit.Entity)
		if err != nil {
			return RuntimeValue{}, err
		}
		if !allowed || options.RedactSpatial {
			return missing(), nil
		}
	case valueKindPosition, valueKindDirection, valueKindPath:
		if options.RedactSpatial {
			return missing(), nil
		}
	case valueKindSnapshotToken, valueKindProcess:
		if options.RedactOpaque {
			return missing(), nil
		}
	case valueKindEffectResult:
		fields := make([]RuntimeValue, len(value.effectResult.fields))
		for index := range value.effectResult.fields {
			redacted, err := RedactRuntimeValue(value.effectResult.fields[index], options)
			if err != nil {
				return RuntimeValue{}, err
			}
			fields[index] = redacted
		}
		value.effectResult.fields = fields
	}
	return cloneRuntimeValue(value), nil
}
