package skill

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"sort"
)

func (runtime *Runtime) executeQuery(cast *castInstance, selector selectorProgram) (flowControl, error) {
	var elements []selectionElement
	if selector.element == selectionAbility && selector.shapePlan.kind == "ability_set" {
		var err error
		elements, err = runtime.selectAbilities(cast, selector)
		if err != nil {
			return flowControl{}, err
		}
	} else {
		request, err := runtime.buildSelectRequest(cast, selector)
		if err != nil {
			return flowControl{}, err
		}
		result, err := runtime.host.Select(request)
		if err != nil {
			return flowControl{}, err
		}
		cast.visibleRevision = maxRevision(cast.visibleRevision, result.Meta.Revision)
		elements = append([]selectionElement(nil), result.Selection.elements...)
	}
	if selector.hasRandomSite {
		invocation := cast.randomInvocations[selector.randomSite]
		cast.randomInvocations[selector.randomSite] = invocation + 1
		// Precompute one HMAC score per candidate: the comparator used to
		// rehash both sides on every comparison, costing O(n log n) SHA256
		// invocations per query instead of O(n).
		type scoredElement struct {
			element selectionElement
			id      uint64
			score   [32]byte
		}
		scored := make([]scoredElement, len(elements))
		for index, element := range elements {
			id := stableSelectionID(element, selector.element)
			scored[index] = scoredElement{element: element, id: id, score: randomCandidateScore(cast.randomKey, selector.randomSite, invocation, id)}
		}
		sort.SliceStable(scored, func(i, j int) bool {
			if comparison := bytes.Compare(scored[i].score[:], scored[j].score[:]); comparison != 0 {
				return comparison < 0
			}
			return scored[i].id < scored[j].id
		})
		for index := range scored {
			elements[index] = scored[index].element
		}
	}
	if selector.limit > 0 && len(elements) > selector.limit {
		elements = elements[:selector.limit]
	}
	if len(elements) == 0 {
		if selector.hasEmpty {
			return runtime.executeOperation(cast, selector.emptyRoot)
		}
		return flowControl{}, nil
	}
	if !selector.hasConsumer || int(selector.consumerLocal) >= len(cast.locals) {
		return flowControl{}, ErrProgramInvariant
	}
	previous := cast.locals[selector.consumerLocal]
	defer func() { cast.locals[selector.consumerLocal] = previous }()
	consume := func(element selectionElement) (flowControl, error) {
		value, valueErr := selectionRuntimeValue(element, selector.element)
		if valueErr != nil {
			return flowControl{}, valueErr
		}
		cast.locals[selector.consumerLocal] = value
		return runtime.executeOperation(cast, selector.consumerRoot)
	}
	if selector.consumerMode == consumerOne {
		return consume(elements[0])
	}
	for _, element := range elements {
		control, consumeErr := consume(element)
		if consumeErr != nil {
			return control, consumeErr
		}
		if control.kind == flowSuspend {
			if err := runtime.schedule(cast, control.dueTick, control.payload); err != nil {
				return flowControl{}, err
			}
			continue
		}
		if control.kind != flowContinue {
			return control, nil
		}
	}
	return flowControl{kind: flowContinue}, nil
}

func (runtime *Runtime) selectAbilities(cast *castInstance, selector selectorProgram) ([]selectionElement, error) {
	from, err := runtime.evalValue(cast, selector.from)
	if err != nil {
		return nil, err
	}
	owner, ok := from.Entity()
	if !ok {
		return nil, ErrRuntimeTypeMismatch
	}
	// Ability state is private runtime state. Cross-owner enumeration is never
	// delegated to the world host and must be explicitly rejected.
	if !runtime.abilityOwnerAllowed(cast.program, cast.caster, owner) {
		return nil, ErrCastInputRejected
	}
	states := runtime.abilitySelection(owner)
	elements := make([]selectionElement, 0, len(states))
	for _, state := range states {
		matched := true
		for _, filter := range selector.filters {
			switch filter.kind {
			case "ability_tag":
				matched = containsGameplayTag(state.tags, filter.tag)
			case "ability_slot":
				matched = state.slot == filter.slot
			case "self_ability":
				matched = state.handle == cast.ability
			case "not_self_ability":
				matched = state.handle != cast.ability
			case "ability_enabled":
				matched = len(state.overlays) == 0
			case "ability_on_cooldown":
				matched = runtime.cooldowns[cooldownKey{Caster: owner, Skill: state.program.id}] > runtime.currentTick
			case "ability_has_ammo":
				stock, readErr := runtime.readAbilityStateLocked(owner, state.handle, "ammo_stock")
				if readErr != nil {
					return nil, readErr
				}
				amount, amountOK := stock.Int()
				matched = amountOK && amount > 0
			default:
				return nil, ErrProgramInvariant
			}
			if !matched {
				break
			}
		}
		if matched {
			elements = append(elements, selectionElement{ability: AbilityRef{Owner: owner, Handle: state.handle}})
		}
	}
	if selector.order.by == "stable_id" {
		sort.SliceStable(elements, func(left, right int) bool {
			if selector.order.direction == "desc" {
				return elements[left].ability.Handle > elements[right].ability.Handle
			}
			return elements[left].ability.Handle < elements[right].ability.Handle
		})
	} else if selector.order.direction == "desc" {
		sort.SliceStable(elements, func(left, right int) bool {
			leftState := runtime.abilities[abilityKey{owner: owner, handle: elements[left].ability.Handle}]
			rightState := runtime.abilities[abilityKey{owner: owner, handle: elements[right].ability.Handle}]
			if leftState.slot != rightState.slot {
				return leftState.slot > rightState.slot
			}
			return leftState.handle > rightState.handle
		})
	}
	return elements, nil
}

