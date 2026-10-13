package skill

// OwnedEntityMetadata 是 Host 记录的一个召唤物：归属、来源、模板、标签、召唤时刻与寿命。
type OwnedEntityMetadata struct {
	Entity            EntityID
	Owner             EntityID
	GameplayDigest    string
	SourceSkillID     string
	SourceCastID      CastID
	SourceEffectIndex EffectIndex
	Template          UnitTemplateHandle
	GameplayTags      []GameplayTagHandle
	SummonTick        Tick
	SummonSequence    uint64
	LifetimeTicks     Tick
	DueTick           Tick
	ControlProfile    string
	ParameterBindings map[string]RuntimeValue
}

// OwnedEntityCommand 是对 Owner 名下召唤物下达的指令（移动、攻击、行为切换等）。
type OwnedEntityCommand struct {
	Owner          EntityID
	GameplayDigest string
	Target         EntityID
	Command        string
	Position       Position
	TargetEntity   EntityID
	Behavior       string
}

// OwnedEntitiesSelectShape 选择 Owner 名下的召唤物。
type OwnedEntitiesSelectShape struct{ Owner EntityID }

func (OwnedEntitiesSelectShape) isSelectShape() {}

// OwnedSourceSkillFilter 等按来源技能、来源施法、单位模板、标签与召唤时刻过滤召唤物。
type OwnedSourceSkillFilter struct{ SkillID string }
type OwnedSourceCastFilter struct{ CastID CastID }
type OwnedUnitTemplateFilter struct{ Template UnitTemplateHandle }
type OwnedEntityTagFilter struct{ Tag GameplayTagHandle }
type OwnedSummonTickFilter struct {
	Operation string
	Tick      Tick
}

func (OwnedSourceSkillFilter) isSelectFilter()  {}
func (OwnedSourceCastFilter) isSelectFilter()   {}
func (OwnedUnitTemplateFilter) isSelectFilter() {}
func (OwnedEntityTagFilter) isSelectFilter()    {}
func (OwnedSummonTickFilter) isSelectFilter()   {}

func validOwnedReplacementPolicy(policy string) bool {
	switch policy {
	case "reject_new", "replace_oldest", "replace_newest", "replace_nearest", "replace_farthest":
		return true
	default:
		return false
	}
}

// OwnedSummonPreview 是两阶段召唤的预检结果：将被替换的实体与可预期的失败原因。
type OwnedSummonPreview struct {
	ReplacedEntities []EntityID
	FailureReason    ExpectedFailureReason
}

// OwnedSummonTransactionID 标识一次两阶段召唤事务。
type OwnedSummonTransactionID uint64

// OwnedEntityRuntimeHost 是支持召唤物的 Host 额外实现的接口（两阶段召唤、查询与按程序 / 对局结束清理）。
type OwnedEntityRuntimeHost interface {
	PreviewOwnedSummon(SummonCommand) (OwnedSummonPreview, error)
	OwnedEntity(EntityID) (OwnedEntityMetadata, bool)
	CommitOwnedSummon(OwnedSummonTransactionID) error
	RollbackOwnedSummon(OwnedSummonTransactionID) error
	RemoveOwnedEntitiesByProgram(string) error
	RemoveOwnedEntitiesForMatchEnd() error
}
