package manager

import (
	"fmt"
	"sort"

	"github.com/tjbdwanghaibo/roost-core/app"
)

// Order returns the managers in start order: every manager appears
// after the managers it declares via app.ManagerDependencyProvider.
//
// Order among managers with no dependency relationship is **registration
// order**, not map order. That matters more than it looks: startup order is
// part of a service's observable behaviour, and an order that varies per
// process turns an ordering bug into an occasional one.
//
// core's container.TopologicalSortCache is deliberately not used here. It
// seeds its queue from a map, so independent managers come out in a different
// order on every process; and it reports a cycle by logging and returning nil, while
// a startup gate has to say which manager is in the cycle.
func Order(managers []app.IManager) ([]app.IManager, error) {
	// No fast path for a single manager: it would skip dependency validation,
	// so one manager declaring DependsOn on something never registered would
	// be accepted and then fail as a nil reference at runtime.
	byName := make(map[string]app.IManager, len(managers))
	registrationIndex := make(map[string]int, len(managers))
	for index, manager := range managers {
		if manager == nil {
			return nil, fmt.Errorf("manager: entry %d is nil", index)
		}
		name := manager.Name()
		if name == "" {
			return nil, fmt.Errorf("manager: entry %d has an empty name", index)
		}
		if _, exists := byName[name]; exists {
			return nil, fmt.Errorf("manager: duplicate manager %q", name)
		}
		byName[name] = manager
		registrationIndex[name] = index
	}

	const (
		unvisited = 0
		visiting  = 1
		visited   = 2
	)
	state := make(map[string]int, len(managers))
	sorted := make([]app.IManager, 0, len(managers))

	var visit func(name string, requiredBy string) error
	visit = func(name string, requiredBy string) error {
		switch state[name] {
		case visited:
			return nil
		case visiting:
			return fmt.Errorf("manager: dependency cycle at %q", name)
		}
		manager, exists := byName[name]
		if !exists {
			return fmt.Errorf("manager: %q depends on missing manager %q", requiredBy, name)
		}
		state[name] = visiting
		if provider, ok := manager.(app.ManagerDependencyProvider); ok {
			dependencies := append([]string(nil), provider.DependsOn()...)
			// Visit dependencies in registration order so the resulting
			// sequence is reproducible regardless of how DependsOn orders them.
			sort.SliceStable(dependencies, func(i, j int) bool {
				return registrationIndex[dependencies[i]] < registrationIndex[dependencies[j]]
			})
			for _, dependency := range dependencies {
				if dependency == "" {
					continue
				}
				if dependency == name {
					return fmt.Errorf("manager: dependency cycle at %q", name)
				}
				if err := visit(dependency, name); err != nil {
					return err
				}
			}
		}
		state[name] = visited
		sorted = append(sorted, manager)
		return nil
	}

	for _, manager := range managers {
		if err := visit(manager.Name(), ""); err != nil {
			return nil, err
		}
	}
	return sorted, nil
}
