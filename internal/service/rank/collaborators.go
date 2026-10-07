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

// Metrics receives the service's counters. The default writes them into the
// process's metrics registry, served as service_* series on the ops /metrics
// endpoint (labels service, op, reason, name, key). Turn them off with
// service_metrics.enabled: false in config, or by returning nil here; nil
// never fails an operation.
func Metrics() servicemetrics.Reporter { return servicemetrics.NewMetricsReporter("rank") }
