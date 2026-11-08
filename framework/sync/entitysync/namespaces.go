package entitysync

import (
	"fmt"
	"math"
	"slices"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
)

// NamespaceLimits 是一个同步命名空间单独的容量与快照预算（ManagerConfig.Namespaces）。
//
// 按 subject 的 Namespace（codegen 的 syncNamespace）分类，而不是按订阅来源：命名空间是对象自己的属性，
// 一个对象经几个来源订阅也只占一个会话引用、只属于一类；它也是客户端的分流键，配置与线上看到的是同一个名字。
// 类别在 subject 登记时按当时状态的命名空间确定，Rebind 不改变。
//
// 用法例：场景里玩家 / 怪沿用 Limits.MaxObjects（100）且不限快照，地形单独 64 个、每会话每次 Flush 至多 4 个全量——
// 两类互不占额度，玩家 / 怪的交付与没有地形时相同。
type NamespaceLimits struct {
	// MaxObjects 是一个会话可同时持有的该命名空间对象数，必须为正。超出时与 Limits.MaxObjects 相同：
	// 不截断、不只拒超出的那几个，在为新对象分配引用时关闭整个会话（SessionLost，原因 object_limit，
	// 错误里点名命名空间）。持有量由政策的上限保证（例如该命名空间所用 Interest 的 AOIConfig.MaxVisible）。
	MaxObjects int
	// SnapshotBudget 是该命名空间单独的冷创建预算：与 Manager 级 SnapshotBudget、其他命名空间互不占额度，
	// 各自轮转会话与实体、各自计字节软上限。零值表示该命名空间不限快照（不继承 Manager 级预算）。
	SnapshotBudget SnapshotBudget
}

// syncClasses 是由 ManagerConfig.Namespaces 得出的类别：类别 0 是没有列出的命名空间，
// 1..n 依次是按名字排序的列出项。类别同时决定会话持有上限（wireConfig.classLimits）与快照预算道（Manager.lanes）。
type syncClasses struct {
	byNamespace map[string]uint8 // 没有配置 Namespaces 时为 nil，登记不读命名空间
	names       []string         // 按类别，诊断用；类别 0 为空
	limits      []int            // 按类别的会话持有上限；没有配置时为 nil
	budgets     []SnapshotBudget // 按类别的快照预算；类别 0 是 Manager 级 SnapshotBudget
	total       int              // 会话引用表上限：各类之和
}

// newSyncClasses 校验并展开 Namespaces；base 是已补默认值的 Limits.MaxObjects。
func newSyncClasses(namespaces map[string]NamespaceLimits, base int, budget SnapshotBudget) (syncClasses, error) {
	classes := syncClasses{total: base, budgets: []SnapshotBudget{budget}}
	if len(namespaces) == 0 {
		return classes, nil
	}
	if len(namespaces) > math.MaxUint8 {
		return syncClasses{}, fmt.Errorf("entitysync: %d namespaces configured, at most %d", len(namespaces), math.MaxUint8)
	}
	names := make([]string, 0, len(namespaces))
	for name := range namespaces {
		names = append(names, name)
	}
	slices.Sort(names)
	classes.byNamespace = make(map[string]uint8, len(names))
	classes.names = append([]string{""}, names...)
	classes.limits = append(make([]int, 0, len(names)+1), base)
	for i, name := range names {
		limits := namespaces[name]
		if name == "" {
			return syncClasses{}, fmt.Errorf("entitysync: namespace limits need a namespace name")
		}
		if limits.MaxObjects <= 0 {
			return syncClasses{}, fmt.Errorf("entitysync: namespace %q needs a positive MaxObjects", name)
		}
		if b := limits.SnapshotBudget; b.MaxObjects < 0 || b.MaxBytes < 0 || b.PerSessionObjects < 0 {
			return syncClasses{}, fmt.Errorf("entitysync: negative snapshot budget for namespace %q", name)
		}
		classes.byNamespace[name] = uint8(i + 1)
		classes.limits = append(classes.limits, limits.MaxObjects)
		classes.budgets = append(classes.budgets, limits.SnapshotBudget)
		classes.total += limits.MaxObjects
	}
	// 引用 ID 是 uint16，0 无效。
	if classes.total > math.MaxUint16 {
		return syncClasses{}, fmt.Errorf("entitysync: a session may hold %d objects in all, more than the %d object references a stream has", classes.total, math.MaxUint16)
	}
	return classes, nil
}

// classOf 是一个命名空间的类别；没有配置 Namespaces 时恒为 0，不读状态锁。
func (m *Manager) classOf(state *entity.SubjectSyncState) uint8 {
	if m.classes.byNamespace == nil {
		return 0
	}
	return m.classes.byNamespace[state.Namespace()]
}

// admitClass 在为新对象分配引用之前检查该类的持有上限；没有配置 Namespaces 时只有 allocate 的总数检查（与以前相同）。
func (s *session) admitClass(class uint8, cfg wireConfig) error {
	if cfg.classLimits == nil || s.classHeld == nil {
		return nil
	}
	if limit := cfg.classLimits[class]; (*s.classHeld)[class] >= limit {
		if class == 0 {
			return fmt.Errorf("%w: session %d holds %d objects of the namespaces Namespaces does not list (Limits.MaxObjects)", frame.ErrObjectLimit, s.id, limit)
		}
		return fmt.Errorf("%w: session %d holds %d objects of namespace %q", frame.ErrObjectLimit, s.id, limit, cfg.classNames[class])
	}
	return nil
}

// countClass 在 allocate / release 之后（引用表已归本副本）调整该类的持有数。
func (s *session) countClass(class uint8, cfg wireConfig, delta int) {
	if cfg.classLimits == nil {
		return
	}
	if s.classHeld == nil {
		held := make([]int, len(cfg.classLimits))
		s.classHeld = &held
	}
	(*s.classHeld)[class] += delta
}
