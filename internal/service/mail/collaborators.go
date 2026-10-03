// Package Mail supplies the collaborators the mail service needs from this
// project. roost-codegen created this file once and will not overwrite it.
package Mail

import (
	"github.com/tjbdwanghaibo/roost-core/kit/service/mail"
	"github.com/tjbdwanghaibo/roost-core/kit/service/servicemetrics"
)

// Broadcast delivers broadcast mail to every online player. nil is a valid,
// fail-closed configuration: broadcasts are refused rather than dropped.
func Broadcast() mail.Deliverer { return nil }

// Metrics receives the service's counters. nil means no reporting and never
// fails an operation; wire the project's servicemetrics.Reporter here.
func Metrics() servicemetrics.Reporter { return nil }
