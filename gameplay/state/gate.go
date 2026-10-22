package state

import (
	"errors"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/gameplay/actionflow"
)

// Freezer 是 Gate 要驱动的冻结入口；actionflow.Behavior（ActionList）实现它。
type Freezer interface {
	Freeze(group actionflow.ActionGroup, mode actionflow.FreezeMode) error
	Recover(group actionflow.ActionGroup) error
}

// Rule 是一条门控规则：State 生效时按 Mode 冻结 Group，失效时撤销这次冻结（对照 ssr“定身冻结移动组、缴械
// 冻结战斗组”）。
type Rule struct {
	State ID
	Group actionflow.ActionGroup
	Mode  actionflow.FreezeMode
}

// Gated 是装好的门控，Close 卸下。
type Gated struct {
	set     *Set
	target  Freezer
	rules   []Rule
	watcher *Watcher
	// applied[i] 表示第 i 条规则此刻在 target 上持有一次冻结。它跟随观察者看到的值，不随事务回滚：回滚后与状态不一致
	// 的部分在下一次通知（任何状态改动或 Set.Refresh）时按差纠正，每条规则至多持有一次冻结，不会累积。
	applied []bool //roost:cache
	onError func(error)
}

// Gate 按规则把状态接到动作组的计数冻结上（门控是声明式的：能用它表达的不要写观察者）。装上时按当前状态对齐一次：
// 已经生效的状态立即冻结对应的组。之后状态生效时 Freeze(Group, Mode)——FreezeCancel 在这一刻取消组的在途动作——
// 失效时 Recover。冻结是计数的，多条规则、多个来源冻结同一组，全部失效才解冻。onError 接收 Freeze / Recover 的错误，
// 可空。
//
// 事务：状态随事务回滚，冻结计数与被打断的动作不回滚（actionflow 的 runner 不随事务回滚）。handler 里
// 先加了一个门控状态、随后失败回滚时，组仍处于冻结，直到这个 Set 下一次通知（任何状态改动或 Refresh）把它解开；
// 需要立即一致就在回滚后调 Set.Refresh。
func Gate(set *Set, target Freezer, onError func(error), rules ...Rule) (*Gated, error) {
	if set == nil || target == nil {
		return nil, errors.New("state: gate needs a state set and a freezer")
	}
	g := &Gated{set: set, target: target, rules: append([]Rule(nil), rules...), applied: make([]bool, len(rules)), onError: onError}
	g.watcher = set.NewWatcher()
	watched := make(map[ID]bool)
	for _, rule := range rules {
		if _, ok := set.catalog.Def(rule.State); !ok {
			g.watcher.Close()
			return nil, fmt.Errorf("%w: gate rule on %d", ErrUnknownState, rule.State)
		}
		if watched[rule.State] {
			continue
		}
		watched[rule.State] = true
		if err := g.watcher.Watch(rule.State, func(Change) { g.align() }); err != nil {
			g.watcher.Close()
			return nil, err
		}
	}
	g.align()
	return g, nil
}

// Close 卸下门控：撤销观察，并撤销本门控持有的全部冻结。
func (g *Gated) Close() {
	if g.watcher == nil {
		return
	}
	g.watcher.Close()
	g.watcher = nil
	for i, rule := range g.rules {
		if g.applied[i] {
			g.applied[i] = false
			g.report(g.target.Recover(rule.Group))
		}
	}
}

// align 让每条规则持有的冻结与状态当前值一致。
func (g *Gated) align() {
	for i, rule := range g.rules {
		want := g.set.Has(rule.State)
		switch {
		case want && !g.applied[i]:
			g.applied[i] = true
			g.report(g.target.Freeze(rule.Group, rule.Mode))
		case !want && g.applied[i]:
			g.applied[i] = false
			g.report(g.target.Recover(rule.Group))
		}
	}
}

func (g *Gated) report(err error) {
	if err != nil && g.onError != nil {
		defer func() { _ = recover() }()
		g.onError(err)
	}
}
