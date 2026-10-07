// Package Match supplies the collaborators the match service needs from this
// project. roost-codegen created this file once and will not overwrite it.
package Match

import (
	"github.com/tjbdwanghaibo/roost-core/kit/service/servicemetrics"
)

// The match service takes no matchmaking policy: it holds the queue and makes
// Commit atomic, and deciding which waiting tickets form a match is the game's
// job — a matchmaker in the game process reads Candidates, applies a
// match.Grouping (FirstComeGrouping, ScoreWindowGrouping or its own) and
// Commits. See the game-demo template's internal/service/<game>/matchmaker.go.

// Metrics receives the service's counters. The default writes them into the
// process's metrics registry, served as service_* series on the ops /metrics
// endpoint (labels service, op, reason, name, key). Turn them off with
// service_metrics.enabled: false in config, or by returning nil here; nil
// never fails an operation.
func Metrics() servicemetrics.Reporter { return servicemetrics.NewMetricsReporter("match") }
