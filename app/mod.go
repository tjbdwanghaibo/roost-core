package app

import (
	"context"
	"time"

	"github.com/spf13/viper"
)

// Mod provides infrastructure capabilities to the Registry.
// Lifecycle: Init → Provide → Start → Stop (reverse order).
type Mod interface {
	Name() ModName
	Init(cfg *viper.Viper) error
	Provide(r *Registry) error // expose capabilities to registry
	Start() error
	Stop()
}

type ModStopperWithContext interface {
	StopWithContext(context.Context) error
}

// ModStopBudgetProvider 由需要固定停机时长的 Mod 实现（例如 dataengine 的
// dataengine.shutdown_timeout：排空 WAL 与投影）。App 在 shutdown.total_timeout 内
// 优先把声明的预算分给它，其余 Mod 均分剩下的时间；总时长不够时按比例缩放并告警。
// 返回值 <= 0 视为未声明。只影响 App 给 StopWithContext 的截止时间，超时后仍按
// 既有语义停止后续 Mod 的关闭、保留它们的资源。
type ModStopBudgetProvider interface {
	StopBudget() time.Duration
}

// ModDependencyProvider can be implemented by Mods that require other Mods to
// be initialized/provided/started first.
type ModDependencyProvider interface {
	DependsOn() []ModName
}

// ModOptionalDependencyProvider declares ordering constraints for capabilities
// that a Mod can integrate with but does not require. A named Mod is ordered
// before this Mod when it is present; an absent optional dependency is ignored.
// Use DependsOn for hard requirements so missing infrastructure still fails
// during graph validation.
type ModOptionalDependencyProvider interface {
	OptionalDependsOn() []ModName
}