func (runtime *Runtime) buildSelectRequest(cast *castInstance, selector selectorProgram) (SelectRequest, error) {
	from, err := runtime.evalValue(cast, selector.from)
	if err != nil {
		return SelectRequest{}, err
	}
	origin := Position{}
	if entity, ok := from.Entity(); ok {
		position, readErr := runtime.readEntityPosition(cast, entity)
		if readErr != nil {
			return SelectRequest{}, readErr
		}
		origin, _ = position.Position()
	} else if position, ok := from.Position(); ok {
		origin = position
	}
	shape, err := runtime.buildSelectShape(cast, selector.shapePlan, from, origin)
	if err != nil {
		return SelectRequest{}, err
	}
	filters := make([]SelectFilter, 0, len(selector.filters))
	for _, filter := range selector.filters {
		switch filter.kind {
		case "alive":
			filters = append(filters, AliveSelectFilter{})
		case "not_caster":
			filters = append(filters, NotCasterSelectFilter{})
		case "relation":
			filters = append(filters, RelationSelectFilter{Relation: filter.relation})
		case "has_status":
			filters = append(filters, StatusSelectFilter{Status: filter.status, Has: true})
		case "missing_status":
			filters = append(filters, StatusSelectFilter{Status: filter.status})
		case "attribute_compare":
			value, evalErr := runtime.evalInt(cast, filter.value)
			if evalErr != nil {
				return SelectRequest{}, evalErr
			}
			filters = append(filters, AttributeSelectFilter{Attribute: filter.attribute, Operation: filter.operation, Value: value})
		case "visible":
			filters = append(filters, VisibleSelectFilter{})
		case "targetable":
			filters = append(filters, TargetableSelectFilter{})
		case "line_of_sight":
			filters = append(filters, LineOfSightSelectFilter{Layers: append([]CollisionLayerHandle(nil), filter.collision...)})
		case "has_gameplay_tag":
			filters = append(filters, GameplayTagSelectFilter{Tag: filter.tag, Has: true})
		case "missing_gameplay_tag":
			filters = append(filters, GameplayTagSelectFilter{Tag: filter.tag})
		case "source_skill":
			filters = append(filters, OwnedSourceSkillFilter{SkillID: filter.text})
		case "source_cast":
			filters = append(filters, OwnedSourceCastFilter{CastID: filter.cast})
		case "spawned_before":
			filters = append(filters, OwnedSpawnTickFilter{Operation: "lt", Tick: filter.tick})
		case "spawned_after":
			filters = append(filters, OwnedSpawnTickFilter{Operation: "gt", Tick: filter.tick})
		case "unit_template":
			filters = append(filters, OwnedUnitTemplateFilter{Template: filter.template})
		case "entity_tag":
			filters = append(filters, OwnedEntityTagFilter{Tag: filter.tag})
		case "status_id":
			filters = append(filters, StatusIDSelectFilter{Status: filter.status})
		case "status_category", "status_polarity":
			filters = append(filters, StatusTextSelectFilter{Kind: filter.kind, Value: filter.text})
		case "status_tag":
			filters = append(filters, StatusTextSelectFilter{Kind: filter.kind, Tag: filter.tag})
		case "status_dispellable", "status_transferable", "status_copyable":
			filters = append(filters, StatusFlagSelectFilter{Kind: filter.kind})
		case "status_source", "status_owner":
			value, evalErr := runtime.evalValue(cast, filter.value)
			if evalErr != nil {
				return SelectRequest{}, evalErr
			}
			entity, entityOK := value.Entity()
			if !entityOK {
				return SelectRequest{}, ErrRuntimeTypeMismatch
			}
			filters = append(filters, StatusEntitySelectFilter{Kind: filter.kind, Entity: entity})
		case "status_source_skill":
			filters = append(filters, StatusSourceSkillSelectFilter{SkillID: filter.text})
		case "status_stack_compare", "status_duration_compare":
			value, evalErr := runtime.evalInt(cast, filter.value)
			if evalErr != nil {
				return SelectRequest{}, evalErr
			}
			filters = append(filters, StatusCompareSelectFilter{Kind: filter.kind, Operation: filter.operation, Value: value})
		default:
			return SelectRequest{}, ErrProgramInvariant
		}
	}
	order := SelectOrder{By: SelectOrderBy(selector.order.by), Direction: SelectDirection(selector.order.direction)}
	limit := selector.limit
	if selector.hasRandomSite {
		order = SelectOrder{By: SelectOrderEntityID, Direction: SelectAscending}
		limit = 0
	}
	return SelectRequest{Meta: QueryMeta{RequiredRevision: cast.visibleRevision}, Caster: cast.caster, ElementKind: selectionElementName(selector.element), Shape: shape, Filters: filters, Order: order, Limit: limit}, nil
}

