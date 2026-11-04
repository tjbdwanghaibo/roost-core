package app

// ModName identifies an infrastructure module.
type ModName string

// ServiceName identifies a business service.
type ServiceName string

const (
	ModHealth         ModName = "health"
	ModMetrics        ModName = "metrics"
	ModAdmin          ModName = "admin"
	ModAdminMetadata  ModName = "admin.metadata"
	ModLifecycle      ModName = "lifecycle"
	ModRuntimeFailure ModName = "runtime.failure"
	// ModService 是本次运行的服务类型对应的 Service（RegisterServer 登记的那一个），App 在任何 Mod Init 之前登记。
	// 它只用于 Mod 在自己的生命周期里调用服务声明的可选钩子（例如 wiring/nest 在 Nest 引擎启动之前调用
	// NestHandlerInstaller 注册实例 handler）；不是给 Mod 调业务方法的入口。未知的服务类型不登记（随后启动失败）。
	ModService ModName = "service"
)
