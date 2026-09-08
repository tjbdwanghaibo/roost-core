package skill

import (
	"fmt"
	"sync"
)

type MemoryEntity struct {
	ID                                                            EntityID
	Owner                                                         EntityID
	Alive                                                         bool
	Position                                                      Position
	Facing                                                        Direction
	Relation                                                      string
	TeamID                                                        uint64
	Resources                                                     map[string]int64
	Statuses                                                      map[StatusHandle]bool
	Attributes                                                    map[AttributeHandle]int64
	Health, MaxHealth                                             int64
	Shield                                                        int64
	Armor, MagicResistance, Penetration                           int64
	DamageDealtBP, DamageTakenBP, CriticalMultiplierBP            int64
	DamageCap, MinimumDamage, VampBP                              int64
	ForceCritical, DamageImmune, SpellShield, Dodge, Block, Parry bool
	Untargetable                                                  bool
	ElementMultipliers                                            map[ElementHandle]int64
	GameplayTags                                                  map[GameplayTagHandle]bool
	VisibleTo                                                     map[EntityID]bool
	BlockedLineOfSightLayers                                      map[CollisionLayerHandle]bool
	AbilityState                                                  map[string]RuntimeValue
	Cooldowns                                                     map[string]Tick
	TenacityBP                                                    int64
}

type memoryProcess struct {
	state  ProcessHostState
	active bool
}

type statusInstance struct {
	target      EntityID
	status      StatusHandle
	sourceOwner EntityID
	sourceSkill string
	sourceCast  CastID
	effect      EffectIndex
	sequence    uint64
	appliedTick Tick
	stacks      int
	dueTick     Tick
	shield      int64
}

type attributeModifierInstance struct {
	target      EntityID
	attribute   AttributeHandle
	sourceOwner EntityID
	sourceSkill string
	sourceCast  CastID
	effect      EffectIndex
	sequence    uint64
	operation   string
	value       int64
	dueTick     Tick
}

type memoryStateKey struct {
	handle  StateHandle
	binding StateScopeBinding
}

type memoryStateRecord struct {
	value    RuntimeValue
	dueTick  Tick
	sequence uint64
	clearOn  []string
	event    EventContext
}

type ownedSpawnTransaction struct {
	created  []EntityID
	replaced map[EntityID]OwnedEntityMetadata
	entities map[EntityID]MemoryEntity
}

type MemoryHost struct {
	mutex                 sync.RWMutex
	authority             AuthorityIdentity
	tick                  Tick
	revision              WorldRevision
	entities              map[EntityID]MemoryEntity
	processes             map[ProcessID]memoryProcess
	events                []RuntimeEvent
	nextCursor            EventCursor
	nextEntity            EntityID
	gameplay              GameplayCatalog
	criticalTag, spellTag GameplayTagHandle
	statuses              []statusInstance
	modifiers             []attributeModifierInstance
	nextInstanceSequence  uint64
	states                map[memoryStateKey]memoryStateRecord
	nextStateSequence     uint64
	ownedEntities         map[EntityID]OwnedEntityMetadata
	nextOwnedSequence     uint64
	ownedTransactions     map[OwnedSpawnTransactionID]ownedSpawnTransaction
	nextOwnedTransaction  OwnedSpawnTransactionID
	temporalSnapshots     map[uint64]temporalSnapshotRecord
	nextTemporalToken     uint64
	temporalBlocked       map[Position]Position
	compactEvents         bool
}

type MemoryHostOptions struct{ CompactEvents bool }

func NewMemoryHost(authority AuthorityIdentity) *MemoryHost {
	return NewMemoryHostWithOptions(authority, MemoryHostOptions{})
}

func NewMemoryHostWithOptions(authority AuthorityIdentity, options MemoryHostOptions) *MemoryHost {
	return &MemoryHost{authority: authority, entities: make(map[EntityID]MemoryEntity), processes: make(map[ProcessID]memoryProcess), states: make(map[memoryStateKey]memoryStateRecord), ownedEntities: make(map[EntityID]OwnedEntityMetadata), ownedTransactions: make(map[OwnedSpawnTransactionID]ownedSpawnTransaction), temporalSnapshots: make(map[uint64]temporalSnapshotRecord), temporalBlocked: make(map[Position]Position), nextEntity: 1, compactEvents: options.CompactEvents}
}

