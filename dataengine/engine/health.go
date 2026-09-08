package engine

import (
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/nestwal"
)

// HealthMessage is the one-line health summary the Data Engine Mod publishes:
// WAL backlog, projection and outbox failure counters in a fixed order so an
// operator can grep it across restarts. It lives with the engine (not the Mod)
// because it only depends on engine statistics.
func HealthMessage(walStats nestwal.Stats, projectorStats ProjectorStats, outboxStats OutboxWorkerStats) string {
	return fmt.Sprintf("wal_unacked=%d wal_oldest=%s projection_failures=%d outbox_pending=%d outbox_oldest=%s publish_failures=%d store_failures=%d fatal_projection_conflicts=%d", projectorStats.WALUnacked, walStats.OldestUnackedAge, projectorStats.ProjectionFailures, outboxStats.Pending, outboxStats.OldestAge, outboxStats.PublishFailures, outboxStats.StoreFailures, projectorStats.FatalProjectionConflicts)
}
