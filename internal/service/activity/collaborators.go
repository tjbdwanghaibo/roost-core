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

// Metrics receives the service's counters. The default writes them into the
// process's metrics registry, served as service_* series on the ops /metrics
// endpoint (labels service, op, reason, name, key). Turn them off with
// service_metrics.enabled: false in config, or by returning nil here; nil
// never fails an operation.
func Metrics() servicemetrics.Reporter { return servicemetrics.NewMetricsReporter("activity") }
