package combat

import (
	"errors"
	"fmt"
)

// BuffID 标识 Host catalog 里的一个 buff 定义。
type BuffID uint32

// BuffInstanceID 标识容器里的一次施加。实例 ID 是每个容器自己的序号，因此同时充当 AttributeSet 的修正句柄与
// 确定的排序键。
type BuffInstanceID uint64

// Tag 给 buff 分类，用于驱散与免疫匹配。
type Tag string

// BuffStackPolicy 决定对已存在的 buff 再次施加时怎么做。
type BuffStackPolicy uint8

const (
	// BuffRefresh 加一层（不超过 MaxStacks）并重置持续时间。
	BuffRefresh BuffStackPolicy = iota
	// BuffExtend 加一层，并把剩余时间延长 spec 的持续时间（经韧性调整）。
	BuffExtend
	// BuffIgnore 保持已有实例不变。
	BuffIgnore
	// BuffIndependent 总是新建一个有自己持续时间的实例，从不与同 ID 的已有实例合并（独立计时的 DoT、一次性属性修正）。
	BuffIndependent
)

// BuffSpec 是 buff 定义。spec 是调用方拥有的值数据，容器复制它要保存的部分。
type BuffSpec struct {
	ID   BuffID
	Tags []Tag
	// GrantsImmunityTo：本 buff 生效期间，阻止之后施加任何带这些标签的 buff。
	GrantsImmunityTo []Tag
	MaxStacks        int64 // <=1 表示不叠层
	StackPolicy      BuffStackPolicy
	DurationTicks    int64 // <=0 表示永久，直到被驱散
	// TenacityAffected 的持续时间按容器的韧性缩短：duration * (10000 - tenacityBP) / 10000，至少 1 tick。
	TenacityAffected bool
	// MaxDurationTicks 为正时，在韧性缩放之后封顶有效持续时间。
	MaxDurationTicks int64
	// Modifiers 按层数授予给关联的 AttributeSet。
	Modifiers []Modifier
}

// BuffApplyOutcome 报告 Apply 的结果。
type BuffApplyOutcome uint8

const (
	BuffApplied BuffApplyOutcome = iota
	BuffStacked
	BuffRefreshed
	BuffIgnored
	BuffBlockedImmune
)

// BuffInstance 是一个生效中的 buff。
type BuffInstance struct {
	Instance    BuffInstanceID
	Spec        BuffSpec
	Stacks      int64
	Source      int64
	AppliedTick int64
	DueTick     int64 // 0 表示永久
}

// BuffContainer 持有一个战斗单位生效中的 buff，实现叠层、驱散标签、免疫与韧性语义。实例保持施加顺序（按实例 ID），
// 每次遍历都是确定的。关联 AttributeSet 后，buff 的修正按层数自动落成属性授予。
type BuffContainer struct {
	sequence   uint64
	active     []BuffInstance
	tenacityBP int64
	attributes *AttributeSet
}

// NewBuffContainer 返回空的 buff 容器。
func NewBuffContainer() *BuffContainer { return &BuffContainer{} }

// LinkAttributes 把容器关联到一个 AttributeSet，并立即重新授予生效中 buff 的修正。
func (container *BuffContainer) LinkAttributes(attributes *AttributeSet) {
	container.attributes = attributes
	for index := range container.active {
		container.grantModifiers(&container.active[index])
	}
}

// SetTenacityBP 设置以基点表示的控制抗性（2000 表示受韧性影响的 buff 持续时间 -20%），夹在 [0, 10000]。
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

// ImmuneTo 报告是否有生效中的 buff 对该标签免疫。
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

// Apply 在给定 tick 施加（或再次施加）一个 buff。
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
			if duration == 0 {
				instance.DueTick = 0
			} else if instance.DueTick != 0 {
				instance.DueTick = saturatingInt64Add(instance.DueTick, duration)
			}
		default: // BuffRefresh
			instance.DueTick = 0
			if duration > 0 {
				instance.DueTick = saturatingInt64Add(tick, duration)
			}
		}
		outcome := BuffRefreshed
		maxStacks := maxInt64(spec.MaxStacks, 1)
		instance.Stacks = minInt64(instance.Stacks, maxStacks)
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

// Dispel 移除至多 limit 个带该标签的生效 buff，最新的先移除（limit <= 0 表示全部），返回被移除的实例。
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

// Remove 按 ID 移除一个实例。
func (container *BuffContainer) Remove(id BuffInstanceID) (BuffInstance, bool) {
	for index := range container.active {
		if container.active[index].Instance == id {
			return container.removeAt(index), true
		}
	}
	return BuffInstance{}, false
}

// SetStacks 固定实例的层数（夹在 [0, MaxStacks]），并按新层数重新落成属性授予。层数为 0 时移除实例。
// 返回修改后的实例。
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

// SetDueTick 固定实例的到期 tick（0 表示永久）。
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

// Adopt 以新的本地实例 ID 注入一个外来实例（从另一个容器复制或转移），绕过免疫与叠层规则：调用方已经判定这次
// 移动合法。被接收的实例保留 spec、层数、来源与到期时间。
func (container *BuffContainer) Adopt(instance BuffInstance) BuffInstanceID {
	container.sequence++
	instance.Instance = BuffInstanceID(container.sequence)
	instance.Spec = copyBuffSpec(instance.Spec)
	container.active = append(container.active, instance)
	container.grantModifiers(&container.active[len(container.active)-1])
	return instance.Instance
}

// Tick 让所有到期 tick 不晚于 now 的实例过期，按施加顺序返回它们。
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

// NextDue 返回生效中实例里最早的到期 tick；没有限时实例（全是永久的，或容器为空）时 ok 为 false。
func (container *BuffContainer) NextDue() (due int64, ok bool) {
	for _, instance := range container.active {
		if instance.DueTick != 0 && (!ok || instance.DueTick < due) {
			due, ok = instance.DueTick, true
		}
	}
	return due, ok
}

// Active 按施加顺序返回生效中的实例。切片是副本；其中的实例是容器的值。
func (container *BuffContainer) Active() []BuffInstance {
	return append([]BuffInstance(nil), container.active...)
}

// HasTag 报告是否有生效中的 buff 带该标签。
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

// BuffContainerState 是容器可持久化的状态。实例 ID 属于状态：恢复后的容器继续签发唯一 ID，属性授予句柄在重新加载
// 前后保持稳定。
type BuffContainerState struct {
	Sequence   uint64         `json:"sequence"`
	TenacityBP int64          `json:"tenacity_bp,omitempty"`
	Active     []BuffInstance `json:"active,omitempty"`
}

// State 快照容器。结果不与活的容器共享任何东西，之后继续修改容器也可以安全保留。
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

// ErrBuffStateCorrupt 拒绝实例违反容器不变量的 BuffContainerState。
var ErrBuffStateCorrupt = errors.New("combat: buff container state corrupt")

// RestoreBuffContainer 从 State 快照重建容器；之后关联 AttributeSet 以重新落成修正授予。
//
// 使用前先校验快照：实例 ID 必须唯一、非零且不大于序号计数器。接受违规快照会让序号之后重新签发一个仍在用的
// ID——实例 ID 同时是属性授予句柄，冲突会在远离损坏数据的地方静默破坏修正记账，所以在这里就失败。
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
