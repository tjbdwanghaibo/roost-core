package skill

// PresentationEventKind 是面向渲染端的事件种类。表现数据刻意与玩法的 RuntimeEvent、TraceEvent 流分开：
// 丢弃或重放表现不改变权威模拟。
type PresentationEventKind string

const (
	PresentationCast        PresentationEventKind = "cast"
	PresentationEffect      PresentationEventKind = "effect"
	PresentationSpawnStart  PresentationEventKind = "spawn_start"
	PresentationSpawnUpdate PresentationEventKind = "spawn_update"
	PresentationSpawnSignal PresentationEventKind = "spawn_signal"
	PresentationSpawnStop   PresentationEventKind = "spawn_stop"
)

// PresentationAnchor 是表现事件的锚点：来源、目标、位置、方向与路径。
type PresentationAnchor struct {
	Source    EntityID
	Target    EntityID
	Position  *Position
	Direction *Direction
	Path      []Position
}

// PresentationMount 把不可变表现清单里的一项连接到编译出的玩法挂载点。
type PresentationMount struct {
	VisualIndex   VisualIndex
	EffectIndex   EffectIndex
	HasEffect     bool
	SpawnTemplate SpawnTemplateIndex
	HasSpawn      bool
}

// PresentationPlan 是 Program 稳定、可缓存的渲染契约；客户端按 Identity.PresentationDigest 作键。
type PresentationPlan struct {
	Identity ProgramIdentityView
	Manifest SkillVisualManifest
	Cast     *PresentationMount
	Effects  []PresentationMount
	Spawns   []PresentationMount
}

// InspectPresentationPlan 返回不可变 Program 的脱离副本投影；修改返回的切片不会改到 Program。
func InspectPresentationPlan(program *Program) PresentationPlan {
	if program == nil {
		return PresentationPlan{}
	}
	plan := PresentationPlan{Identity: InspectIdentity(program), Manifest: InspectVisualManifest(program)}
	if program.hasCastVisual {
		plan.Cast = &PresentationMount{VisualIndex: program.castVisual}
	}
	for _, operation := range program.operations {
		continuations, effectIndex, ok := operationEffectContinuations(operation)
		if !ok || !continuations.hasVisual {
			continue
		}
		plan.Effects = append(plan.Effects, PresentationMount{
			VisualIndex: continuations.visual, EffectIndex: effectIndex, HasEffect: true,
			SpawnTemplate: continuations.spawnTemplate, HasSpawn: continuations.hasSpawn,
		})
	}
	for _, template := range program.spawnTemplates {
		if !template.hasVisual {
			continue
		}
		plan.Spawns = append(plan.Spawns, PresentationMount{VisualIndex: template.visual, SpawnTemplate: template.index, HasSpawn: true})
	}
	return plan
}

// PresentationEvent 是发给客户端的有序运行期指令。Sequence 只在一个 Runtime 内有意义，WorldRevision 标识事件
// 提交时对应的权威状态。
type PresentationEvent struct {
	Sequence           uint64
	Tick               Tick
	WorldRevision      WorldRevision
	Kind               PresentationEventKind
	ProgramID          string
	GameplayDigest     string
	PresentationDigest string
	CastID             CastID
	EffectIndex        EffectIndex
	HasEffect          bool
	VisualIndex        VisualIndex
	Source             EntityID
	PrimaryTarget      EntityID
	Anchor             PresentationAnchor
	SpawnID            SpawnID
	SpawnTemplate      SpawnTemplateIndex
	HasSpawn           bool
	SpawnStatus        SpawnStatus
	SpawnSignal        SpawnSignalKind
	StopCause          StopCause
}

// PresentationBatch 是 PollPresentation 返回的一批表现事件及游标状态。
type PresentationBatch struct {
	Events         []PresentationEvent
	OldestSequence uint64
	LatestSequence uint64
	CursorExpired  bool
	More           bool
}

// PresentationEvents 返回 Sequence 大于 after 的事件；返回的存储与 Runtime 的缓冲脱离。
func (runtime *Runtime) PresentationEvents(after uint64) []PresentationEvent {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	index := 0
	for index < len(runtime.presentationEvents) && runtime.presentationEvents[index].Sequence <= after {
		index++
	}
	return append([]PresentationEvent(nil), runtime.presentationEvents[index:]...)
}

// PollPresentation 显式报告保留丢失：CursorExpired 为 true 时，调用方必须先从权威状态恢复，再应用新事件。
func (runtime *Runtime) PollPresentation(after uint64, limit int) PresentationBatch {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	batch := PresentationBatch{LatestSequence: runtime.presentationSequence}
	if len(runtime.presentationEvents) == 0 {
		batch.OldestSequence = runtime.presentationSequence + 1
		batch.CursorExpired = after < runtime.presentationSequence
		return batch
	}
	batch.OldestSequence = runtime.presentationEvents[0].Sequence
	if after+1 < batch.OldestSequence {
		batch.CursorExpired = true
		return batch
	}
	index := 0
	for index < len(runtime.presentationEvents) && runtime.presentationEvents[index].Sequence <= after {
		index++
	}
	end := len(runtime.presentationEvents)
	if limit > 0 && index+limit < end {
		end = index + limit
		batch.More = true
	}
	batch.Events = clonePresentationEvents(runtime.presentationEvents[index:end])
	return batch
}

