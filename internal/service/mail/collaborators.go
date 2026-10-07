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

// Metrics receives the service's counters. The default writes them into the
// process's metrics registry, served as service_* series on the ops /metrics
// endpoint (labels service, op, reason, name, key). Turn them off with
// service_metrics.enabled: false in config, or by returning nil here; nil
// never fails an operation.
func Metrics() servicemetrics.Reporter { return servicemetrics.NewMetricsReporter("mail") }
