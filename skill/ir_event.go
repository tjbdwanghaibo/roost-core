package skill

type EventID uint64
type CastID uint64

type EventContext struct {
	EventID           EventID
	RootEventID       EventID
	ParentEventID     EventID
	Tick              Tick
	WorldRevision     WorldRevision
	EmissionSequence  uint64
	Source            EntityID
	Owner             EntityID
	Target            EntityID
	SkillID           string
	CastID            CastID
	EffectIndex       EffectIndex
	ProcessID         ProcessID
	DamageType        DamageTypeHandle
	Element           ElementHandle
	ProcDepth         int
	ProcCoefficientBP int64
	Result            string
	MembershipTicks   int64
	EnterCount        int64
	gameplayTags      []GameplayTagHandle
}

func newRootEvent(id EventID) EventContext {
	return EventContext{EventID: id, RootEventID: id, ProcCoefficientBP: 10000}
}

func deriveEvent(parent EventContext, id EventID) EventContext {
	root := parent.RootEventID
	if root == 0 {
		root = parent.EventID
	}
	result := parent
	result.EventID = id
	result.RootEventID = root
	result.ParentEventID = parent.EventID
	result.gameplayTags = append([]GameplayTagHandle(nil), parent.gameplayTags...)
	return result
}

func (event EventContext) WithGameplayTags(tags []GameplayTagHandle) EventContext {
	event.gameplayTags = normalizeGameplayTagHandles(tags)
	return event
}

func (event EventContext) GameplayTags() []GameplayTagHandle {
	return append([]GameplayTagHandle(nil), event.gameplayTags...)
}

func normalizeGameplayTagHandles(tags []GameplayTagHandle) []GameplayTagHandle {
	result := append([]GameplayTagHandle(nil), tags...)
	sortGameplayTagHandles(result)
	write := 0
	for _, tag := range result {
		if write > 0 && result[write-1] == tag {
			continue
		}
		result[write] = tag
		write++
	}
	return result[:write]
}

func sortGameplayTagHandles(tags []GameplayTagHandle) {
	for index := 1; index < len(tags); index++ {
		for current := index; current > 0 && tags[current] < tags[current-1]; current-- {
			tags[current], tags[current-1] = tags[current-1], tags[current]
		}
	}
}