func (runtime *Runtime) emitCastPresentation(cast *castInstance) {
	if cast == nil || cast.program == nil || !cast.program.hasCastVisual {
		return
	}
	runtime.appendPresentation(cast, PresentationEvent{
		WorldRevision: runtime.host.CurrentRevision(), Kind: PresentationCast,
		VisualIndex: cast.program.castVisual, Anchor: PresentationAnchor{Source: cast.caster, Target: cast.primaryTarget},
	})
}

func (runtime *Runtime) emitEffectPresentation(cast *castInstance, continuations effectContinuations, effectIndex EffectIndex, revision WorldRevision, anchor PresentationAnchor) {
	if cast == nil || cast.program == nil || !continuations.hasVisual {
		return
	}
	runtime.appendPresentation(cast, PresentationEvent{
		WorldRevision: revision, Kind: PresentationEffect,
		EffectIndex: effectIndex, HasEffect: true, VisualIndex: continuations.visual,
		SpawnTemplate: continuations.spawnTemplate, HasSpawn: continuations.hasSpawn, Anchor: clonePresentationAnchor(anchor),
	})
}

func (runtime *Runtime) emitSpawnPresentation(cast *castInstance, spawn *SpawnInstance, kind PresentationEventKind, signal SpawnSignalKind, cause StopCause, revision WorldRevision) {
	if spawn == nil || spawn.Program == nil || int(spawn.TemplateIndex) >= len(spawn.Program.spawnTemplates) {
		return
	}
	template := spawn.Program.spawnTemplates[spawn.TemplateIndex]
	if !template.hasVisual {
		return
	}
	if cast == nil {
		cast = runtime.detachedSpawnCast(spawn, evalSpawnCallback)
	}
	position, direction := spawn.Motion.Position, spawn.Motion.Direction
	runtime.appendPresentation(cast, PresentationEvent{
		WorldRevision: revision, Kind: kind, VisualIndex: template.visual,
		SpawnID: spawn.ID, SpawnTemplate: spawn.TemplateIndex, HasSpawn: true,
		SpawnStatus: spawn.Status, SpawnSignal: signal, StopCause: cause,
		Anchor: PresentationAnchor{Source: spawn.Owner, Target: spawn.LifecycleEntity, Position: &position, Direction: &direction},
	})
}

func (runtime *Runtime) emitSpawnSignals(cast *castInstance, spawn *SpawnInstance, signals []SpawnSignal, revision WorldRevision) {
	for _, signal := range normalizeSpawnSignals(signals) {
		view := *spawn
		if signal.Target != 0 {
			view.LifecycleEntity = signal.Target
		}
		runtime.emitSpawnPresentation(cast, &view, PresentationSpawnSignal, signal.Kind, "", revision)
	}
}

func (runtime *Runtime) appendPresentation(cast *castInstance, event PresentationEvent) {
	runtime.presentationSequence++
	event.Sequence = runtime.presentationSequence
	event.Tick = runtime.currentTick
	event.ProgramID = cast.program.id
	event.GameplayDigest = cast.program.identity.gameplayDigest
	event.PresentationDigest = cast.program.identity.presentationDigest
	event.CastID = cast.id
	event.Source = cast.caster
	event.PrimaryTarget = cast.primaryTarget
	if event.Anchor.Source == 0 {
		event.Anchor.Source = cast.caster
	}
	if event.Anchor.Target == 0 {
		event.Anchor.Target = cast.primaryTarget
	}
	event.Anchor = clonePresentationAnchor(event.Anchor)
	runtime.presentationEvents = append(runtime.presentationEvents, event)
	limit := runtime.options.PresentationLimit
	if overflow := len(runtime.presentationEvents) - limit; overflow > 0 {
		copy(runtime.presentationEvents, runtime.presentationEvents[overflow:])
		runtime.presentationEvents = runtime.presentationEvents[:limit]
	}
}

func clonePresentationAnchor(anchor PresentationAnchor) PresentationAnchor {
	if anchor.Position != nil {
		value := *anchor.Position
		anchor.Position = &value
	}
	if anchor.Direction != nil {
		value := *anchor.Direction
		anchor.Direction = &value
	}
	anchor.Path = append([]Position(nil), anchor.Path...)
	return anchor
}

func clonePresentationEvents(events []PresentationEvent) []PresentationEvent {
	result := append([]PresentationEvent(nil), events...)
	for index := range result {
		result[index].Anchor = clonePresentationAnchor(result[index].Anchor)
	}
	return result
}

func presentationEffectMount(program *Program, effectIndex EffectIndex) (effectContinuations, bool) {
	if program == nil {
		return effectContinuations{}, false
	}
	for _, operation := range program.operations {
		continuations, index, ok := operationEffectContinuations(operation)
		if ok && index == effectIndex {
			return continuations, true
		}
	}
	return effectContinuations{}, false
}
