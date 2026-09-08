package combat

import (
	"errors"
	"fmt"
)

// BuffID identifies a buff definition in the host's catalog.
type BuffID uint32

// BuffInstanceID identifies one application inside a container. Instance ids
// are a per-container sequence, so they double as AttributeSet modifier
// handles and as deterministic ordering keys.
type BuffInstanceID uint64

// Tag classifies buffs for dispel and immunity matching.
type Tag string

// BuffStackPolicy controls what re-applying an already-present buff does.
type BuffStackPolicy uint8

const (
	// BuffRefresh adds a stack (up to MaxStacks) and resets the duration.
	BuffRefresh BuffStackPolicy = iota
	// BuffExtend adds a stack and extends the remaining duration by the
	// spec's (tenacity-adjusted) duration.
	BuffExtend
	// BuffIgnore keeps the existing instance untouched.
	BuffIgnore
	// BuffIndependent always creates a fresh instance with its own duration,
	// never merging with existing instances of the same ID (independently
	// timed DoTs, one-shot attribute modifiers).
	BuffIndependent
)

// BuffSpec is a buff definition. Specs are value data owned by the caller;
// the container copies what it stores.
type BuffSpec struct {
	ID   BuffID
	Tags []Tag
	// GrantsImmunityTo blocks later applications of any buff carrying one of
	// these tags while this buff is active.
	GrantsImmunityTo []Tag
	MaxStacks        int64 // <=1 means no stacking
	StackPolicy      BuffStackPolicy
	DurationTicks    int64 // <=0 means permanent until dispelled
	// TenacityAffected durations are reduced by the container's tenacity:
	// duration * (10000 - tenacityBP) / 10000, floored at 1 tick.
	TenacityAffected bool
	// MaxDurationTicks, when positive, caps the effective duration after
	// tenacity scaling.
	MaxDurationTicks int64
	// Modifiers are granted to the linked AttributeSet per stack.
	Modifiers []Modifier
}

// BuffApplyOutcome reports what Apply did.
type BuffApplyOutcome uint8

const (
	BuffApplied BuffApplyOutcome = iota
	BuffStacked
	BuffRefreshed
	BuffIgnored
	BuffBlockedImmune
)

// BuffInstance is one active buff.
type BuffInstance struct {
	Instance    BuffInstanceID
	Spec        BuffSpec
	Stacks      int64
	Source      int64
	AppliedTick int64
	DueTick     int64 // 0 means permanent
}

// BuffContainer holds a combatant's active buffs with stacking, dispel-tag,
// immunity, and tenacity semantics. Instances keep application order (by
// instance id), so every walk is deterministic. Linking an AttributeSet
// makes buff modifiers materialize as attribute grants automatically, scaled
// by stack count.
type BuffContainer struct {
	sequence   uint64
	active     []BuffInstance
	tenacityBP int64
	attributes *AttributeSet
}

func NewBuffContainer() *BuffContainer { return &BuffContainer{} }

// LinkAttributes connects the container to an AttributeSet; active buffs are
// re-granted immediately.
func (container *BuffContainer) LinkAttributes(attributes *AttributeSet) {
	container.attributes = attributes
	for index := range container.active {
		container.grantModifiers(&container.active[index])
	}
}

// SetTenacityBP sets crowd-control resistance in basis points (2000 = -20%
// duration on tenacity-affected buffs). Values are clamped into [0, 10000].
func (container *BuffContainer) SetTenacityBP(tenacityBP int64) {
	if tenacityBP < 0 {
		tenacityBP = 0
	}
	if tenacityBP > BasisPointScale {
		tenacityBP = BasisPointScale
	}
	container.tenacityBP = tenacityBP
}

func (container *BuffContainer) effectiveDuration(spec BuffSpec) int64 {
	if spec.DurationTicks <= 0 {
		return 0
	}
	duration := spec.DurationTicks
	if spec.TenacityAffected && container.tenacityBP > 0 {
		duration = ScaleBasisPoints(duration, BasisPointScale-container.tenacityBP)
		if duration < 1 {
			duration = 1
		}
	}
	if spec.MaxDurationTicks > 0 && duration > spec.MaxDurationTicks {
		duration = spec.MaxDurationTicks
	}
	return duration
}

// ImmuneTo reports whether an active buff grants immunity against the tag.
func (container *BuffContainer) ImmuneTo(tags []Tag) bool {
	for _, instance := range container.active {
		for _, immunity := range instance.Spec.GrantsImmunityTo {
			for _, tag := range tags {
				if tag == immunity {
					return true
				}
			}
		}
	}
	return false
}

