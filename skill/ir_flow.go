package skill

type flowIR interface {
	isFlowIR()
	sourceRef() sourceRef
	walkValues(valueVisitor)
}

type sequenceFlowIR struct {
	source sourceRef
	steps  []flowIR
}

func (*sequenceFlowIR) isFlowIR()              {}
func (f *sequenceFlowIR) sourceRef() sourceRef { return f.source }
func (f *sequenceFlowIR) walkValues(visitor valueVisitor) {
	for _, child := range f.steps {
		child.walkValues(visitor)
	}
}

type parallelFlowIR struct {
	source   sourceRef
	branches []flowIR
}

func (*parallelFlowIR) isFlowIR()              {}
func (f *parallelFlowIR) sourceRef() sourceRef { return f.source }
func (f *parallelFlowIR) walkValues(visitor valueVisitor) {
	for _, child := range f.branches {
		child.walkValues(visitor)
	}
}

type ifFlowIR struct {
	source             sourceRef
	condition          valueIR
	thenFlow, elseFlow flowIR
}

func (*ifFlowIR) isFlowIR()              {}
func (f *ifFlowIR) sourceRef() sourceRef { return f.source }
func (f *ifFlowIR) walkValues(visitor valueVisitor) {
	walkValue(f.condition, visitor)
	walkOptionalFlowValues(f.thenFlow, visitor)
	walkOptionalFlowValues(f.elseFlow, visitor)
}

type repeatFlowIR struct {
	source        sourceRef
	times         valueIR
	intervalTicks Tick
	index         localSymbol
	body          flowIR
}

func (*repeatFlowIR) isFlowIR()              {}
func (f *repeatFlowIR) sourceRef() sourceRef { return f.source }
func (f *repeatFlowIR) walkValues(visitor valueVisitor) {
	walkValue(f.times, visitor)
	walkOptionalFlowValues(f.body, visitor)
}

type waitFlowIR struct {
	source sourceRef
	ticks  Tick
	then   flowIR
}

func (*waitFlowIR) isFlowIR()                         {}
func (f *waitFlowIR) sourceRef() sourceRef            { return f.source }
func (f *waitFlowIR) walkValues(visitor valueVisitor) { walkOptionalFlowValues(f.then, visitor) }

type selectFlowIR struct {
	source     sourceRef
	selectPlan selectIR
	consume    selectConsumeIR
	onEmpty    flowIR
}

func (*selectFlowIR) isFlowIR()              {}
func (f *selectFlowIR) sourceRef() sourceRef { return f.source }
func (f *selectFlowIR) walkValues(visitor valueVisitor) {
	f.selectPlan.walkValues(visitor)
	f.consume.walkValues(visitor)
	walkOptionalFlowValues(f.onEmpty, visitor)
}

type effectFlowIR struct {
	source          sourceRef
	effect          effectIR
	result          *effectResultIR
	resultLayout    resultLayoutProgram
	hasResultLayout bool
	callbacks       *processCallbacksIR
	process         *processIR
}

func (*effectFlowIR) isFlowIR()              {}
func (f *effectFlowIR) sourceRef() sourceRef { return f.source }
func (f *effectFlowIR) walkValues(visitor valueVisitor) {
	f.effect.walkValues(visitor)
	if f.result != nil {
		f.result.walkValues(visitor)
	}
	if f.callbacks != nil {
		f.callbacks.walkValues(visitor)
	}
	if f.process != nil {
		f.process.walkValues(visitor)
	}
}

type gotoFlowIR struct {
	source sourceRef
	phase  string
}

func (*gotoFlowIR) isFlowIR()               {}
func (f *gotoFlowIR) sourceRef() sourceRef  { return f.source }
func (*gotoFlowIR) walkValues(valueVisitor) {}

type finishFlowIR struct {
	source sourceRef
	reason string
}

func (*finishFlowIR) isFlowIR()               {}
func (f *finishFlowIR) sourceRef() sourceRef  { return f.source }
func (*finishFlowIR) walkValues(valueVisitor) {}

type processCallbacksIR struct{ tick, hit, collision, end, cancel, transition, targetLost, enter, leave flowIR }

func (p *processCallbacksIR) walkValues(visitor valueVisitor) {
	for _, flow := range []flowIR{p.tick, p.hit, p.collision, p.end, p.cancel, p.transition, p.targetLost, p.enter, p.leave} {
		walkOptionalFlowValues(flow, visitor)
	}
}

func walkOptionalFlowValues(flow flowIR, visitor valueVisitor) {
	if flow != nil {
		flow.walkValues(visitor)
	}
}
