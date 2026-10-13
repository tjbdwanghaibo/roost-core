package engine

import (
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
)

// HealthMessage 生成 DataEngine Mod 发布的一行健康摘要：WAL 积压、投影与 outbox 失败计数，
// 字段顺序固定，便于跨重启检索。它只依赖 engine 的统计，所以放在 engine 而不是 Mod。
func HealthMessage(walStats nestwal.Stats, projectorStats ProjectorStats, outboxStats OutboxWorkerStats) string {
	return fmt.Sprintf("wal_unacked=%d wal_oldest=%s projection_failures=%d outbox_pending=%d outbox_oldest=%s publish_failures=%d store_failures=%d fatal_projection_conflicts=%d projection_backlog_warning=%t admission_rejected=%d", projectorStats.WALUnacked, walStats.OldestUnackedAge, projectorStats.ProjectionFailures, outboxStats.Pending, outboxStats.OldestAge, outboxStats.PublishFailures, outboxStats.StoreFailures, projectorStats.FatalProjectionConflicts, projectorStats.BacklogWarning, projectorStats.AdmissionRejected)
}
