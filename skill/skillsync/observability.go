package skillsync

import "time"

type MetricSink interface {
	Gauge(name string, value float64, labels map[string]string)
}

type HealthOptions struct {
	MaxPending            int
	MaxPendingBytes       int64
	MaxPendingAge         time.Duration
	MaxPublishFailures    uint64
	MaxVisibilityFailures uint64
}
type HealthStatus struct {
	Healthy     bool
	Reason      string
	Coordinator CoordinatorMetrics
	Outbox      OutboxMetrics
}

func (coordinator *Coordinator) Health(options HealthOptions) HealthStatus {
	status := HealthStatus{Healthy: true, Coordinator: coordinator.Metrics(), Outbox: coordinator.outbox.Metrics()}
	if options.MaxPending > 0 && status.Outbox.Pending > options.MaxPending {
		status.Healthy, status.Reason = false, "outbox_pending"
	} else if options.MaxPendingBytes > 0 && status.Outbox.PendingBytes > options.MaxPendingBytes {
		status.Healthy, status.Reason = false, "outbox_pending_bytes"
	} else if options.MaxPendingAge > 0 && status.Outbox.OldestPendingAge > options.MaxPendingAge {
		status.Healthy, status.Reason = false, "outbox_pending_age"
	} else if options.MaxPublishFailures > 0 && status.Coordinator.PublishFailures > options.MaxPublishFailures {
		status.Healthy, status.Reason = false, "publish_failures"
	} else if options.MaxVisibilityFailures > 0 && status.Coordinator.VisibilityFailures > options.MaxVisibilityFailures {
		status.Healthy, status.Reason = false, "visibility_failures"
	}
	return status
}

func (coordinator *Coordinator) ExportMetrics(sink MetricSink, labels map[string]string) {
	if sink == nil {
		return
	}
	value, outbox := coordinator.Metrics(), coordinator.outbox.Metrics()
	sink.Gauge("skillsync_published", float64(value.Published), labels)
	sink.Gauge("skillsync_publish_failures", float64(value.PublishFailures), labels)
	sink.Gauge("skillsync_filtered", float64(value.Filtered), labels)
	sink.Gauge("skillsync_visibility_failures", float64(value.VisibilityFailures), labels)
	sink.Gauge("skillsync_snapshot_recoveries", float64(value.SnapshotRecoveries), labels)
	sink.Gauge("skillsync_outbox_pending", float64(outbox.Pending), labels)
	sink.Gauge("skillsync_outbox_pending_bytes", float64(outbox.PendingBytes), labels)
	sink.Gauge("skillsync_outbox_oldest_pending_seconds", outbox.OldestPendingAge.Seconds(), labels)
	sink.Gauge("skillsync_outbox_publish_attempts", float64(outbox.PublishAttempts), labels)
}
