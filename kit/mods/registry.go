package mods

import (
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/app"
)

// Capability describes one startup-time Registry publication.
type Capability struct {
	Name  app.ModName
	Value any
}

// RegisterAll preflights the complete capability set before publishing it.
// App performs Provide serially, so after this validation the individual
// registrations cannot conflict with another Mod midway through the batch.
func RegisterAll(registry *app.Registry, capabilities ...Capability) error {
	if registry == nil {
		return fmt.Errorf("mods: nil registry")
	}
	seen := make(map[app.ModName]struct{}, len(capabilities))
	for _, capability := range capabilities {
		if capability.Name == "" {
			return fmt.Errorf("mods: capability name is empty")
		}
		if _, duplicate := seen[capability.Name]; duplicate {
			return fmt.Errorf("mods: duplicate capability %q in batch", capability.Name)
		}
		seen[capability.Name] = struct{}{}
		if existing, exists := registry.Get(capability.Name); exists {
			return fmt.Errorf("mods: capability %q already registered by %T", capability.Name, existing)
		}
	}
	for _, capability := range capabilities {
		if err := registry.Register(capability.Name, capability.Value); err != nil {
			return fmt.Errorf("mods: register capability %q after successful preflight: %w", capability.Name, err)
		}
	}
	return nil
}
