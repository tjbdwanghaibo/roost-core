package engine

import (
	"context"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
	"testing"
	"time"
)

func requireBacklogGauge(t *testing.T, name string, want int64) {
	t.Helper()
	for _, s := range metrics.Snapshot() {
		if s.Name == name && s.Value == want {
			return
		}
	}
	t.Fatalf("backlog gauge %s=%d missing", name, want)
}
func TestProjectionAdmissionAndAckPublishBacklogGauge(t *testing.T) {
	metrics.DefaultRegistry().Reset()
	opts := nestwal.DefaultOptions(t.TempDir())
	opts.WriterVersion = nestwal.WriterVersionV2
	w, err := nestwal.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewProjector(w, newProjectorOutboxFake(), ProjectorOptions{CloseWAL: true, ManualReplay: true})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(context.Background())
	r := projectorRecord(1, false)
	if err = p.reserve(r, true); err != nil {
		t.Fatal(err)
	}
	requireBacklogGauge(t, "dataengine.projection.pending", 1)
	p.discard(r.ID)
	requireBacklogGauge(t, "dataengine.projection.pending", 0)
}
func TestOutboxSamplingPublishesBacklogAndAge(t *testing.T) {
	metrics.DefaultRegistry().Reset()
	store := newProjectorOutboxFake()
	now := time.Now()
	store.pending["b7"] = OutboxItem{CreatedAt: now.Add(-time.Minute)}
	w, err := NewOutboxWorker(store, &successfulOutboxPublisher{}, OutboxWorkerOptions{Owner: "b7"})
	if err != nil {
		t.Fatal(err)
	}
	w.now = func() time.Time { return now }
	if err = w.RefreshBacklog(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireBacklogGauge(t, "dataengine.outbox.pending", 1)
	requireBacklogGauge(t, "dataengine.outbox.oldest_age_ms", 60000)
	delete(store.pending, "b7")
	if err = w.RefreshBacklog(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireBacklogGauge(t, "dataengine.outbox.pending", 0)
	requireBacklogGauge(t, "dataengine.outbox.oldest_age_ms", 0)
}
