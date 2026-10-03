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

// Metrics receives the service's counters. nil means no reporting and never
// fails an operation; wire the project's servicemetrics.Reporter here.
func Metrics() servicemetrics.Reporter { return nil }
