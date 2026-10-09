package skill

type StatusInstanceID struct{ opaque uint64 }

// NewStatusInstanceID wraps a host-issued opaque instance id. External Host
// implementations need it to build StatusInstanceRef values for Select
// results and instance events; the id's meaning is entirely host-defined
// (combatcomponent uses the combat.BuffInstanceID).
func NewStatusInstanceID(opaque uint64) StatusInstanceID { return StatusInstanceID{opaque: opaque} }

func (id StatusInstanceID) OpaqueID() uint64 { return id.opaque }

type StatusInstanceRef struct {
	ID     StatusInstanceID
	Target EntityID
}

type StatusSetSelectShape struct{ Target EntityID }

func (StatusSetSelectShape) isSelectShape() {}

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
