// Package Global supplies the collaborators the global service needs from this
// project. roost-codegen created this file once and will not overwrite it.
package Global

import (
	"github.com/tjbdwanghaibo/roost-core/kit/service/servicemetrics"
)

// The global service takes no collaborators: a route is a binding between a
// game server and a coordination group, state this service owns outright, so
// there is no policy for a project to supply — what a project decides is who
// calls Bind and who drives a migration, and those are callers, not
// collaborators. Whether a game server is alive is not asked here: that is
// the App's singleton lock, read with app.SingletonLiveness.Live.

// Metrics receives the service's counters. The default writes them into the
// process's metrics registry, served as service_* series on the ops /metrics
// endpoint (labels service, op, reason, name, key). Turn them off with
// service_metrics.enabled: false in config, or by returning nil here; nil
// never fails an operation.
func Metrics() servicemetrics.Reporter { return servicemetrics.NewMetricsReporter("global") }
