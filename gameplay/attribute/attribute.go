// Package attribute 是属性系统里框架负责的一半：标识、元数据、profile 契约，以及持有主体各层属性的容器。另一半由
// roost-codegen 的属性生成器按项目从 `//roost:attribute` 标记生成——它产出一个 profile 的 ID、掩码、元数据、
// 带类型的 setter、派生公式与带类型的访问器，并实现 Profile。
//
// 这样分开是有意的：每个游戏都以同样方式需要的东西（属性如何命名与寻址、快照意味着什么、如何选择层）放在这里，
// 可以跨项目依赖；游戏特有的东西（有哪些属性、攻击力如何派生出战力）从游戏自己的声明生成。
package attribute

// AttrID 标识 profile 里的一个属性。ID 由生成器从 profile 声明的下标范围分配，只要声明不变就稳定：可以持久化、
// 上线路、作 map 的键。
type AttrID uint16

// AttrValue 是每个属性传递时共用的数值形式。profile 可以用更窄的类型存储属性，由生成的 setter 转换。线路与批量
// 传输只用一种类型，框架才能搬运它一无所知的属性。
type AttrValue int64

// Meta 描述一个属性：声明里的名字（`attr:"hp"`）与 Go 里的名字（`HP`）、它对应的脏位，以及是否派生——派生属性
// 由公式计算，不能直接设置，所以生成的 SetDirectAttr 会拒绝它。
type Meta struct {
	ID      AttrID
	Name    string
	Field   string
	Mask    uint64
	Derived bool
}

// Profile 是生成的属性 profile 实现的接口。游戏持有 profile，框架搬运它们。
//
// 脏掩码是契约的核心：每个 setter 把自己属性的位 OR 进去，Update 重算输入被置脏的派生属性，变化发布之后
// ClearDirty 清掉它。生成代码在 profile 结构体上维护一个未导出的 `dirtyMask uint64` 字段——这是声明本身必须
// 携带的唯一字段。
type Profile interface {
	// Reset 把 profile 恢复为零值，包括脏掩码。
	Reset()
	// CloneProfile 是深拷贝：快照交出的是它，读者无法修改活的 profile。
	CloneProfile() Profile
	// DirtyMask 是自上次 ClearDirty 以来改变过的属性的位 OR。
	DirtyMask() uint64
	ClearDirty()
	// SetAttr 设置任意属性（包括派生属性），生成代码在恢复时用它。返回值是否改变。
	SetAttr(id AttrID, value AttrValue) bool
	// SetDirectAttr 只设置非派生属性，是玩法的入口：派生属性归公式所有。
	SetDirectAttr(id AttrID, value AttrValue) bool
	GetAttr(id AttrID) (AttrValue, bool)
	// LoadValues 应用一个批量 map 并运行 Update，返回这次调用置上的位（不含加载前已有的掩码，调用方知道的是
	// 自己改了什么）。
	LoadValues(values map[AttrID]AttrValue) uint64
	// ExportValues 把 profile 写进 dst，并删除属性为零的项，让 map 保持线路需要的稀疏形式。
	ExportValues(dst map[AttrID]AttrValue)
	// Update 重算输入已脏的每个派生属性，报告其中是否有变化。
	Update() bool
	MetaByID(id AttrID) (Meta, bool)
	MetaByName(name string) (Meta, bool)
}
