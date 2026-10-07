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
	SpawnID           SpawnID
	DamageType        DamageTypeHandle
	Element           ElementHandle
	ProcDepth         int
	ProcCoefficientBP int64
	Result            string
	MembershipTicks   int64
	EnterCount        int64
	gameplayTags      []GameplayTagHandle
}

// 事件 ID 的编号空间（RR-20261006-53 / 54）：
//
//	主动施法（根事件）            castID
//	proc 施放自己的事件           castID<<32（低 32 位为 0）
//	施法流程里第 i 号效果         castID<<32 | (i+1)
//	衍生物回调、回调里的每个效果  spawnEventIDBit | 序号（nextSpawnEventID，Runtime 级计数，随 checkpoint 保存）
//
// 前三类的 castID 小于 2^31 时不置最高位，与第四类不相交（castID 到 2^32 时第二、三类本来就会回绕）。衍生物回调跑在
// 脱离施法的 cast 上，cast.id 是所属施法：按前三类编号，同一个回调效果每个 tick、每个目标都是同一个 ID，所以单独编号。
const spawnEventIDBit EventID = 1 << 63

// nextSpawnEventID 给衍生物回调或回调里的一次效果结算分配一个新的事件 ID。
func (runtime *Runtime) nextSpawnEventID() EventID {
	runtime.spawnEventSequence++
	return spawnEventIDBit | EventID(runtime.spawnEventSequence)
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
