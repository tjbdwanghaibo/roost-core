package skill

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

type OwnedEntityCommand struct {
	Owner          EntityID
	GameplayDigest string
	Target         EntityID
	Command        string
	Position       Position
	TargetEntity   EntityID
	Behavior       string
}

type OwnedEntitiesSelectShape struct{ Owner EntityID }

func (OwnedEntitiesSelectShape) isSelectShape() {}

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

type OwnedSummonPreview struct {
	ReplacedEntities []EntityID
	FailureReason    ExpectedFailureReason
}

type OwnedSummonTransactionID uint64

type OwnedEntityRuntimeHost interface {
	PreviewOwnedSummon(SummonCommand) (OwnedSummonPreview, error)
	OwnedEntity(EntityID) (OwnedEntityMetadata, bool)
	CommitOwnedSummon(OwnedSummonTransactionID) error
	RollbackOwnedSummon(OwnedSummonTransactionID) error
	RemoveOwnedEntitiesByProgram(string) error
	RemoveOwnedEntitiesForMatchEnd() error
}
