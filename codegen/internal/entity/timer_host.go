package entity

import (
	"fmt"
	"path/filepath"
)

// TimerHost 是 roost add timer 需要知道的实体事实（D11）：能不能装持久定时器、节点放进哪个 DAO。
type TimerHost struct {
	// Daos 是实体的 DAO 字段（类型名不带包名，如 PlayerDao；TypePkg 为外部包路径，本包 / db 别名时为空或别名路径）。
	Daos []DaoField
	// Components 是已有组件的 CompType 常量名，用来拒绝重名。
	Components []string
	// ComponentFields 是已有组件字段（类型名与 CompType）。
	ComponentFields []ComponentField
}

// InspectTimerHost 读 dir 包里名为 name 的实体，拒绝不能装持久定时器的实体：
//   - remote=managed：实例的所有权在进程间迁移，驱动的期限索引只覆盖本进程加载的实例，迁移中的到期语义没有定义；
//   - noPersist=true：节点随实体 DAO 持久，不落库的实体重启后没有期限可以补触发。
func InspectTimerHost(dir, name string) (TimerHost, error) {
	defs, _, err := parsePackage(dir)
	if err != nil {
		return TimerHost{}, err
	}
	for _, ent := range defs.Entities {
		if ent.Name != name {
			continue
		}
		source := filepath.ToSlash(ent.SourceFile)
		if ent.RemotePolicy == "entity.RemotePolicyManaged" {
			return TimerHost{}, fmt.Errorf("%s: %s is remote=managed; persistent entity timers are not supported on Remote-managed entities: the entity moves between processes, the timer driver only indexes instances loaded in this process, and what a deadline means while ownership moves is not defined. Keep the deadline on a local entity, or schedule it through a saga", source, ent.Name)
		}
		if ent.NoPersist {
			return TimerHost{}, fmt.Errorf("%s: %s is noPersist=true; persistent entity timers store their nodes in the entity's DAO, and an entity that is never stored has nothing to fire after a restart. Use an in-memory timer (infra/base/timer closure timers) for it", source, ent.Name)
		}
		host := TimerHost{Daos: append([]DaoField(nil), ent.Daos...)}
		for _, comp := range ent.Components {
			host.Components = append(host.Components, comp.CompType)
		}
		host.ComponentFields = append([]ComponentField(nil), ent.Components...)
		return host, nil
	}
	return TimerHost{}, fmt.Errorf("%s: no //roost:entity struct named %s", filepath.ToSlash(dir), name)
}
