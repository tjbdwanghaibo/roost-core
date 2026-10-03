// Package Activity supplies the collaborators the activity service needs from this
// project. roost-codegen created this file once and will not overwrite it.
package Activity

import (
	"github.com/tjbdwanghaibo/roost-core/kit/service/servicemetrics"
)

// The activity service takes no collaborators: it aggregates what game
// servers report and says when a phase is collected. What the phase MEANS —
// which activity, what a point is worth, what the settlement pays — is the
// game's, and it stays in the game process, on the two sides of this service:
// the progress it applies and the dispatch it acks.

// Metrics receives the service's counters. nil means no reporting and never
// fails an operation; wire the project's servicemetrics.Reporter here.
func Metrics() servicemetrics.Reporter { return nil }
