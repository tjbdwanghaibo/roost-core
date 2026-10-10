package entity

// EntityCategory identifies ownership/access category. It is encoded in the
// low 2 bits of EntityID.
type EntityCategory uint8

const (
	EntityCategoryNone EntityCategory = 0
)

// EntityKind identifies the concrete business entity definition. It is encoded
// in EntityID so any server can choose the right factory/loader from the ID.
type EntityKind uint8

const EntityKindNone EntityKind = 0

// EntityKindCategory declares the ownership category for one concrete entity
// kind. The relation is global because EntityID encodes both fields and every
// server must resolve the same kind to the same category.
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

// EntityKindDef declares all global ID-visible properties for one concrete
// entity kind. Every server should register the same definition before it
// builds, validates, or routes EntityIDs.
type EntityKindDef struct {
	BusinessPool BusinessPool
	Kind         EntityKind
	Category     EntityCategory
	RemotePolicy RemotePolicy
}

// ComponentType identifies component type within an entity.
type ComponentType uint16

// EntityDestroyReason describes why an entity is being destroyed.
type EntityDestroyReason uint8

const (
	// DestroyReasonCommon is the neutral default reason for infrastructure-level
	// entity removal. Business packages may alias it with domain-specific names.
	DestroyReasonCommon EntityDestroyReason = 0
	// DestroyReasonMemoryUnload 表示框架只把实例从本进程内存卸载、不删除持久数据：实例的内存状态
	// 已不可信（DataEngine 驱逐被跳过的原生步骤留下的实体，RR-20260926-30；Remote 事务被持久拒绝后
	// 内存仍留着被拒绝的修改，RR-20260926-39），下一次访问从权威重新加载。业务的 OnDestroy 可据此只回收
	// 内存资源，不当作业务删除；Sync 订阅不要注销，重载后框架 Rebind 强制全量。
	DestroyReasonMemoryUnload EntityDestroyReason = 255
	// DestroyReasonCreateRevoked 表示 Nest handler 内新建的实体随事务回滚 / 提交被明确拒绝而撤销发布（RR-20260926-35）：
	// 它从未进入已提交的持久化记录，也从未成为权威；框架在 Guard 释放后以此原因调用 DestroyAll / OnDestroy 并回收 ID。
	// 业务的 OnDestroy 可据此只回收内存资源，不当作业务删除（RR-20260927-12；之前用 DestroyReasonCommon，与业务 Destroy
	// 的缺省原因无法区分）。取值紧挨 DestroyReasonMemoryUnload，从高位往下分配框架原因，低位留给业务的领域别名。
	DestroyReasonCreateRevoked EntityDestroyReason = 254
)
