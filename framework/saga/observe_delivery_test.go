package saga

import (
	"context"
	"errors"
	"maps"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// Return the count and exact sum, not an estimate from histogram percentiles.
func timingSeries(name string, labels metrics.Labels) (int64, time.Duration) {
	for _, m := range metrics.Snapshot() {
		if m.Name == name && maps.Equal(m.Labels, labels) {
			return m.Count, time.Duration(m.TotalNanos)
		}
	}
	return 0, 0
}

func TestDeliverySplitInvalidTimesDoNotCreatePartialOrZeroSamples(t *testing.T) {
	at := time.Unix(123, 0)
	labels := metrics.Labels{"stage": "mongo_step", "topic": "split.invalid", "delivery": "first"}
	names := []string{"saga.consumer.append_to_callback", "saga.consumer.callback_to_worker"}
	before := map[string]int64{}
	for _, n := range names {
		before[n] = metrics.HistogramCount(n, labels)
	}
	for _, msg := range []*fnats.JetStreamMsg{nil, {}, {StoredAt: at}, {NumDelivered: 1}, {StoredAt: at.Add(time.Millisecond), NumDelivered: 1}} {
		deliveryTiming{callback: at, started: at.Add(time.Second)}.observe("mongo_step", "split.invalid", msg)
	}
	deliveryTiming{callback: at, started: at.Add(-time.Second)}.observe("mongo_step", "split.invalid", &fnats.JetStreamMsg{StoredAt: at.Add(-2 * time.Second), NumDelivered: 1})
	for _, n := range names {
		if metrics.HistogramCount(n, labels) != before[n] {
			t.Fatal("invented sample:", n)
		}
	}
}

func TestDeliverySplitAttributesOnlyTheAdmittedSlotWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const stage, topic = "native_step", "split.blocked"
		labels := metrics.Labels{"stage": stage, "topic": topic, "delivery": "retry"}
		a, sa := timingSeries("saga.consumer.append_to_callback", labels)
		b, sb := timingSeries("saga.consumer.callback_to_worker", labels)
		blocked := make(chan struct{})
		release := sync.OnceFunc(func() { close(blocked) })
		defer release()
		finished := make(chan struct{}, 4)
		handle := withConsumerWorkers(2, stage, topic, func(_ context.Context, m *fnats.JetStreamMsg, _ deliveryTiming) error {
			if m.NumDelivered == 0 {
				<-blocked
			}
			return nil
		})
		msg := func(n uint64) *fnats.JetStreamMsg {
			return &fnats.JetStreamMsg{NumDelivered: n, StoredAt: time.Now().Add(-300 * time.Millisecond), DeferSettle: func() func(error) { return func(error) { finished <- struct{}{} } }}
		}
		for range 2 {
			if err := handle(context.Background(), msg(0)); err != nil {
				t.Fatal(err)
			}
		}
		synctest.Wait()
		// Both callbacks reached the subscription at the same time. One cancels
		// while waiting; its two durations must not become a started sample.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		canceled, admitted := make(chan error, 1), make(chan error, 1)
		go func() { canceled <- handle(ctx, msg(2)) }()
		go func() { admitted <- handle(context.Background(), msg(2)) }()
		synctest.Wait()
		time.Sleep(1200 * time.Millisecond)
		cancel()
		if err := <-canceled; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		release()
		if err := <-admitted; err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if len(finished) != 3 {
			t.Fatalf("settled %d, want 3", len(finished))
		}
		na, ta := timingSeries("saga.consumer.append_to_callback", labels)
		nb, tb := timingSeries("saga.consumer.callback_to_worker", labels)
		if na-a != 1 || nb-b != 1 || ta-sa != 300*time.Millisecond || tb-sb != 1200*time.Millisecond {
			t.Fatalf("append->callback: %d/%v; callback->worker: %d/%v", na-a, ta-sa, nb-b, tb-sb)
		}
	})
}

func TestDeliverySplitSynchronousPathsExcludeHandlerTime(t *testing.T) {
	for _, tc := range []struct {
		name     string
		workers  int
		deferNil bool
	}{{"serial", 1, false}, {"no_defer", 2, false}, {"nil_settle", 2, true}} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				topic := "split." + tc.name
				labels := metrics.Labels{"stage": "mongo_step", "topic": topic, "delivery": "first"}
				a, sa := timingSeries("saga.consumer.append_to_callback", labels)
				b, sb := timingSeries("saga.consumer.callback_to_worker", labels)
				m := &fnats.JetStreamMsg{StoredAt: time.Now().Add(-2 * time.Second), NumDelivered: 1, Subject: "per.partition.dynamic"}
				if tc.deferNil {
					m.DeferSettle = func() func(error) { return nil }
				}
				want := errors.New("original outcome")
				h := withConsumerWorkers(tc.workers, "mongo_step", topic, func(context.Context, *fnats.JetStreamMsg, deliveryTiming) error { time.Sleep(time.Second); return want })
				if err := h(context.Background(), m); err != want {
					t.Fatal(err)
				}
				na, ta := timingSeries("saga.consumer.append_to_callback", labels)
				nb, tb := timingSeries("saga.consumer.callback_to_worker", labels)
				if na-a != 1 || nb-b != 1 || ta-sa != 2*time.Second || tb-sb != 0 {
					t.Fatalf("append->callback: %d/%v; callback->worker: %d/%v", na-a, ta-sa, nb-b, tb-sb)
				}
			})
		})
	}
}

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
