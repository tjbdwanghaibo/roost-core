package skill

type PassiveActivationID uint64
type AbilityHandle uint64

type PassiveCandidate struct {
	Program *Program
	Owner   EntityID
	Ability AbilityHandle
}

type PassiveRouter interface {
	Candidates(event EventContext) []PassiveCandidate
}

type procLedgerKey struct {
	Root   EventID
	Caster EntityID
	Digest string
}

func (runtime *Runtime) ActivatePassive(program *Program, event EventContext) (PassiveActivationID, error) {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	root := event.RootEventID
	if root == 0 {
		root = event.EventID
	}
	if root == 0 {
		return 0, ErrCastInputInvalid
	}
	if err := runtime.trackRootEventLocked(root); err != nil {
		return 0, err
	}
	owner := event.Owner
	if owner == 0 {
		owner = event.Source
	}
	return runtime.enqueuePassive(program, event, owner, 0)
}

func (runtime *Runtime) enqueuePassive(program *Program, event EventContext, owner EntityID, ability AbilityHandle) (PassiveActivationID, error) {
	if program == nil || program.activationKind == "active" || owner == 0 {
		return 0, ErrCastInputInvalid
	}
	runtime.nextPassiveActivationID++
	id := runtime.nextPassiveActivationID
	due := runtime.currentTick
	if event.Tick > due {
		due = event.Tick
	}
	if err := runtime.scheduleSystem(due, &passiveActivationTask{ID: id, Program: program, Event: cloneEventContext(event), Owner: owner, Ability: ability}); err != nil {
		return 0, err
	}
	return id, nil
}

func (runtime *Runtime) executePassiveActivation(task *passiveActivationTask) error {
	program := task.Program
	if program == nil || len(program.eventPlans) == 0 {
		return nil
	}
	plan := program.eventPlans[0]
	if !eventMatchesFilter(task.Event, plan.filter) {
		runtime.emitPassiveSuppressed(task, "filter")
		return nil
	}
	root := task.Event.RootEventID
	if root == 0 {
		root = task.Event.EventID
	}
	if task.Event.ProcDepth >= plan.proc.MaxDepth {
		runtime.emitPassiveSuppressed(task, "max_depth")
		return nil
	}
	if !plan.proc.AllowSelfTrigger && task.Event.SkillID == program.id {
		runtime.emitPassiveSuppressed(task, "self_trigger")
		return nil
	}
	ledgerKey := procLedgerKey{Root: root, Caster: task.Owner, Digest: program.identity.gameplayDigest}
	if plan.proc.OncePerRootEvent {
		if _, exists := runtime.procLedger[ledgerKey]; exists {
			runtime.emitPassiveSuppressed(task, "once_per_root")
			return nil
		}
	}
	if runtime.rootEventCounts[root] > plan.proc.MaxEventsPerRoot {
		runtime.emitPassiveSuppressed(task, "max_events_per_root")
		return nil
	}
	if runtime.passiveCountTick != runtime.currentTick {
		runtime.passiveCountTick, runtime.passiveCount = runtime.currentTick, 0
	}
	if runtime.passiveCount >= runtime.options.MaxPassiveActivationsPerTick {
		runtime.emitPassiveSuppressed(task, "max_activations_per_tick")
		return nil
	}
	if plan.proc.OncePerRootEvent && len(runtime.procLedger) >= runtime.options.MaxProcLedgerEntries {
		for len(runtime.procLedger) >= runtime.options.MaxProcLedgerEntries && runtime.evictInactiveRootLocked(root) {
		}
		if len(runtime.procLedger) >= runtime.options.MaxProcLedgerEntries {
			runtime.emitPassiveSuppressed(task, "capacity")
			return nil
		}
	}
	input := passiveCastInput(program, task.Owner, task.Event)
	castID, err := runtime.startLocked(program, input, &task.Event)
	if err != nil {
		if err == ErrCooldownActive || err == ErrCastInputInvalid || err == ErrCastInputRejected {
			runtime.emitPassiveSuppressed(task, "unavailable")
			return nil
		}
		return err
	}
	runtime.passiveCount++
	if plan.proc.OncePerRootEvent {
		runtime.procLedger[ledgerKey] = struct{}{}
	}
	runtime.appendRuntimeEvent(RuntimeEvent{Tick: runtime.currentTick, Kind: "passive_activated", Entity: task.Owner, Context: EventContext{RootEventID: root, ParentEventID: task.Event.EventID, Owner: task.Owner, SkillID: program.id, CastID: castID, ProcDepth: task.Event.ProcDepth + 1}})
	return nil
}

func passiveCastInput(program *Program, owner EntityID, event EventContext) CastInput {
	input := CastInput{Caster: owner}
	for _, slot := range program.input.slots {
		switch slot.name {
		case "$input.target":
			input.Target = event.Target
		}
	}
	return input
}

func eventMatchesFilter(event EventContext, filter FilterPlan) bool {
	tags := event.GameplayTags()
	for _, required := range filter.RequiredTags {
		if !containsGameplayTag(tags, required) {
			return false
		}
	}
	for _, excluded := range filter.ExcludedTags {
		if containsGameplayTag(tags, excluded) {
			return false
		}
	}
	if len(filter.Elements) > 0 && !containsElement(filter.Elements, event.Element) {
		return false
	}
	if len(filter.DamageTypes) > 0 && !containsDamageType(filter.DamageTypes, event.DamageType) {
		return false
	}
	if len(filter.Results) > 0 && !containsString(filter.Results, event.Result) {
		return false
	}
	return true
}

func containsGameplayTag(values []GameplayTagHandle, wanted GameplayTagHandle) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func containsElement(values []ElementHandle, wanted ElementHandle) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func containsDamageType(values []DamageTypeHandle, wanted DamageTypeHandle) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func (runtime *Runtime) emitPassiveSuppressed(task *passiveActivationTask, reason string) {
	runtime.appendRuntimeEvent(RuntimeEvent{Tick: runtime.currentTick, Kind: "passive_suppressed", Entity: task.Owner, Context: EventContext{RootEventID: task.Event.RootEventID, ParentEventID: task.Event.EventID, Owner: task.Owner, SkillID: task.Program.id, Result: reason}})
}
