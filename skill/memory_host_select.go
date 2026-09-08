package skill

import "sort"

type selectCandidate struct {
	entity   MemoryEntity
	distance int64
	hit      Hit
	owned    OwnedEntityMetadata
}

func (host *MemoryHost) Select(request SelectRequest) (SelectResult, error) {
	host.mutex.RLock()
	defer host.mutex.RUnlock()
	if err := host.requireRevisionLocked(request.Meta.RequiredRevision); err != nil {
		return SelectResult{}, err
	}
	if shape, ok := request.Shape.(StatusSetSelectShape); ok {
		return host.selectStatusInstancesLocked(request, shape)
	}
	candidates := make([]selectCandidate, 0, len(host.entities))
	ownedShape, selectingOwned := request.Shape.(OwnedEntitiesSelectShape)
	ownerPosition := Position{}
	if selectingOwned {
		if owner, found := host.entities[ownedShape.Owner]; found {
			ownerPosition = owner.Position
		}
	}
	for _, entity := range host.entities {
		owned := OwnedEntityMetadata{}
		if selectingOwned {
			var found bool
			owned, found = host.ownedEntities[entity.ID]
			if !found || owned.Owner != ownedShape.Owner {
				continue
			}
		}
		included, distance, hit := entityMatchesShape(entity, request.Shape)
		if !included || !host.entityMatchesFiltersLocked(entity, request) {
			continue
		}
		if selectingOwned {
			distance = distanceSquared(entity.Position, ownerPosition)
		}
		candidates = append(candidates, selectCandidate{entity: entity, distance: distance, hit: hit, owned: owned})
	}
	order := request.Order.By
	direction := request.Order.Direction
	switch request.Shape.(type) {
	case RaycastSelectShape, ChainSelectShape, NearestValidSelectShape:
		order = SelectOrderDistance
		direction = SelectAscending
	}
	if order == "" || order == SelectOrderRandom {
		order = SelectOrderEntityID
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		var less bool
		var equal bool
		switch order {
		case SelectOrderDistance:
			less, equal = left.distance < right.distance, left.distance == right.distance
		case SelectOrderSpawnTick:
			less, equal = left.owned.SpawnTick < right.owned.SpawnTick, left.owned.SpawnTick == right.owned.SpawnTick
		case SelectOrderSpawnSequence:
			less, equal = left.owned.SpawnSequence < right.owned.SpawnSequence, left.owned.SpawnSequence == right.owned.SpawnSequence
		case SelectOrderDistanceToOwner:
			less, equal = left.distance < right.distance, left.distance == right.distance
		case SelectOrderRemainingLifetime:
			leftRemaining := max(Tick(0), left.owned.DueTick-host.tick)
			rightRemaining := max(Tick(0), right.owned.DueTick-host.tick)
			less, equal = leftRemaining < rightRemaining, leftRemaining == rightRemaining
		default:
			less, equal = left.entity.ID < right.entity.ID, left.entity.ID == right.entity.ID
		}
		if equal {
			if selectingOwned && left.owned.SpawnSequence != right.owned.SpawnSequence {
				return left.owned.SpawnSequence < right.owned.SpawnSequence
			}
			return left.entity.ID < right.entity.ID
		}
		if direction == SelectDescending {
			return !less
		}
		return less
	})
	limit := request.Limit
	if chain, ok := request.Shape.(ChainSelectShape); ok && chain.MaxTargets > 0 && (limit <= 0 || chain.MaxTargets < limit) {
		limit = chain.MaxTargets
	}
	if limit > 0 && len(candidates) > limit {
		candidates = candidates[:limit]
	}
	elementType := selectionEntity
	switch request.ElementKind {
	case "position":
		elementType = selectionPosition
	case "hit":
		elementType = selectionHit
	case "path":
		elementType = selectionPath
	}
	if _, raycast := request.Shape.(RaycastSelectShape); raycast {
		elementType = selectionHit
	}
	meta := QueryResultMeta{Revision: host.revision}
	selection := Selection{elementType: elementType, meta: meta, query: SelectionQueryMeta{Revision: host.revision, Order: order, Direction: direction, Limit: limit}, elements: make([]selectionElement, len(candidates))}
	for index, candidate := range candidates {
		selection.elements[index] = selectionElement{entity: candidate.entity.ID, position: candidate.entity.Position, hit: candidate.hit}
	}
	return SelectResult{Meta: meta, Selection: selection}, nil
}

