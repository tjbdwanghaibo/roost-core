package decision

import "errors"

func (st *compiledStage) initialActive() []bool {
	if len(st.Events) == 0 {
		return nil
	}
	active := make([]bool, len(st.Events))
	for i, l := range st.Events {
		active[i] = l.Enabled
	}
	return active
}

// NewState 创建独占的初始快照。每个有寿命上限的 emit 有一个计数槽，无上限 emit 不计数。
func (p *Program) NewState() State {
	if p == nil || p.plan == "" {
		return State{}
	}
	s := State{Plan: p.plan, Stage: p.start, LastTick: -1, Active: p.stages[p.start].initialActive()}
	if len(p.limited) > 0 {
		s.Counts = make([]int64, len(p.limited))
	}
	return s
}

// ValidateState 检查可达状态的必要不变量，不证明一个外部快照确实来自某段历史。
// 认证、版本单调和快照来源由宿主保证。不能把 plan 哈希当成签名。
func (p *Program) ValidateState(s State) error {
	if p == nil || p.plan == "" || s.Plan != p.plan {
		return errors.New("decision state belongs to a different plan")
	}
	if !validName(s.Stage) {
		return errors.New("invalid state stage")
	}
	st := p.stages[s.Stage]
	if st == nil || !st.reachable {
		return errors.New("unknown or unreachable state stage")
	}
	if s.Now < 0 || s.LastTick < -1 || s.LastTick > s.Now {
		return errors.New("invalid logical clock")
	}
	if len(s.Active) != len(st.Events) || len(s.Counts) != len(p.limited) || len(s.Pending) > p.delayed {
		return errors.New("invalid decision state dimensions")
	}
	for i, active := range s.Active {
		l := st.Events[i]
		if active && !l.Enabled && !st.canOpen[i] || !active && l.Enabled && !st.canClose[i] {
			return errors.New("unreachable listener flag")
		}
	}
	for i, n := range s.Counts {
		e := p.limited[i]
		if n < 0 || n > e.a.Limit {
			return errors.New("invalid lifetime count")
		}
		if n > 0 && (!e.stage.reachable || e.tick && s.LastTick < 0 || e.a.Delay > 0 && (s.LastTick < 0 || n > s.LastTick/e.a.Delay)) {
			return errors.New("count precedes a possible emission")
		}
	}
	ids := make(map[int64]bool, len(s.Pending))
	var lastScheduled int64
	for _, q := range s.Pending {
		e, ok := p.emits[q.NodeID]
		if !ok || !e.stage.reachable || e.a.Delay <= 0 || ids[q.NodeID] {
			return errors.New("unknown, immediate or duplicate pending emit")
		}
		ids[q.NodeID] = true
		// 先判下界再减，避免伪造期限产生减法溢出。Due 可早于 Now（事件不清队列），但不能早于/等于已跑完的 Tick。
		if q.Due < e.a.Delay || q.Due <= s.LastTick {
			return errors.New("impossible pending deadline")
		}
		scheduled := q.Due - e.a.Delay
		if scheduled < lastScheduled || scheduled > s.Now || e.tick && scheduled > s.LastTick {
			return errors.New("impossible pending scheduling time")
		}
		lastScheduled = scheduled
		if e.counter >= 0 {
			n := s.Counts[e.counter]
			if n >= e.a.Limit || n > scheduled/e.a.Delay {
				return errors.New("impossible count for pending emit")
			}
		}
	}
	return nil
}
