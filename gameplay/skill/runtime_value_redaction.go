package skill

import "errors"

// ErrRuntimeValueVisibilityRequired 表示脱敏需要实体可见性判断，但 EntityVisible 为空。
var ErrRuntimeValueVisibilityRequired = errors.New("skill: runtime value entity visibility evaluator is required")

// RuntimeValueRedactionOptions 控制 RedactRuntimeValue 去掉哪些引用。
type RuntimeValueRedactionOptions struct {
	EntityVisible func(EntityID) (bool, error)
	RedactSpatial bool
	RedactOpaque  bool
}

// RedactRuntimeValue 从一个脱离 Runtime 的 RuntimeValue 里递归去掉实体、空间与不透明引用。被隐藏的标量引用变成
// 带类型的缺省值；实体列表里被隐藏的成员直接移除。
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
	case valueKindSnapshotToken, valueKindSpawn:
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
