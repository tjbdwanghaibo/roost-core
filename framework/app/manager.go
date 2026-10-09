package app

import "context"

// IManager is the interface for business managers driven by the manager
// engine (roost-core/manager.Engine, wrapped by roost-kit/manager.ManagerMod).
// Managers handle specific business concerns (entity, checkpoint, nest, etc.)
// and are started/stopped based on the current service type.
type IManager interface {
	Name() string
	Start(r *Registry) error
	Stop()
}

// IManagerStopperWithContext can be implemented by managers that need
// bounded, observable shutdown. The manager engine prefers it over Stop when present.
type IManagerStopperWithContext interface {
	StopWithContext(context.Context) error
}

// ManagerDependencyProvider can be implemented by managers that have startup
// dependencies on other managers in the same engine.
type ManagerDependencyProvider interface {
	DependsOn() []string
}
