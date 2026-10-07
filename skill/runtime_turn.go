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
	// PayCosts 成功之后这里不能再失败：调用方按“付费失败 = 没有付”回滚（未提交的启动被删、ID 回收），付费之后的
	// 失败会让费用扣了不退。drainHostEvents 不返回错误（RR-20261006-55）。
	runtime.drainHostEvents(cast)
	return nil
}