func (host *MemoryHost) AuthorityIdentity() AuthorityIdentity { return host.authority }

func (host *MemoryHost) AbilityOwnerRelation(viewer, owner EntityID) (string, bool) {
	if viewer == owner {
		return "self", true
	}
	host.mutex.RLock()
	defer host.mutex.RUnlock()
	entity, found := host.entities[owner]
	if !found || (entity.Relation != "ally" && entity.Relation != "enemy") {
		return "", false
	}
	return entity.Relation, true
}

func (host *MemoryHost) ConfigureGameplayCatalog(catalog GameplayCatalog) {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	host.gameplay = cloneHostGameplayCatalog(catalog)
	if handle, ok := lookupTag(catalog.Tags, "critical"); ok {
		host.criticalTag = handle
	}
	if handle, ok := lookupTag(catalog.Tags, "spell"); ok {
		host.spellTag = handle
	}
}

func cloneHostGameplayCatalog(catalog GameplayCatalog) GameplayCatalog {
	result := catalog
	result.Attributes.Entries = append([]AttributeCatalogEntry(nil), catalog.Attributes.Entries...)
	for index := range result.Attributes.Entries {
		result.Attributes.Entries[index].Snapshots = append([]string(nil), result.Attributes.Entries[index].Snapshots...)
		result.Attributes.Entries[index].ModifierOperations = append([]string(nil), result.Attributes.Entries[index].ModifierOperations...)
	}
	result.Statuses.Entries = append([]StatusCatalogEntry(nil), catalog.Statuses.Entries...)
	for index := range result.Statuses.Entries {
		entry := &result.Statuses.Entries[index]
		entry.ImmunityTags = append([]GameplayTagHandle(nil), entry.ImmunityTags...)
		entry.GameplayTags = append([]GameplayTagHandle(nil), entry.GameplayTags...)
		entry.AttributeModifiers = append([]StatusAttributeModifier(nil), entry.AttributeModifiers...)
		entry.CombatHooks = append([]string(nil), entry.CombatHooks...)
	}
	result.UnitTemplates.Entries = append([]UnitTemplateCatalogEntry(nil), catalog.UnitTemplates.Entries...)
	for index := range result.UnitTemplates.Entries {
		entry := &result.UnitTemplates.Entries[index]
		entry.Commands = append([]string(nil), entry.Commands...)
		entry.Behaviors = append([]string(nil), entry.Behaviors...)
		entry.GameplayTags = append([]GameplayTagHandle(nil), entry.GameplayTags...)
		entry.AllowedAttributeOverrides = append([]UnitTemplateAttributeOverridePolicy(nil), entry.AllowedAttributeOverrides...)
		entry.Parameters = append([]UnitTemplateParameterPolicy(nil), entry.Parameters...)
	}
	result.Tags.Entries = append([]GameplayTagCatalogEntry(nil), catalog.Tags.Entries...)
	result.DamageTypes.Entries = append([]DamageTypeCatalogEntry(nil), catalog.DamageTypes.Entries...)
	result.Elements.Entries = append([]ElementCatalogEntry(nil), catalog.Elements.Entries...)
	result.Combat.DamageTypes = append([]DamageTypeHandle(nil), catalog.Combat.DamageTypes...)
	result.Temporal.Entries = append([]TemporalSnapshotProfile(nil), catalog.Temporal.Entries...)
	for index := range result.Temporal.Entries {
		result.Temporal.Entries[index].Fields = append([]string(nil), result.Temporal.Entries[index].Fields...)
	}
	return result
}

func (host *MemoryHost) CurrentRevision() WorldRevision {
	host.mutex.RLock()
	defer host.mutex.RUnlock()
	return host.revision
}

func (host *MemoryHost) Advance(tick Tick) (WorldRevision, error) {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	if tick < host.tick {
		return host.revision, fmt.Errorf("skill: tick moved backwards")
	}
	if tick == host.tick {
		return host.revision, nil
	}
	host.tick = tick
	host.revision++
	host.appendEventLocked("tick_advanced", 0, 0)
	host.expireDueLocked()
	host.expireStatesLocked()
	host.expireOwnedLocked()
	host.expireTemporalSnapshotsLocked()
	return host.revision, nil
}

