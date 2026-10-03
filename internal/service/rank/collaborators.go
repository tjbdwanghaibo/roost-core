// Package Rank supplies the collaborators the rank service needs from this
// project. roost-codegen created this file once and will not overwrite it.
package Rank

import (
	"github.com/tjbdwanghaibo/roost-core/kit/service/servicemetrics"
)

// The rank service takes no collaborators: what is ranked, how a submit
// combines with the stored value and when a season ends are all the caller's
// (a board id, an UpdateMode and a RequestID per submit). Reset is
// deliberately absent from the bus interface — emptying a board is an
// operator action with an audit trail, not something every peer can reach.

// Metrics receives the service's counters. nil means no reporting and never
// fails an operation; wire the project's servicemetrics.Reporter here.
func Metrics() servicemetrics.Reporter { return nil }