func (host *MemoryHost) selectStatusInstancesLocked(request SelectRequest, shape StatusSetSelectShape) (SelectResult, error) {
	type candidate struct {
		instance statusInstance
		policy   StatusCatalogEntry
	}
	candidates := make([]candidate, 0)
	for _, instance := range host.statuses {
		if instance.target != shape.Target {
			continue
		}
		policy, ok := host.statusPolicy(instance.status)
		if !ok {
			continue
		}
		matched := true
		for _, filter := range request.Filters {
			switch typed := filter.(type) {
			case StatusIDSelectFilter:
				matched = instance.status == typed.Status
			case StatusTextSelectFilter:
				switch typed.Kind {
				case "status_category":
					matched = policy.Category == typed.Value
				case "status_polarity":
					matched = policy.Polarity == typed.Value
				case "status_tag":
					matched = containsGameplayTag(policy.GameplayTags, typed.Tag)
				default:
					matched = false
				}
			case StatusFlagSelectFilter:
				switch typed.Kind {
				case "status_dispellable":
					matched = policy.Dispellable
				case "status_transferable":
					matched = policy.Transferable
				case "status_copyable":
					matched = policy.Copyable
				default:
					matched = false
				}
			case StatusEntitySelectFilter:
				if typed.Kind == "status_source" {
					matched = instance.sourceOwner == typed.Entity
				} else {
					matched = instance.target == typed.Entity
				}
			case StatusSourceSkillSelectFilter:
				matched = instance.sourceSkill == typed.SkillID
			case StatusCompareSelectFilter:
				actual := int64(instance.stacks)
				if typed.Kind == "status_duration_compare" {
					actual = int64(max(Tick(0), instance.dueTick-host.tick))
				}
				matched = compareInt64(actual, typed.Operation, typed.Value)
			default:
				matched = false
			}
			if !matched {
				break
			}
		}
		if matched {
			candidates = append(candidates, candidate{instance: instance, policy: policy})
		}
	}
	order, direction := request.Order.By, request.Order.Direction
	if order == "" || order == SelectOrderRandom {
		order = SelectOrderStatusInstanceID
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		var lv, rv int64
		switch order {
		case SelectOrderStatusDispelPriority:
			lv, rv = int64(left.policy.DispelPriority), int64(right.policy.DispelPriority)
		case SelectOrderRemainingDuration:
			lv, rv = int64(max(Tick(0), left.instance.dueTick-host.tick)), int64(max(Tick(0), right.instance.dueTick-host.tick))
		case SelectOrderStackCount:
			lv, rv = int64(left.instance.stacks), int64(right.instance.stacks)
		case SelectOrderAppliedTick:
			lv, rv = int64(left.instance.appliedTick), int64(right.instance.appliedTick)
		default:
			lv, rv = int64(left.instance.sequence), int64(right.instance.sequence)
		}
		if lv != rv {
			if direction == SelectDescending {
				return lv > rv
			}
			return lv < rv
		}
		return left.instance.sequence < right.instance.sequence
	})
	limit := request.Limit
	if limit > 0 && len(candidates) > limit {
		candidates = candidates[:limit]
	}
	meta := QueryResultMeta{Revision: host.revision}
	selection := Selection{elementType: selectionStatusInstance, meta: meta, query: SelectionQueryMeta{Revision: host.revision, Order: order, Direction: direction, Limit: limit}, elements: make([]selectionElement, len(candidates))}
	for index, candidate := range candidates {
		selection.elements[index].status = StatusInstanceRef{ID: StatusInstanceID{opaque: candidate.instance.sequence}, Target: candidate.instance.target}
	}
	return SelectResult{Meta: meta, Selection: selection}, nil
}

func compareInt64(left int64, operation string, right int64) bool {
	switch operation {
	case "eq":
		return left == right
	case "ne":
		return left != right
	case "lt":
		return left < right
	case "lte":
		return left <= right
	case "gt":
		return left > right
	case "gte":
		return left >= right
	default:
		return false
	}
}

func entityMatchesShape(entity MemoryEntity, shape SelectShape) (bool, int64, Hit) {
	if shape == nil {
		return true, 0, Hit{Entity: entity.ID, Position: entity.Position, ColliderID: uint64(entity.ID)}
	}
	var origin Position
	var included bool
	switch typed := shape.(type) {
	case SingleSelectShape:
		included = entity.ID == typed.Entity
		origin = entity.Position
	case CircleSelectShape:
		origin = typed.Center
		included = withinRadius(entity.Position, origin, typed.Radius)
	case RingSelectShape:
		origin = typed.Center
		distance := distanceSquared(entity.Position, origin)
		included = distance >= squareSaturating(typed.InnerRadius) && distance <= squareSaturating(typed.OuterRadius)
	case ConeSelectShape:
		origin = typed.Origin
		included = withinRadius(entity.Position, origin, typed.Range) && inForwardHalfPlane(entity.Position, origin, typed.Direction)
	case LineSelectShape:
		origin = typed.Origin
		included = withinLineBounds(entity.Position, typed.Origin, typed.Direction, typed.Length, typed.Width)
	case RectangleSelectShape:
		origin = typed.Origin
		included = withinLineBounds(entity.Position, typed.Origin, typed.Direction, typed.Length, typed.Width)
	case RaycastSelectShape:
		origin = typed.Origin
		included = withinLineBounds(entity.Position, typed.Origin, typed.Direction, typed.Length, 0)
	case ChainSelectShape:
		origin = typed.Origin
		included = withinRadius(entity.Position, origin, typed.HopRange)
	case PathSelectShape:
		for _, point := range typed.Points {
			if withinRadius(entity.Position, point, typed.Width) {
				origin, included = point, true
				break
			}
		}
	case NearestValidSelectShape:
		origin = typed.Origin
		included = withinRadius(entity.Position, origin, typed.SearchRadius)
	case OwnedEntitiesSelectShape:
		included = true
		origin = entity.Position
	default:
		return false, 0, Hit{}
	}
	distance := distanceSquared(entity.Position, origin)
	return included, distance, Hit{Entity: entity.ID, Position: entity.Position, Distance: distance, ColliderID: uint64(entity.ID)}
}

