package skill

// StatusInstanceID 是 Host 签发的不透明状态实例 ID。
type StatusInstanceID struct{ opaque uint64 }

// NewStatusInstanceID 包装 Host 签发的不透明实例 ID。外部 Host 实现用它为选择结果与实例事件构造 StatusInstanceRef；
// ID 的含义完全由 Host 定义（combatcomponent 用的是 combat.BuffInstanceID）。
func NewStatusInstanceID(opaque uint64) StatusInstanceID { return StatusInstanceID{opaque: opaque} }

// OpaqueID 返回 Host 签发的原始 ID。
func (id StatusInstanceID) OpaqueID() uint64 { return id.opaque }

// StatusInstanceRef 引用目标身上的一个状态实例。
type StatusInstanceRef struct {
	ID     StatusInstanceID
	Target EntityID
}

// StatusSetSelectShape 选择目标身上的状态实例。
type StatusSetSelectShape struct{ Target EntityID }

func (StatusSetSelectShape) isSelectShape() {}

// StatusIDSelectFilter 等按状态 ID、文本 / 标签、标志、相关实体、来源技能与数值比较过滤状态实例。
type StatusIDSelectFilter struct{ Status StatusHandle }
type StatusTextSelectFilter struct {
	Kind, Value string
	Tag         GameplayTagHandle
}
type StatusFlagSelectFilter struct{ Kind string }
type StatusEntitySelectFilter struct {
	Kind   string
	Entity EntityID
}
type StatusSourceSkillSelectFilter struct{ SkillID string }
type StatusCompareSelectFilter struct {
	Kind, Operation string
	Value           int64
}

func (StatusIDSelectFilter) isSelectFilter()          {}
func (StatusTextSelectFilter) isSelectFilter()        {}
func (StatusFlagSelectFilter) isSelectFilter()        {}
func (StatusEntitySelectFilter) isSelectFilter()      {}
func (StatusSourceSkillSelectFilter) isSelectFilter() {}
func (StatusCompareSelectFilter) isSelectFilter()     {}

// ModifyStatusInstanceCommand 对一个状态实例做偷取、转移、层数或时长修改。
type ModifyStatusInstanceCommand struct {
	Owner           EntityID
	SourceSkillID   string
	Status          StatusInstanceRef
	Operation       string
	Value           int64
	Target          EntityID
	OwnershipPolicy string
	Event           EventContext
	Meta            CommandMeta
}

func (ModifyStatusInstanceCommand) isEffectCommandPayload() {}
