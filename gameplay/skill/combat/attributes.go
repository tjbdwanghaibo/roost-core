package combat

import (
	"math"
	"math/big"
	"sort"
)

// AttributeID 标识一个属性通道（生命、护甲、急速等）；与玩法含义的对应由 Host 的 catalog 决定。
type AttributeID uint16

// ModifierHandle 标识 AttributeSet 里的一次授予，以便精确撤销。句柄由调用方提供（buff 实例 ID 是自然的选择）；
// 对已有句柄再次授予会替换它的修正。
type ModifierHandle uint64

// Modifier 调整一个属性：Flat 加到基础值之和上，RateBP 以基点增量加进加法比率桶（+2000 = +20%，-3000 = -30%）。
// 固定值与比率桶都按求和聚合，当前值与授予顺序无关——这是确定性要求。
type Modifier struct {
	Attribute AttributeID
	Flat      int64
	RateBP    int64
}

// AttributeBounds 夹取属性的当前值。
type AttributeBounds struct {
	Minimum int64
	Maximum int64
}

// AttributeSet 由基础值加上授予的修正算出当前属性值：
//
//	Current = clamp((base + Σflat) * max(0, 10000 + ΣrateBP) / 10000)
//
// 所有聚合都是精确整数求和，Grant 与 Revoke 完全可逆，结果与施加顺序无关。
type AttributeSet struct {
	base     map[AttributeID]int64
	flatSum  map[AttributeID]*big.Int
	rateSum  map[AttributeID]*big.Int
	bounds   map[AttributeID]AttributeBounds
	grants   map[ModifierHandle][]Modifier
	observer func(AttributeID)
}

// NewAttributeSet 返回空的属性集。
func NewAttributeSet() *AttributeSet {
	return &AttributeSet{
		base:    make(map[AttributeID]int64),
		flatSum: make(map[AttributeID]*big.Int),
		rateSum: make(map[AttributeID]*big.Int),
		bounds:  make(map[AttributeID]AttributeBounds),
		grants:  make(map[ModifierHandle][]Modifier),
	}
}

// Observe 注册一个回调：某个属性的输入（基础值、上下界或修正）改变时，每个属性触发一次——脏标记跟踪接在这里。
func (set *AttributeSet) Observe(observer func(AttributeID)) { set.observer = observer }

func (set *AttributeSet) notify(id AttributeID) {
	if set.observer != nil {
		set.observer(id)
	}
}

// SetBase 设置属性的基础值，值变化时通知观察者。
func (set *AttributeSet) SetBase(id AttributeID, value int64) {
	if set.base[id] == value {
		return
	}
	set.base[id] = value
	set.notify(id)
}

// Base 返回属性的基础值。
func (set *AttributeSet) Base(id AttributeID) int64 { return set.base[id] }

// SetBounds 设置属性当前值的上下界，值变化时通知观察者。
func (set *AttributeSet) SetBounds(id AttributeID, bounds AttributeBounds) {
	if set.bounds[id] == bounds {
		return
	}
	set.bounds[id] = bounds
	set.notify(id)
}

// Grant 安装句柄的修正，替换同一句柄下先前的授予。
func (set *AttributeSet) Grant(handle ModifierHandle, modifiers ...Modifier) {
	set.Revoke(handle)
	if len(modifiers) == 0 {
		return
	}
	stored := append([]Modifier(nil), modifiers...)
	set.grants[handle] = stored
	for _, modifier := range stored {
		addModifierSum(set.flatSum, modifier.Attribute, modifier.Flat, false)
		addModifierSum(set.rateSum, modifier.Attribute, modifier.RateBP, false)
		set.notify(modifier.Attribute)
	}
}

// Revoke 精确移除一次授予；未知句柄是空操作。
func (set *AttributeSet) Revoke(handle ModifierHandle) {
	modifiers, ok := set.grants[handle]
	if !ok {
		return
	}
	delete(set.grants, handle)
	for _, modifier := range modifiers {
		addModifierSum(set.flatSum, modifier.Attribute, modifier.Flat, true)
		addModifierSum(set.rateSum, modifier.Attribute, modifier.RateBP, true)
		set.notify(modifier.Attribute)
	}
}

// Current 算出属性的有效值。
func (set *AttributeSet) Current(id AttributeID) int64 {
	total := clampModifierSum(set.base[id], set.flatSum[id])
	rate := clampModifierSum(BasisPointScale, set.rateSum[id])
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

// Snapshot 按 AttributeID 升序返回每个有非零输入的属性，是供持久化与同步用的确定视图。
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

// AttributeValue 是 AttributeSet 快照里的一行。
type AttributeValue struct {
	Attribute AttributeID
	Base      int64
	Current   int64
}

// AttributeBaseState 是一个属性可持久化的配置：基础值与上下界。修正授予刻意不含在内——它们属于授予方（buff、装备），
// 恢复时由授予方重新授予。
type AttributeBaseState struct {
	Attribute AttributeID     `json:"attribute"`
	Base      int64           `json:"base"`
	HasBounds bool            `json:"has_bounds,omitempty"`
	Bounds    AttributeBounds `json:"bounds,omitempty"`
}

// BaseState 按属性升序快照基础值与上下界。
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

// RestoreBase 用给定状态替换全部基础值与上下界，对出现在任一侧的每个属性通知观察者。
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

// 聚合桶保留精确整数，不能每次加减都饱和，否则撤销不可逆、施加顺序会影响结果。
// 只在 Current 进入固定点公式时钳到 int64；常规数值仅占一个 big.Int word。
func addModifierSum(sums map[AttributeID]*big.Int, id AttributeID, value int64, remove bool) {
	sum := sums[id]
	if sum == nil {
		sum = new(big.Int)
		sums[id] = sum
	}
	operand := big.NewInt(value)
	if remove {
		sum.Sub(sum, operand)
	} else {
		sum.Add(sum, operand)
	}
}
func clampModifierSum(base int64, sum *big.Int) int64 {
	if sum == nil {
		return base
	}
	total := new(big.Int).Add(big.NewInt(base), sum)
	if total.IsInt64() {
		return total.Int64()
	}
	if total.Sign() < 0 {
		return math.MinInt64
	}
	return math.MaxInt64
}
