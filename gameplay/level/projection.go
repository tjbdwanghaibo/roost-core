package level

// View 只投影公开状态，不包含 source 水位、隐藏变量、动作参数或回执账本。
// 私人资产/参与者信息由业务在认证后组合，不能把 Snapshot 直接发给客户端。
type View struct {
	Instance        string
	Epoch, Revision uint64
	Phase           Phase
	Result          string
	Stages          []StageView
	Variables       map[string]Value
}
type StageView struct {
	ID         string
	Generation uint64
}

// Revision 返回状态版本：每次成功的变更加一，没有变更的调用（未到期的 Advance、重复的事实或回执）不变。
// 宿主用它判断这一次提交之后要不要重新取 PendingActions 与 Project。只读一个字段，不复制状态。
func (c *LevelComponent) Revision() uint64 { return c.dao.state.Revision }

// Phase 返回当前阶段；尚未 Initialize 时为空。只读一个字段，不复制状态。
func (c *LevelComponent) Phase() Phase { return c.dao.state.Phase }

func (c *LevelComponent) Project() View {
	s := c.dao.state
	out := View{Instance: s.Instance, Epoch: s.Epoch, Revision: s.Revision, Phase: s.Phase, Result: s.Result, Variables: map[string]Value{}}
	for _, name := range sortedKeys(s.Active) {
		out.Stages = append(out.Stages, StageView{name, s.Active[name].Generation})
	}
	for _, v := range c.program.definition.Variables {
		if !v.Public {
			continue
		}
		put := func(stage, trigger string) {
			key := storedKey(v.Scope, v.Name, stage, trigger)
			value, ok := s.Variables[key]
			if !ok {
				value = v.Default
			}
			out.Variables[key] = value
		}
		switch v.Scope {
		case Global:
			put("", "")
		case Stage:
			for name := range s.Active {
				put(name, "")
			}
		case Trigger:
			for name := range s.Active {
				for _, t := range c.program.stages[name].Triggers {
					put(name, t.ID)
				}
			}
		}
	}
	return out
}