func (host *MemoryHost) UpsertEntity(entity MemoryEntity) {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	entity.Resources = cloneStringIntMap(entity.Resources)
	entity.Statuses = cloneStatusMap(entity.Statuses)
	entity.Attributes = cloneAttributeMap(entity.Attributes)
	entity.ElementMultipliers = cloneElementMultiplierMap(entity.ElementMultipliers)
	entity.GameplayTags = cloneGameplayTagSet(entity.GameplayTags)
	entity.VisibleTo = cloneEntityBoolSet(entity.VisibleTo)
	entity.BlockedLineOfSightLayers = cloneCollisionLayerBoolSet(entity.BlockedLineOfSightLayers)
	entity.AbilityState = cloneRuntimeValueMap(entity.AbilityState)
	entity.Cooldowns = cloneStringTickMap(entity.Cooldowns)
	host.entities[entity.ID] = entity
	if entity.ID >= host.nextEntity {
		host.nextEntity = entity.ID + 1
	}
	host.revision++
}

func (host *MemoryHost) HealthForTest(entity EntityID) int64 {
	host.mutex.RLock()
	defer host.mutex.RUnlock()
	return host.entities[entity].Health
}

func (host *MemoryHost) ResourceForTest(entity EntityID, resource string) int64 {
	host.mutex.RLock()
	defer host.mutex.RUnlock()
	return host.entities[entity].Resources[resource]
}

func (host *MemoryHost) AttributeForTest(entity EntityID, attribute AttributeHandle) int64 {
	host.mutex.RLock()
	defer host.mutex.RUnlock()
	return host.entities[entity].Attributes[attribute]
}

func (host *MemoryHost) Read(request ReadRequest) (ReadResult, error) {
	host.mutex.RLock()
	defer host.mutex.RUnlock()
	if err := host.requireRevisionLocked(request.Meta.RequiredRevision); err != nil {
		return ReadResult{}, err
	}
	result := ReadResult{Meta: QueryResultMeta{Revision: host.revision}}
	switch payload := request.Payload.(type) {
	case ResourceRead:
		entity, ok := host.entities[payload.Entity]
		if !ok {
			return ReadResult{}, ErrEntityNotFound
		}
		result.Value = IntRuntimeValue(entity.Resources[payload.Resource], quantityResourceAmount)
	case PositionRead:
		entity, ok := host.entities[payload.Entity]
		if !ok {
			return ReadResult{}, ErrEntityNotFound
		}
		result.Value = PositionRuntimeValue(entity.Position)
	case AttributeRead:
		if _, ok := host.entities[payload.Entity]; !ok {
			return ReadResult{}, ErrEntityNotFound
		}
		quantity := quantityDimensionless
		for _, entry := range host.gameplay.Attributes.Entries {
			if entry.Handle == payload.Attribute {
				quantity = entry.Quantity
				break
			}
		}
		result.Value = IntRuntimeValue(host.effectiveAttributeLocked(payload.Entity, payload.Attribute), quantity)
	default:
		return ReadResult{}, fmt.Errorf("skill: unsupported read payload %T", request.Payload)
	}
	return result, nil
}

func (host *MemoryHost) PayCosts(payment CostPayment) (CommitReceipt, error) {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	if err := host.requireRevisionLocked(payment.Meta.RequiredRevision); err != nil {
		return CommitReceipt{}, err
	}
	entity, ok := host.entities[payment.Entity]
	if !ok {
		return CommitReceipt{}, ErrEntityNotFound
	}
	totals := make(map[string]int64, len(payment.Entries))
	for _, entry := range payment.Entries {
		if entry.Amount < 0 {
			return CommitReceipt{}, fmt.Errorf("skill: negative cost")
		}
		resource := entry.Resource
		if resource == "" {
			resource = host.resourceKeyLocked(entry.Handle)
		}
		if resource == "" {
			return CommitReceipt{}, ErrCombatHandleInvalid
		}
		totals[resource] = saturatingInt64Add(totals[resource], entry.Amount)
	}
	for resource, amount := range totals {
		if entity.Resources[resource] < amount {
			return CommitReceipt{}, ErrInsufficientResource
		}
	}
	changed := false
	for resource, amount := range totals {
		if amount > 0 {
			entity.Resources[resource] -= amount
			changed = true
		}
	}
	if !changed {
		return CommitReceipt{Revision: host.revision}, nil
	}
	host.entities[payment.Entity] = entity
	receipt := host.commitLocked("costs_paid", payment.Entity, 0)
	return receipt, nil
}