// Apply adds (or re-applies) a buff at the given tick.
func (container *BuffContainer) Apply(spec BuffSpec, tick, source int64) (BuffInstanceID, BuffApplyOutcome) {
	if container.ImmuneTo(spec.Tags) {
		return 0, BuffBlockedImmune
	}
	duration := container.effectiveDuration(spec)
	for index := 0; spec.StackPolicy != BuffIndependent && index < len(container.active); index++ {
		instance := &container.active[index]
		if instance.Spec.ID != spec.ID {
			continue
		}
		switch spec.StackPolicy {
		case BuffIgnore:
			return instance.Instance, BuffIgnored
		case BuffExtend:
			if duration > 0 {
				if instance.DueTick == 0 {
					instance.DueTick = saturatingInt64Add(tick, duration)
				} else {
					instance.DueTick = saturatingInt64Add(instance.DueTick, duration)
				}
			}
		default: // BuffRefresh
			if duration > 0 {
				instance.DueTick = saturatingInt64Add(tick, duration)
			}
		}
		outcome := BuffRefreshed
		maxStacks := maxInt64(spec.MaxStacks, 1)
		if instance.Stacks < maxStacks {
			instance.Stacks++
			outcome = BuffStacked
		}
		instance.Spec = copyBuffSpec(spec)
		instance.Source = source
		container.grantModifiers(instance)
		return instance.Instance, outcome
	}
	container.sequence++
	instance := BuffInstance{
		Instance: BuffInstanceID(container.sequence), Spec: copyBuffSpec(spec),
		Stacks: 1, Source: source, AppliedTick: tick,
	}
	if duration > 0 {
		instance.DueTick = saturatingInt64Add(tick, duration)
	}
	container.active = append(container.active, instance)
	container.grantModifiers(&container.active[len(container.active)-1])
	return instance.Instance, BuffApplied
}

// Dispel removes up to limit active buffs carrying the tag, newest first
// (limit <= 0 means all). It returns the removed instances.
func (container *BuffContainer) Dispel(tag Tag, limit int) []BuffInstance {
	var removed []BuffInstance
	for index := len(container.active) - 1; index >= 0; index-- {
		if limit > 0 && len(removed) >= limit {
			break
		}
		if !containsTag(container.active[index].Spec.Tags, tag) {
			continue
		}
		removed = append(removed, container.removeAt(index))
	}
	return removed
}

// Remove drops one instance by id.
func (container *BuffContainer) Remove(id BuffInstanceID) (BuffInstance, bool) {
	for index := range container.active {
		if container.active[index].Instance == id {
			return container.removeAt(index), true
		}
	}
	return BuffInstance{}, false
}

// SetStacks pins an instance's stack count (clamped to [0, MaxStacks]) and
// re-materializes its attribute grants at the new scale. A count of zero
// removes the instance. Returns the post-change instance.
func (container *BuffContainer) SetStacks(id BuffInstanceID, stacks int64) (BuffInstance, bool) {
	for index := range container.active {
		instance := &container.active[index]
		if instance.Instance != id {
			continue
		}
		maximum := maxInt64(instance.Spec.MaxStacks, 1)
		if stacks > maximum {
			stacks = maximum
		}
		if stacks <= 0 {
			return container.removeAt(index), true
		}
		instance.Stacks = stacks
		container.grantModifiers(instance)
		return *instance, true
	}
	return BuffInstance{}, false
}

// SetDueTick pins an instance's expiry (0 means permanent).
func (container *BuffContainer) SetDueTick(id BuffInstanceID, dueTick int64) (BuffInstance, bool) {
	for index := range container.active {
		instance := &container.active[index]
		if instance.Instance != id {
			continue
		}
		instance.DueTick = dueTick
		return *instance, true
	}
	return BuffInstance{}, false
}

// Adopt injects a foreign instance (a copy or transfer from another
// container) under a fresh local instance id, bypassing immunity and
// stacking rules: the caller has already decided the move is legal. The
// adopted instance keeps its spec, stacks, source, and expiry.
func (container *BuffContainer) Adopt(instance BuffInstance) BuffInstanceID {
	container.sequence++
	instance.Instance = BuffInstanceID(container.sequence)
	instance.Spec = copyBuffSpec(instance.Spec)
	container.active = append(container.active, instance)
	container.grantModifiers(&container.active[len(container.active)-1])
	return instance.Instance
}

// Tick expires every instance due at or before now and returns them in
// application order.
func (container *BuffContainer) Tick(now int64) []BuffInstance {
	var expired []BuffInstance
	for index := 0; index < len(container.active); {
		instance := container.active[index]
		if instance.DueTick != 0 && instance.DueTick <= now {
			expired = append(expired, container.removeAt(index))
			continue
		}
		index++
	}
	return expired
}

