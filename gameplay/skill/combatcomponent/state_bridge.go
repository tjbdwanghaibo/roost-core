package combatcomponent

import (
	"errors"
	"fmt"
	"slices"

	"github.com/tjbdwanghaibo/roost-core/gameplay/skill/combat"
	"github.com/tjbdwanghaibo/roost-core/gameplay/state"
)

// StateBinding 把一组 buff 标签接成一个派生状态：任一生效中的 buff 带其中一个标签，状态即生效（例如 "stun"、"freeze"
// → 被定身）。
type StateBinding struct {
	State state.ID
	Tags  []combat.Tag
}

// BindStates 把 buff 标签接到实体的状态组件上（DESIGN-TODO D21 S6；维护者 2026-10-21 决定 12：派生，不复制）。每个
// 绑定在 set 上登记一个派生状态（state.Set.Provide），查询时读 buff 容器的标签——Buff 是控制效果的唯一持有者，
// 时长、叠层、驱散、免疫都在容器里，state 不另存一份，buff 随事务回滚、随实体加载，状态自然跟着对。之后组件在每个
// 改 buff 集合的 mutator（ApplyBuff、RemoveBuff、SetBuffStacks、AdoptBuff、DispelBuffs、TickBuffs）末尾、同一事务里
// 调 set.Changed，让观察者（门控、AI）与同步位跟上；OnInitFinish（加载 / 新建完成）也调一次。
//
// 在实体工厂里构造组件后调用一次（与 ProjectAttributes 一样在事务外安装）。状态必须已在 set 的 Catalog 里声明、不能
// 声明 Persist，也不能已经有提供函数。
func (component *CombatComponent) BindStates(set *state.Set, bindings ...StateBinding) error {
	if set == nil {
		return errors.New("combatcomponent: bind states needs a state set")
	}
	ids := make([]state.ID, 0, len(bindings))
	for _, binding := range bindings {
		if len(binding.Tags) == 0 {
			return fmt.Errorf("combatcomponent: state %d is bound to no buff tag", binding.State)
		}
		tags := slices.Clone(binding.Tags)
		if err := set.Provide(binding.State, func() bool { return component.hasAnyBuffTag(tags) }); err != nil {
			return err
		}
		ids = append(ids, binding.State)
	}
	component.buffStatesChanged = func() { set.Changed(ids...) }
	return nil
}

// HasBuffTag 报告是否有生效中的 buff 带该标签（例如接到 StatusBridge.HasGameplayTag，或业务自己的派生状态）。
func (component *CombatComponent) HasBuffTag(tag combat.Tag) bool {
	return component.dao.buffs.HasTag(tag)
}

// hasAnyBuffTag 每次都经 component.dao 读当前容器：回滚会换掉容器实例（beginChange 的逆操作），不能缓存它。
func (component *CombatComponent) hasAnyBuffTag(tags []combat.Tag) bool {
	return slices.ContainsFunc(tags, component.dao.buffs.HasTag)
}

// buffsChanged 在 buff 集合变化后通知绑定的派生状态（没有绑定时什么都不做），并对齐 buff 心跳（AttachBeat，D20 P5）。
func (component *CombatComponent) buffsChanged() {
	if component.buffStatesChanged != nil {
		component.buffStatesChanged()
	}
	component.alignBeat()
}