func (host *MemoryHost) resourceKeyLocked(handle ResourceHandle) string {
	for _, entry := range host.gameplay.Resources.Entries {
		if entry.Handle == handle {
			return entry.Key
		}
	}
	return ""
}

func (host *MemoryHost) Events(after EventCursor) []RuntimeEvent {
	host.mutex.RLock()
	defer host.mutex.RUnlock()
	first := len(host.events)
	for index, event := range host.events {
		if event.Cursor > after {
			first = index
			break
		}
	}
	return cloneRuntimeEvents(host.events[first:])
}

func (host *MemoryHost) CompactEventsThrough(cursor EventCursor) {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	if !host.compactEvents {
		return
	}
	first := 0
	for first < len(host.events) && host.events[first].Cursor <= cursor {
		first++
	}
	if first == 0 {
		return
	}
	if first == len(host.events) {
		host.events = nil
		return
	}
	copy(host.events, host.events[first:])
	for index := len(host.events) - first; index < len(host.events); index++ {
		host.events[index] = RuntimeEvent{}
	}
	host.events = host.events[:len(host.events)-first]
}

func (host *MemoryHost) requireRevisionLocked(required WorldRevision) error {
	if required > host.revision {
		return ErrRevisionUnavailable
	}
	return nil
}

func (host *MemoryHost) commitLocked(kind string, entity EntityID, process ProcessID) CommitReceipt {
	host.revision++
	host.appendEventLocked(kind, entity, process)
	return CommitReceipt{Revision: host.revision, Changed: true}
}

func (host *MemoryHost) appendEventLocked(kind string, entity EntityID, process ProcessID) {
	host.appendContextEventLocked(kind, entity, process, EventContext{})
}

func (host *MemoryHost) appendContextEventLocked(kind string, entity EntityID, process ProcessID, context EventContext) {
	host.nextCursor++
	context.Tick = host.tick
	context.WorldRevision = host.revision
	host.events = append(host.events, RuntimeEvent{Cursor: host.nextCursor, Revision: host.revision, Tick: host.tick, Kind: kind, Entity: entity, ProcessID: process, Context: context})
}

func cloneStringIntMap(values map[string]int64) map[string]int64 {
	result := make(map[string]int64, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneStatusMap(values map[StatusHandle]bool) map[StatusHandle]bool {
	result := make(map[StatusHandle]bool, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneStringTickMap(values map[string]Tick) map[string]Tick {
	result := make(map[string]Tick, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneAttributeMap(values map[AttributeHandle]int64) map[AttributeHandle]int64 {
	result := make(map[AttributeHandle]int64, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneElementMultiplierMap(values map[ElementHandle]int64) map[ElementHandle]int64 {
	result := make(map[ElementHandle]int64, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneGameplayTagSet(values map[GameplayTagHandle]bool) map[GameplayTagHandle]bool {
	result := make(map[GameplayTagHandle]bool, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneEntityBoolSet(values map[EntityID]bool) map[EntityID]bool {
	result := make(map[EntityID]bool, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneCollisionLayerBoolSet(values map[CollisionLayerHandle]bool) map[CollisionLayerHandle]bool {
	result := make(map[CollisionLayerHandle]bool, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func saturatingInt64Add(left, right int64) int64 {
	maximum := int64(^uint64(0) >> 1)
	minimum := -maximum - 1
	if right > 0 && left > maximum-right {
		return maximum
	}
	if right < 0 && left < minimum-right {
		return minimum
	}
	return left + right
}

func saturatingInt64Mul(left, right int64) int64 {
	product, ok := checkedInt64Mul(left, right)
	if ok {
		return product
	}
	if (left < 0) != (right < 0) {
		return -int64(^uint64(0)>>1) - 1
	}
	return int64(^uint64(0) >> 1)
}

func saturatingInt64Sub(left, right int64) int64 {
	maximum := int64(^uint64(0) >> 1)
	minimum := -maximum - 1
	if right > 0 && left < minimum+right {
		return minimum
	}
	if right < 0 && left > maximum+right {
		return maximum
	}
	return left - right
}
