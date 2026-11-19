package saga

import (
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"testing"
	"time"
)

func TestDeliveryAgeSeparatesRetriesAndSkipsUnknownClock(t *testing.T) {
	at := time.Unix(100, 0)
	count := func(delivery string) int64 {
		return metrics.HistogramCount("saga.consumer.delivery_age", metrics.Labels{"stage": "nest_start", "delivery": delivery})
	}
	first, retry := count("first"), count("retry")
	for _, msg := range []*fnats.JetStreamMsg{nil, {}, {NumDelivered: 1}, {StoredAt: at.Add(time.Second), NumDelivered: 1}, {StoredAt: at}, {StoredAt: at.Add(-time.Second), NumDelivered: 1}, {StoredAt: at.Add(-10 * time.Second), NumDelivered: 3}} {
		observeDeliveryAge("nest_start", msg, at)
	}
	if count("first") != first+1 || count("retry") != retry+1 {
		t.Fatal("delivery age included synthetic samples or merged retries into first deliveries")
	}
}