// Active returns the live instances in application order. The slice is a
// copy; instances inside are the container's values.
func (container *BuffContainer) Active() []BuffInstance {
	return append([]BuffInstance(nil), container.active...)
}

// HasTag reports whether any active buff carries the tag.
func (container *BuffContainer) HasTag(tag Tag) bool {
	for _, instance := range container.active {
		if containsTag(instance.Spec.Tags, tag) {
			return true
		}
	}
	return false
}

func (container *BuffContainer) removeAt(index int) BuffInstance {
	instance := container.active[index]
	container.active = append(container.active[:index], container.active[index+1:]...)
	if container.attributes != nil {
		container.attributes.Revoke(ModifierHandle(instance.Instance))
	}
	return instance
}

func (container *BuffContainer) grantModifiers(instance *BuffInstance) {
	if container.attributes == nil || len(instance.Spec.Modifiers) == 0 {
		return
	}
	scaled := make([]Modifier, len(instance.Spec.Modifiers))
	for index, modifier := range instance.Spec.Modifiers {
		scaled[index] = Modifier{
			Attribute: modifier.Attribute,
			Flat:      saturatingInt64Mul(modifier.Flat, instance.Stacks),
			RateBP:    saturatingInt64Mul(modifier.RateBP, instance.Stacks),
		}
	}
	container.attributes.Grant(ModifierHandle(instance.Instance), scaled...)
}

func copyBuffSpec(spec BuffSpec) BuffSpec {
	spec.Tags = append([]Tag(nil), spec.Tags...)
	spec.GrantsImmunityTo = append([]Tag(nil), spec.GrantsImmunityTo...)
	spec.Modifiers = append([]Modifier(nil), spec.Modifiers...)
	return spec
}

func containsTag(tags []Tag, want Tag) bool {
	for _, tag := range tags {
		if tag == want {
			return true
		}
	}
	return false
}

// BuffContainerState is the container's persistable state. Instance ids are
// part of the state so restored containers keep issuing unique ids and
// attribute grant handles stay stable across a reload.
type BuffContainerState struct {
	Sequence   uint64         `json:"sequence"`
	TenacityBP int64          `json:"tenacity_bp,omitempty"`
	Active     []BuffInstance `json:"active,omitempty"`
}

// State snapshots the container. The result shares nothing with the live
// container and is safe to retain across further mutations.
func (container *BuffContainer) State() BuffContainerState {
	state := BuffContainerState{Sequence: container.sequence, TenacityBP: container.tenacityBP}
	if len(container.active) > 0 {
		state.Active = make([]BuffInstance, len(container.active))
		for index, instance := range container.active {
			instance.Spec = copyBuffSpec(instance.Spec)
			state.Active[index] = instance
		}
	}
	return state
}

// ErrBuffStateCorrupt rejects a BuffContainerState whose instances violate
// the container's invariants.
var ErrBuffStateCorrupt = errors.New("combat: buff container state corrupt")

// RestoreBuffContainer rebuilds a container from a State snapshot. Link an
// AttributeSet afterwards to re-materialize modifier grants.
//
// The snapshot is validated before use: instance ids must be unique, nonzero,
// and no greater than the sequence counter. Accepting a violating snapshot
// would let the sequence re-issue a live id later — and instance ids double
// as attribute grant handles, so a collision silently corrupts modifier
// bookkeeping far from the corrupt data. Fail here instead.
func RestoreBuffContainer(state BuffContainerState) (*BuffContainer, error) {
	seen := make(map[BuffInstanceID]struct{}, len(state.Active))
	for _, instance := range state.Active {
		if instance.Instance == 0 {
			return nil, fmt.Errorf("%w: instance id 0", ErrBuffStateCorrupt)
		}
		if uint64(instance.Instance) > state.Sequence {
			return nil, fmt.Errorf("%w: instance id %d beyond sequence %d", ErrBuffStateCorrupt, instance.Instance, state.Sequence)
		}
		if _, duplicate := seen[instance.Instance]; duplicate {
			return nil, fmt.Errorf("%w: duplicate instance id %d", ErrBuffStateCorrupt, instance.Instance)
		}
		seen[instance.Instance] = struct{}{}
	}
	container := NewBuffContainer()
	container.sequence = state.Sequence
	container.SetTenacityBP(state.TenacityBP)
	container.active = make([]BuffInstance, len(state.Active))
	for index, instance := range state.Active {
		instance.Spec = copyBuffSpec(instance.Spec)
		container.active[index] = instance
	}
	return container, nil
}
