package combat

import "sort"

// AttributeID identifies one attribute channel (health, armor, haste, ...).
// The mapping to gameplay meaning belongs to the host's catalog.
type AttributeID uint16

// ModifierHandle identifies one grant inside an AttributeSet so it can be
// revoked exactly. Handles are caller-supplied (buff instance ids are a
// natural choice); granting an existing handle replaces its modifiers.
type ModifierHandle uint64

// Modifier adjusts one attribute: Flat adds to the base sum, RateBP adds to
// the additive rate bucket in basis-point deltas (+2000 = +20%, -3000 =
// -30%). Flat and rate buckets aggregate by summation, so the current value
// is independent of grant order — a determinism requirement.
type Modifier struct {
	Attribute AttributeID
	Flat      int64
	RateBP    int64
}

// AttributeBounds clamps an attribute's current value.
type AttributeBounds struct {
	Minimum int64
	Maximum int64
}

// AttributeSet resolves current attribute values from a base value plus
// granted modifiers:
//
//	Current = clamp((base + Σflat) * max(0, 10000 + ΣrateBP) / 10000)
//
// All aggregation is exact integer summation, so Grant and Revoke are
// perfectly reversible and the result never depends on application order.
type AttributeSet struct {
	base     map[AttributeID]int64
	flatSum  map[AttributeID]int64
	rateSum  map[AttributeID]int64
	bounds   map[AttributeID]AttributeBounds
	grants   map[ModifierHandle][]Modifier
	observer func(AttributeID)
}

func NewAttributeSet() *AttributeSet {
	return &AttributeSet{
		base:    make(map[AttributeID]int64),
		flatSum: make(map[AttributeID]int64),
		rateSum: make(map[AttributeID]int64),
		bounds:  make(map[AttributeID]AttributeBounds),
		grants:  make(map[ModifierHandle][]Modifier),
	}
}

// Observe registers a callback fired once per attribute whose inputs change
// (base, bounds, or modifiers) — the hook dirty tracking plugs into.
func (set *AttributeSet) Observe(observer func(AttributeID)) { set.observer = observer }

func (set *AttributeSet) notify(id AttributeID) {
	if set.observer != nil {
		set.observer(id)
	}
}

func (set *AttributeSet) SetBase(id AttributeID, value int64) {
	if set.base[id] == value {
		return
	}
	set.base[id] = value
	set.notify(id)
}

func (set *AttributeSet) Base(id AttributeID) int64 { return set.base[id] }

func (set *AttributeSet) SetBounds(id AttributeID, bounds AttributeBounds) {
	if set.bounds[id] == bounds {
		return
	}
	set.bounds[id] = bounds
	set.notify(id)
}

// Grant installs the handle's modifiers, replacing any prior grant under the
// same handle.
func (set *AttributeSet) Grant(handle ModifierHandle, modifiers ...Modifier) {
	set.Revoke(handle)
	if len(modifiers) == 0 {
		return
	}
	stored := append([]Modifier(nil), modifiers...)
	set.grants[handle] = stored
	for _, modifier := range stored {
		set.flatSum[modifier.Attribute] += modifier.Flat
		set.rateSum[modifier.Attribute] += modifier.RateBP
		set.notify(modifier.Attribute)
	}
}

// Revoke removes a grant exactly; unknown handles are a no-op.
func (set *AttributeSet) Revoke(handle ModifierHandle) {
	modifiers, ok := set.grants[handle]
	if !ok {
		return
	}
	delete(set.grants, handle)
	for _, modifier := range modifiers {
		set.flatSum[modifier.Attribute] -= modifier.Flat
		set.rateSum[modifier.Attribute] -= modifier.RateBP
		set.notify(modifier.Attribute)
	}
}

// Current resolves the attribute's effective value.
func (set *AttributeSet) Current(id AttributeID) int64 {
	total := saturatingInt64Add(set.base[id], set.flatSum[id])
	rate := saturatingInt64Add(BasisPointScale, set.rateSum[id])
	if rate < 0 {
		rate = 0
	}
	value := ScaleBasisPoints(total, rate)
	if bounds, ok := set.bounds[id]; ok {
		if value < bounds.Minimum {
			value = bounds.Minimum
		}
		if value > bounds.Maximum {
			value = bounds.Maximum
		}
	}
	return value
}

// Snapshot returns every attribute with a non-zero input, in ascending
// AttributeID order — a deterministic view for persistence and sync.
func (set *AttributeSet) Snapshot() []AttributeValue {
	present := make(map[AttributeID]struct{}, len(set.base)+len(set.flatSum))
	for id := range set.base {
		present[id] = struct{}{}
	}
	for id := range set.flatSum {
		present[id] = struct{}{}
	}
	for id := range set.rateSum {
		present[id] = struct{}{}
	}
	order := make([]AttributeID, 0, len(present))
	for id := range present {
		order = append(order, id)
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	result := make([]AttributeValue, 0, len(order))
	for _, id := range order {
		result = append(result, AttributeValue{Attribute: id, Base: set.base[id], Current: set.Current(id)})
	}
	return result
}

// AttributeValue is one row of an AttributeSet snapshot.
type AttributeValue struct {
	Attribute AttributeID
	Base      int64
	Current   int64
}

// AttributeBaseState is one attribute's persistable configuration: base
// value and bounds. Modifier grants are intentionally excluded — they belong
// to their granters (buffs, equipment), which re-grant on restore.
type AttributeBaseState struct {
	Attribute AttributeID     `json:"attribute"`
	Base      int64           `json:"base"`
	HasBounds bool            `json:"has_bounds,omitempty"`
	Bounds    AttributeBounds `json:"bounds,omitempty"`
}

// BaseState snapshots base values and bounds in ascending attribute order.
func (set *AttributeSet) BaseState() []AttributeBaseState {
	present := make(map[AttributeID]struct{}, len(set.base)+len(set.bounds))
	for id := range set.base {
		present[id] = struct{}{}
	}
	for id := range set.bounds {
		present[id] = struct{}{}
	}
	order := make([]AttributeID, 0, len(present))
	for id := range present {
		order = append(order, id)
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	result := make([]AttributeBaseState, 0, len(order))
	for _, id := range order {
		state := AttributeBaseState{Attribute: id, Base: set.base[id]}
		if bounds, ok := set.bounds[id]; ok {
			state.HasBounds, state.Bounds = true, bounds
		}
		result = append(result, state)
	}
	return result
}

// RestoreBase replaces every base value and bound with the given state,
// notifying the observer for each attribute that appears on either side.
func (set *AttributeSet) RestoreBase(states []AttributeBaseState) {
	touched := make(map[AttributeID]struct{}, len(set.base)+len(states))
	for id := range set.base {
		touched[id] = struct{}{}
	}
	for id := range set.bounds {
		touched[id] = struct{}{}
	}
	set.base = make(map[AttributeID]int64, len(states))
	set.bounds = make(map[AttributeID]AttributeBounds, len(states))
	for _, state := range states {
		set.base[state.Attribute] = state.Base
		if state.HasBounds {
			set.bounds[state.Attribute] = state.Bounds
		}
		touched[state.Attribute] = struct{}{}
	}
	order := make([]AttributeID, 0, len(touched))
	for id := range touched {
		order = append(order, id)
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	for _, id := range order {
		set.notify(id)
	}
}
