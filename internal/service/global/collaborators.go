// Package Global supplies the collaborators the global service needs from this
// project. roost-codegen created this file once and will not overwrite it.
package Global

import (
	"github.com/tjbdwanghaibo/roost-core/kit/service/servicemetrics"
)

// The global service takes no collaborators: a route is a binding between a
// game server and a coordination group, and a lease is that server saying it
// is still alive. Both are state this service owns outright, so there is no
// policy for a project to supply — what a project decides is who calls Bind
// and who drives a migration, and those are callers, not collaborators.

// Metrics receives the service's counters. nil means no reporting and never
// fails an operation; wire the project's servicemetrics.Reporter here.
func Metrics() servicemetrics.Reporter { return nil }
