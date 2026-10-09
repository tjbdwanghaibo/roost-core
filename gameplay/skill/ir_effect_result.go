package skill

type effectResultIR struct {
	source           sourceRef
	local            *localSymbol
	success, failure flowIR
	layout           resultLayoutProgram
}

func (r *effectResultIR) walkValues(visitor valueVisitor) {
	walkOptionalFlowValues(r.success, visitor)
	walkOptionalFlowValues(r.failure, visitor)
}