func (host *MemoryHost) entityMatchesFiltersLocked(entity MemoryEntity, request SelectRequest) bool {
	for _, filter := range request.Filters {
		switch typed := filter.(type) {
		case AliveSelectFilter:
			if !entity.Alive {
				return false
			}
		case NotCasterSelectFilter:
			if entity.ID == request.Caster {
				return false
			}
		case RelationSelectFilter:
			if entity.Relation != typed.Relation {
				return false
			}
		case StatusSelectFilter:
			if entity.Statuses[typed.Status] != typed.Has {
				return false
			}
		case AttributeSelectFilter:
			if !compareInt(host.effectiveAttributeLocked(entity.ID, typed.Attribute), typed.Operation, typed.Value) {
				return false
			}
		case VisibleSelectFilter:
			if !entity.VisibleTo[request.Caster] {
				return false
			}
		case TargetableSelectFilter:
			if entity.Untargetable {
				return false
			}
		case LineOfSightSelectFilter:
			for _, layer := range typed.Layers {
				if entity.BlockedLineOfSightLayers[layer] {
					return false
				}
			}
		case GameplayTagSelectFilter:
			if entity.GameplayTags[typed.Tag] != typed.Has {
				return false
			}
		case OwnedSourceSkillFilter:
			record, found := host.ownedEntities[entity.ID]
			if !found || record.SourceSkillID != typed.SkillID {
				return false
			}
		case OwnedSourceCastFilter:
			record, found := host.ownedEntities[entity.ID]
			if !found || record.SourceCastID != typed.CastID {
				return false
			}
		case OwnedUnitTemplateFilter:
			record, found := host.ownedEntities[entity.ID]
			if !found || record.Template != typed.Template {
				return false
			}
		case OwnedEntityTagFilter:
			record, found := host.ownedEntities[entity.ID]
			if !found || !containsGameplayTag(record.GameplayTags, typed.Tag) {
				return false
			}
		case OwnedSpawnTickFilter:
			record, found := host.ownedEntities[entity.ID]
			if !found || !compareInt(int64(record.SpawnTick), typed.Operation, int64(typed.Tick)) {
				return false
			}
		}
	}
	return true
}

func compareInt(left int64, operation string, right int64) bool {
	switch operation {
	case "eq":
		return left == right
	case "ne":
		return left != right
	case "lt":
		return left < right
	case "lte":
		return left <= right
	case "gt":
		return left > right
	case "gte":
		return left >= right
	default:
		return false
	}
}

func withinRadius(position, origin Position, radius int64) bool {
	return radius >= 0 && distanceSquared(position, origin) <= squareSaturating(radius)
}

func distanceSquared(left, right Position) int64 {
	dx, dy := absoluteDifference(left.X, right.X), absoluteDifference(left.Y, right.Y)
	return saturatingInt64Add(squareSaturating(dx), squareSaturating(dy))
}

func absoluteDifference(left, right int64) int64 {
	if left >= right {
		if right < 0 && left > int64(^uint64(0)>>1)+right {
			return int64(^uint64(0) >> 1)
		}
		return left - right
	}
	return absoluteDifference(right, left)
}

func squareSaturating(value int64) int64 {
	if value < 0 {
		if value == -int64(^uint64(0)>>1)-1 {
			return int64(^uint64(0) >> 1)
		}
		value = -value
	}
	maximum := int64(^uint64(0) >> 1)
	if value != 0 && value > maximum/value {
		return maximum
	}
	return value * value
}

func inForwardHalfPlane(position, origin Position, direction Direction) bool {
	dx, dy := saturatingInt64Sub(position.X, origin.X), saturatingInt64Sub(position.Y, origin.Y)
	return saturatingInt64Add(saturatingInt64Mul(dx, direction.X), saturatingInt64Mul(dy, direction.Y)) >= 0
}

func withinLineBounds(position, origin Position, direction Direction, length, width int64) bool {
	if length < 0 || width < 0 {
		return false
	}
	dx, dy := saturatingInt64Sub(position.X, origin.X), saturatingInt64Sub(position.Y, origin.Y)
	directionNorm := saturatingInt64Add(squareSaturating(direction.X), squareSaturating(direction.Y))
	if directionNorm == 0 {
		return withinRadius(position, origin, width)
	}
	dot := saturatingInt64Add(saturatingInt64Mul(dx, direction.X), saturatingInt64Mul(dy, direction.Y))
	if dot < 0 || squareSaturating(dot) > saturatingInt64Mul(squareSaturating(length), directionNorm) {
		return false
	}
	cross := saturatingInt64Sub(saturatingInt64Mul(dx, direction.Y), saturatingInt64Mul(dy, direction.X))
	return squareSaturating(cross) <= saturatingInt64Mul(squareSaturating(width), directionNorm)
}
