// Package state 是实体的状态位（对照 ssr 的 state 模块，按 roost 的模型重写）：一组各自
// 独立的“是否生效”事实（定身、缴械、无敌、手动操作中、回城中……），不是状态机，没有当前状态与迁移规则。
//
// 两种写入语义（取代 ssr 的覆盖 / 匿名计数 / 带序号计数 / 一次性四种）：
//   - 按来源（Add / Remove）：每个来源（Source：某个 Buff 实例、某个动作、GM……）各记一笔，有任一来源即生效；同一
//     来源重复 Add 不叠加，Remove 不存在的来源无效——不会出现匿名计数“多减一次就错位”。
//   - 派生（Provide）：查询时现算（例如“被定身” = Buff 容器里有 stun 标签），不存储；拥有数据的组件在数据变化后调
//     Changed 通知观察者。派生状态随数据回滚、加载后自动正确，不需要持久与恢复。
//
// 按来源的状态存在 StateDao（手写 DAO，形状同生成 DAO）：随 Nest 事务回滚，声明了 Sync 的变化标同步脏、
// 声明了 Persist 的落库，缺省两者都不。不内建时长：有时长的控制效果走 Buff，离线也要准点的长期标记走
// entitytimer，到期时 Remove。不做一次性事件（生命周期事件走实体回调或 AI 事件）、不做通用参数槽（需要参数的由来源
// 自带或业务组件自己存）。
//
// 状态对动作组的门控用 Gate（声明式：状态生效时计数冻结一个 actionflow 组，失效时解冻）。Set 的方法只能在实体锁内
// 调用（Nest handler、心跳回调、加载 / 新建的初始化阶段）。
package state

import (
	"errors"
	"fmt"
	"slices"
)

// ID 是状态编号，0 无效。编号在一个 Catalog 内唯一，由业务分配。
type ID uint16

// Def 声明一个状态。
type Def struct {
	ID ID
	// Name 是状态名，同时是持久文档里的键（改名等于丢弃旧数据，见 StateDao）。非空、在 Catalog 内唯一。
	Name string
	// Sync 为 true 时，这个状态的变化标实体同步脏（客户端可见，例如“被定身”）；业务的同步打包读 Set.Synced。
	Sync bool
	// Persist 为 true 时，按来源记下的这个状态随 StateDao 落库；只用于业务直接设置的长期标记（例如保护期），缺省
	// false。派生状态不能持久（它不存储）。
	Persist bool
}

// Source 是一个状态的来源：Kind 由业务定义（例如 1 = Buff、2 = 动作、3 = GM），ID 是来源在该种类里的编号（Buff 实例
// ID、动作 ID……）。
type Source struct {
	Kind uint8
	ID   int64
}

func (s Source) less(o Source) bool {
	if s.Kind != o.Kind {
		return s.Kind < o.Kind
	}
	return s.ID < o.ID
}

var (
	// ErrCatalogInvalid：状态声明有误（编号为 0、名字为空、编号或名字重复）。
	ErrCatalogInvalid = errors.New("state: invalid catalog")
	// ErrUnknownState：状态编号不在 Catalog 里。
	ErrUnknownState = errors.New("state: unknown state")
	// ErrNotInTransaction：在 Nest 事务外改声明了 Persist 的状态（改动无法落库，拒绝而不是静默丢失）。
	ErrNotInTransaction = errors.New("state: persistent state changed outside a Nest transaction")
	// ErrRollbackNotCaptured：在 rollback=state 的 handler 里改状态，而这个事务没有对 StateDao 拍快照（DAO 没登记进
	// 实体的 DaoManager / RangeDao）。回滚恢复不了这次改动，所以拒绝、什么都不改；接线错误，修法是把 DAO 登记进去，
	// 或让改状态的 handler 用 rollback=undo。
	ErrRollbackNotCaptured = errors.New("state: rollback=state transaction did not capture this StateDao (register it in the entity's DaoManager)")
	// ErrProviderInvalid：派生状态的提供函数有误（nil、重复登记、状态声明了 Persist）。
	ErrProviderInvalid = errors.New("state: invalid provider")
	// ErrNotifyRunaway：观察者互相触发，一次最外层调用里的通知轮数超过上限。
	ErrNotifyRunaway = errors.New("state: watchers kept changing states")
)

// Catalog 是状态声明表，启动时构造一次，之后只读（多个实体共用一个）。
type Catalog struct {
	defs   map[ID]Def
	byName map[string]ID
	order  []ID
}

// NewCatalog 校验并封存声明：编号非 0、名字非空，编号与名字都不重复。
func NewCatalog(defs ...Def) (*Catalog, error) {
	c := &Catalog{defs: make(map[ID]Def, len(defs)), byName: make(map[string]ID, len(defs))}
	for _, def := range defs {
		switch {
		case def.ID == 0:
			return nil, fmt.Errorf("%w: state %q has id 0", ErrCatalogInvalid, def.Name)
		case def.Name == "":
			return nil, fmt.Errorf("%w: state %d has no name", ErrCatalogInvalid, def.ID)
		}
		if _, dup := c.defs[def.ID]; dup {
			return nil, fmt.Errorf("%w: duplicate id %d", ErrCatalogInvalid, def.ID)
		}
		if _, dup := c.byName[def.Name]; dup {
			return nil, fmt.Errorf("%w: duplicate name %q", ErrCatalogInvalid, def.Name)
		}
		c.defs[def.ID] = def
		c.byName[def.Name] = def.ID
		c.order = append(c.order, def.ID)
	}
	slices.Sort(c.order)
	return c, nil
}

// Def 返回状态的声明。
func (c *Catalog) Def(id ID) (Def, bool) {
	def, ok := c.defs[id]
	return def, ok
}

// Lookup 按名字查编号。
func (c *Catalog) Lookup(name string) (ID, bool) {
	id, ok := c.byName[name]
	return id, ok
}

// IDs 按编号升序返回全部状态。
func (c *Catalog) IDs() []ID { return slices.Clone(c.order) }
