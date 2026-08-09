package syncstream

type MetricSink interface {
	Gauge(name string, value float64, labels map[string]string)
}
type HealthOptions struct {
	MaxFailures     uint64
	MaxBackpressure uint64
}
type HealthStatus struct {
	Healthy   bool
	Reason    string
	Publisher PublisherMetrics
	Buffered  BufferedPublisherMetrics
}

func Health(publisher *Publisher, buffered *BufferedPublisher, options HealthOptions) HealthStatus {
	status := HealthStatus{Healthy: true}
	if publisher != nil {
		status.Publisher = publisher.Metrics()
	}
	if buffered != nil {
		status.Buffered = buffered.Metrics()
	}
	failures := status.Publisher.Failures + status.Buffered.Failures
	if options.MaxFailures > 0 && failures > options.MaxFailures {
		status.Healthy, status.Reason = false, "publish_failures"
	} else if options.MaxBackpressure > 0 && status.Buffered.Backpressure > options.MaxBackpressure {
		status.Healthy, status.Reason = false, "backpressure"
	}
	return status
}

func ExportMetrics(sink MetricSink, labels map[string]string, publisher *Publisher, buffered *BufferedPublisher) {
	if sink == nil {
		return
	}
	status := Health(publisher, buffered, HealthOptions{})
	sink.Gauge("cube_sync_packets_published", float64(status.Publisher.Published), labels)
	sink.Gauge("cube_sync_frames_published", float64(status.Publisher.Frames), labels)
	sink.Gauge("cube_sync_publish_failures", float64(status.Publisher.Failures+status.Buffered.Failures), labels)
	sink.Gauge("cube_sync_queue_backpressure", float64(status.Buffered.Backpressure), labels)
	sink.Gauge("cube_sync_queue_depth_total", float64(status.Buffered.Queued), labels)
}
