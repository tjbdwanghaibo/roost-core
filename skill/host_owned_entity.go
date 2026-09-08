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
	SpawnTick         Tick
	SpawnSequence     uint64
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
type OwnedSpawnTickFilter struct {
	Operation string
	Tick      Tick
}

func (OwnedSourceSkillFilter) isSelectFilter()  {}
func (OwnedSourceCastFilter) isSelectFilter()   {}
func (OwnedUnitTemplateFilter) isSelectFilter() {}
func (OwnedEntityTagFilter) isSelectFilter()    {}
func (OwnedSpawnTickFilter) isSelectFilter()    {}

func validOwnedReplacementPolicy(policy string) bool {
	switch policy {
	case "reject_new", "replace_oldest", "replace_newest", "replace_nearest", "replace_farthest":
		return true
	default:
		return false
	}
}

type OwnedSpawnPreview struct {
	ReplacedEntities []EntityID
	FailureReason    ExpectedFailureReason
}

type OwnedSpawnTransactionID uint64

type OwnedEntityRuntimeHost interface {
	PreviewOwnedSpawn(SpawnCommand) (OwnedSpawnPreview, error)
	OwnedEntity(EntityID) (OwnedEntityMetadata, bool)
	CommitOwnedSpawn(OwnedSpawnTransactionID) error
	RollbackOwnedSpawn(OwnedSpawnTransactionID) error
	RemoveOwnedEntitiesByProgram(string) error
	RemoveOwnedEntitiesForMatchEnd() error
}