func (runtime *Runtime) buildSelectShape(cast *castInstance, shape shapeProgram, from RuntimeValue, origin Position) (SelectShape, error) {
	integer := func(index int) (int64, error) {
		if index >= len(shape.values) {
			return 0, ErrProgramInvariant
		}
		return runtime.evalInt(cast, shape.values[index])
	}
	direction := func(index int) (Direction, error) {
		if index >= len(shape.values) {
			return Direction{}, ErrProgramInvariant
		}
		value, err := runtime.evalValue(cast, shape.values[index])
		if err != nil {
			return Direction{}, err
		}
		result, ok := value.Direction()
		if !ok {
			return Direction{}, ErrRuntimeTypeMismatch
		}
		return result, nil
	}
	switch shape.kind {
	case "status_set":
		target, ok := from.Entity()
		if !ok {
			return nil, ErrRuntimeTypeMismatch
		}
		return StatusSetSelectShape{Target: target}, nil
	case "owned_entities":
		owner, ok := from.Entity()
		if !ok {
			return nil, ErrRuntimeTypeMismatch
		}
		return OwnedEntitiesSelectShape{Owner: owner}, nil
	case "single":
		entity, ok := from.Entity()
		if !ok {
			return nil, ErrRuntimeTypeMismatch
		}
		return SingleSelectShape{Entity: entity}, nil
	case "circle":
		radius, err := integer(0)
		return CircleSelectShape{Center: origin, Radius: radius}, err
	case "ring":
		inner, err := integer(0)
		if err != nil {
			return nil, err
		}
		outer, err := integer(1)
		return RingSelectShape{Center: origin, InnerRadius: inner, OuterRadius: outer}, err
	case "cone":
		rangeValue, err := integer(0)
		if err != nil {
			return nil, err
		}
		angle, err := integer(1)
		if err != nil {
			return nil, err
		}
		directionValue, err := direction(2)
		return ConeSelectShape{Origin: origin, Direction: directionValue, Range: rangeValue, AngleMDeg: angle}, err
	case "line", "rectangle":
		length, err := integer(0)
		if err != nil {
			return nil, err
		}
		width, err := integer(1)
		if err != nil {
			return nil, err
		}
		directionValue, err := direction(2)
		if shape.kind == "rectangle" {
			return RectangleSelectShape{Origin: origin, Direction: directionValue, Length: length, Width: width}, err
		}
		return LineSelectShape{Origin: origin, Direction: directionValue, Length: length, Width: width}, err
	case "raycast":
		length, err := integer(0)
		if err != nil {
			return nil, err
		}
		directionValue, err := direction(1)
		return RaycastSelectShape{Origin: origin, Direction: directionValue, Length: length}, err
	case "chain":
		hopRange, err := integer(0)
		return ChainSelectShape{Origin: origin, HopRange: hopRange, MaxTargets: shape.maxTargets}, err
	case "path":
		if len(shape.values) != 1 {
			return nil, ErrProgramInvariant
		}
		value, err := runtime.evalValue(cast, shape.values[0])
		if err != nil {
			return nil, err
		}
		points, ok := value.Path()
		if !ok {
			return nil, ErrRuntimeTypeMismatch
		}
		return PathSelectShape{Points: points}, nil
	case "nearest_valid":
		radius, err := integer(0)
		return NearestValidSelectShape{Origin: origin, SearchRadius: radius}, err
	default:
		return nil, ErrProgramInvariant
	}
}

func selectionRuntimeValue(element selectionElement, kind selectionElementType) (RuntimeValue, error) {
	switch kind {
	case selectionEntity:
		return EntityRuntimeValue(element.entity), nil
	case selectionPosition:
		return PositionRuntimeValue(element.position), nil
	case selectionHit:
		return HitRuntimeValue(element.hit), nil
	case selectionAbility:
		return AbilityRuntimeValue(element.ability), nil
	case selectionStatusInstance:
		return StatusInstanceRuntimeValue(element.status), nil
	default:
		return RuntimeValue{}, ErrProgramInvariant
	}
}

func stableSelectionID(element selectionElement, kind selectionElementType) uint64 {
	switch kind {
	case selectionEntity:
		return uint64(element.entity)
	case selectionHit:
		return element.hit.ColliderID
	case selectionPosition:
		var encoded [16]byte
		binary.BigEndian.PutUint64(encoded[:8], uint64(element.position.X))
		binary.BigEndian.PutUint64(encoded[8:], uint64(element.position.Y))
		sum := sha256.Sum256(encoded[:])
		return binary.BigEndian.Uint64(sum[:8])
	case selectionAbility:
		return uint64(element.ability.Handle)
	case selectionStatusInstance:
		return element.status.ID.OpaqueID()
	default:
		return 0
	}
}
