package skill

// PresentationEventKind describes a renderer-facing event. Presentation data
// is deliberately separate from gameplay RuntimeEvent and TraceEvent streams:
// it may be dropped and replayed without changing authoritative simulation.
type PresentationEventKind string

const (
	PresentationCast          PresentationEventKind = "cast"
	PresentationEffect        PresentationEventKind = "effect"
	PresentationProcessStart  PresentationEventKind = "process_start"
	PresentationProcessUpdate PresentationEventKind = "process_update"
	PresentationProcessSignal PresentationEventKind = "process_signal"
	PresentationProcessStop   PresentationEventKind = "process_stop"
)

type PresentationAnchor struct {
	Source    EntityID
	Target    EntityID
	Position  *Position
	Direction *Direction
	Path      []Position
}

// PresentationMount connects an immutable visual manifest entry to a compiled
// gameplay mount point.
type PresentationMount struct {
	VisualIndex     VisualIndex
	EffectIndex     EffectIndex
	HasEffect       bool
	ProcessTemplate ProcessTemplateIndex
	HasProcess      bool
}

// PresentationPlan is the stable, cacheable renderer contract for a Program.
// A client should key it by Identity.PresentationDigest.
type PresentationPlan struct {
	Identity  ProgramIdentityView
	Manifest  SkillVisualManifest
	Cast      *PresentationMount
	Effects   []PresentationMount
	Processes []PresentationMount
}

// InspectPresentationPlan returns a detached projection of the immutable
// compiled program. Mutating the returned slices cannot mutate the Program.
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
			ProcessTemplate: continuations.processTemplate, HasProcess: continuations.hasProcess,
		})
	}
	for _, template := range program.processTemplates {
		if !template.hasVisual {
			continue
		}
		plan.Processes = append(plan.Processes, PresentationMount{VisualIndex: template.visual, ProcessTemplate: template.index, HasProcess: true})
	}
	return plan
}

// PresentationEvent is an ordered runtime instruction for clients. Sequence is
// local to one Runtime and WorldRevision identifies the authoritative state the
// event was committed against.
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
	ProcessID          ProcessID
	ProcessTemplate    ProcessTemplateIndex
	HasProcess         bool
	ProcessStatus      ProcessStatus
	ProcessSignal      ProcessSignalKind
	StopCause          StopCause
}

type PresentationBatch struct {
	Events         []PresentationEvent
	OldestSequence uint64
	LatestSequence uint64
	CursorExpired  bool
	More           bool
}

// PresentationEvents returns events with Sequence greater than after. Returned
// storage is detached from the runtime buffer.
func (runtime *Runtime) PresentationEvents(after uint64) []PresentationEvent {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	index := 0
	for index < len(runtime.presentationEvents) && runtime.presentationEvents[index].Sequence <= after {
		index++
	}
	return append([]PresentationEvent(nil), runtime.presentationEvents[index:]...)
}

// PollPresentation makes retention loss explicit. When CursorExpired is true,
// the caller must recover from authoritative state before applying new events.
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
		ProcessTemplate: continuations.processTemplate, HasProcess: continuations.hasProcess, Anchor: clonePresentationAnchor(anchor),
	})
}

func (runtime *Runtime) emitProcessPresentation(cast *castInstance, process *ProcessInstance, kind PresentationEventKind, signal ProcessSignalKind, cause StopCause, revision WorldRevision) {
	if process == nil || process.Program == nil || int(process.TemplateIndex) >= len(process.Program.processTemplates) {
		return
	}
	template := process.Program.processTemplates[process.TemplateIndex]
	if !template.hasVisual {
		return
	}
	if cast == nil {
		cast = runtime.detachedProcessCast(process)
	}
	position, direction := process.Motion.Position, process.Motion.Direction
	runtime.appendPresentation(cast, PresentationEvent{
		WorldRevision: revision, Kind: kind, VisualIndex: template.visual,
		ProcessID: process.ID, ProcessTemplate: process.TemplateIndex, HasProcess: true,
		ProcessStatus: process.Status, ProcessSignal: signal, StopCause: cause,
		Anchor: PresentationAnchor{Source: process.Owner, Target: process.LifecycleEntity, Position: &position, Direction: &direction},
	})
}

func (runtime *Runtime) emitProcessSignals(cast *castInstance, process *ProcessInstance, signals []ProcessSignal, revision WorldRevision) {
	for _, signal := range normalizeProcessSignals(signals) {
		view := *process
		if signal.Target != 0 {
			view.LifecycleEntity = signal.Target
		}
		runtime.emitProcessPresentation(cast, &view, PresentationProcessSignal, signal.Kind, "", revision)
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
