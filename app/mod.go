package app

import (
	"context"

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
