package entity

// EntityCategory 是实体的归属 / 访问分类，取值即锁序等级（见 category_order.go）。分类以 kind 注册表为准；EntityID
// 低两位只保存截断后的值，供本进程未注册该 kind 时兜底。
type EntityCategory uint8

const (
	EntityCategoryNone EntityCategory = 0
)

// EntityKind 标识具体的业务实体定义。它编码在 EntityID 中，任何服务都能据 ID 选出正确的工厂 / 加载器。
type EntityKind uint8

const EntityKindNone EntityKind = 0

// EntityKindCategory 声明一个具体实体 kind 的分类。这个关系是全局的：每个服务都必须把同一 kind 解析为同一分类。
type EntityKindCategory struct {
	Kind     EntityKind
	Category EntityCategory
}

// BusinessPool 决定业务执行资源；任一目标为 Long，整条消息走长业务池。
type BusinessPool uint8

const (
	BusinessPoolDefault BusinessPool = iota
	BusinessPoolShort
	BusinessPoolLong
)

// EntityKindDef 声明一个具体实体 kind 的全部全局可见属性。每个服务都应在构建、校验或路由 EntityID 之前登记同样的定义。
type EntityKindDef struct {
	BusinessPool BusinessPool
	Kind         EntityKind
	Category     EntityCategory
	RemotePolicy RemotePolicy
}

// ComponentType 标识实体内的组件类型。
type ComponentType uint16

// EntityDestroyReason 描述实体被销毁的原因。
type EntityDestroyReason uint8

const (
	// DestroyReasonCommon 是基础设施层移除实体的中性缺省原因，业务包可以给它起领域别名。
	DestroyReasonCommon EntityDestroyReason = 0
	// DestroyReasonMemoryUnload 表示框架只把实例从本进程内存卸载、不删除持久数据：实例的内存状态
	// 已不可信（DataEngine 驱逐被跳过的原生步骤留下的实体；Remote 事务被持久拒绝后
	// 内存仍留着被拒绝的修改），下一次访问从权威重新加载。业务的 OnDestroy 可据此只回收
	// 内存资源，不当作业务删除；Sync 订阅不要注销，重载后框架 Rebind 强制全量。
	DestroyReasonMemoryUnload EntityDestroyReason = 255
	// DestroyReasonCreateRevoked 表示 Nest handler 内新建的实体随事务回滚 / 提交被明确拒绝而撤销发布：
	// 它从未进入已提交的持久化记录，也从未成为权威；框架在 Guard 释放后以此原因调用 DestroyAll / OnDestroy 并回收 ID。
	// 业务的 OnDestroy 可据此只回收内存资源，不当作业务删除（不能用 DestroyReasonCommon，那与业务 Destroy 的缺省原因
	// 无法区分）。取值紧挨 DestroyReasonMemoryUnload，从高位往下分配框架原因，低位留给业务的领域别名。
	DestroyReasonCreateRevoked EntityDestroyReason = 254
)
