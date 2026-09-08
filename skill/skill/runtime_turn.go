package skill

func (runtime *Runtime) payCosts(cast *castInstance) error {
	return runtime.payCostList(cast, cast.program.costs)
}

func (runtime *Runtime) payCostList(cast *castInstance, costs []costProgram) error {
	if len(costs) == 0 {
		return nil
	}
	entries := make([]CostEntry, len(costs))
	for index, cost := range costs {
		value, err := runtime.evalValue(cast, cost.amount)
		if err != nil {
			return err
		}
		amount, ok := value.Int()
		if !ok || amount < 0 {
			return ErrRuntimeTypeMismatch
		}
		entries[index] = CostEntry{Handle: cost.resource, Amount: amount}
	}
	receipt, err := runtime.host.PayCosts(CostPayment{Meta: CommandMeta{RequiredRevision: cast.visibleRevision}, Entity: cast.caster, Entries: entries})
	if err != nil {
		return err
	}
	cast.visibleRevision = maxRevision(cast.visibleRevision, receipt.Revision)
	return runtime.drainHostEvents(cast)
}
