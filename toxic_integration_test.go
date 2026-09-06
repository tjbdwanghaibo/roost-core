//go:build integration

package dataengine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// toxiproxyClient is the slice of the toxiproxy HTTP API these tests need.
// No client library: three requests are not worth a dependency in go.mod.
type toxiproxyClient struct{ base string }

// toxiproxyEnv returns the toxiproxy API and the proxied NATS URL the
// environment script exported, or skips — unless ROOST_IT_TOXIPROXY=1 (the
// nightly fault matrix), where a missing proxy is a failure, not a skip.
func toxiproxyEnv(t *testing.T) (toxiproxyClient, string) {
	t.Helper()
	if os.Getenv("ROOST_DATAENGINE_IT") != "1" {
		t.Skip("set ROOST_DATAENGINE_IT=1 or use scripts/integration/dataengine-env.sh test")
	}
	api := os.Getenv("ROOST_DATAENGINE_IT_TOXIPROXY_URL")
	natsURL := os.Getenv("ROOST_DATAENGINE_IT_NATS_PROXIED_URL")
	if api == "" || natsURL == "" {
		if os.Getenv("ROOST_IT_TOXIPROXY") == "1" {
			t.Fatal("ROOST_IT_TOXIPROXY=1 but the environment exported no toxiproxy; install toxiproxy-server and rerun dataengine-env.sh up")
		}
		t.Skip("toxiproxy-server not installed; network fault tests need it (brew install toxiproxy)")
	}
	client := toxiproxyClient{base: api}
	client.reset(t)
	t.Cleanup(func() { client.reset(t) })
	return client, natsURL
}

func (c toxiproxyClient) do(t *testing.T, method, path string, body any) {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, c.base+path, &payload)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("toxiproxy %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("toxiproxy %s %s: status %d", method, path, resp.StatusCode)
	}
}

func (c toxiproxyClient) reset(t *testing.T) { c.do(t, http.MethodPost, "/reset", nil) }

// addToxic applies one toxic to every NATS proxy.
func (c toxiproxyClient) addToxic(t *testing.T, name, kind string, attributes map[string]any) {
	t.Helper()
	for index := 1; index <= 3; index++ {
		c.do(t, http.MethodPost, fmt.Sprintf("/proxies/nats-%d/toxics", index), map[string]any{
			"name": name, "type": kind, "stream": "downstream", "toxicity": 1.0, "attributes": attributes,
		})
	}
}

func subscribeEffects(t *testing.T, fx *realFixture, consumer, topic string) *atomic.Int32 {
	t.Helper()
	var handled atomic.Int32
	subscription, err := fx.jetStream.Subscribe(fx.context(), fnats.JetStreamConsumerConfig{
		Stream: fx.stream, Name: consumer, Durable: consumer,
		FilterSubject: fx.effectSub + "." + topic, DeliverPolicy: fnats.JetStreamDeliverAll,
		AckWait: 5 * time.Second, MaxDeliver: 5, MaxAckPending: 8,
	}, func(context.Context, *fnats.JetStreamMsg) error {
		handled.Add(1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(subscription.Stop)
	return &handled
}

// Invariant 1, success not before the commit point — and not AFTER it either:
// the commit point is WAL + Mongo, and NATS carries effects through the
// outbox afterwards. Three seconds of network latency towards every NATS node
// must therefore neither delay the caller's commit acknowledgement by three
// seconds nor make it fail; the effect arrives once the network recovers, and
// exactly once.
func TestToxicNATSLatencyKeepsTheCommitOnTheDurablePath(t *testing.T) {
	proxy, natsURL := toxiproxyEnv(t)
	fx := newRealFixtureWithNATS(t, natsURL)
	defer fx.close()
	handled := subscribeEffects(t, fx, "latency-consumer", "latency")

	proxy.addToxic(t, "slow", "latency", map[string]any{"latency": 3000, "jitter": 0})
	record := realRecord(40, []coredata.Mutation{
		realPut(t, fx.database, "toxic_players", 701, 0, 1, bson.M{"name": "latency"}),
	})
	record.Effects = []coredata.Effect{{ID: "effect-toxic-40", Topic: "latency", Payload: []byte("payload")}}
	started := time.Now()
	ticket, err := fx.runtime.Projector.CommitSystem(fx.context(), record)
	if err != nil {
		t.Fatalf("commit failed under NATS latency: %v", err)
	}
	if err := coredata.WaitProjection(fx.context(), ticket); err != nil {
		t.Fatalf("projection failed under NATS latency: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2500*time.Millisecond {
		t.Fatalf("commit + projection took %s under 3s NATS latency; the durable path is waiting on the bus", elapsed)
	}
	assertDocumentVersion(t, fx, "toxic_players", 701, 1)

	proxy.reset(t)
	waitFor(t, 30*time.Second, "effect delivery after latency cleared", func() bool {
		return collectionCount(fx, outboxCollection) == 0 && handled.Load() == 1
	})
	if got := handled.Load(); got != 1 {
		t.Fatalf("effect deliveries=%d, want exactly 1", got)
	}
}

// Invariant 4, admission = execution, on the effect path: connections to
// every NATS node are reset by the network while a commit with an effect goes
// through. The commit is admitted (durable) regardless, the outbox keeps the
// effect, and when the network heals the effect is delivered exactly once —
// a reset is not a reason to drop or to duplicate.
func TestToxicNATSConnectionResetDeliversTheEffectExactlyOnce(t *testing.T) {
	proxy, natsURL := toxiproxyEnv(t)
	fx := newRealFixtureWithNATS(t, natsURL)
	defer fx.close()
	handled := subscribeEffects(t, fx, "reset-consumer", "reset")

	proxy.addToxic(t, "reset", "reset_peer", map[string]any{"timeout": 0})
	record := realRecord(41, []coredata.Mutation{
		realPut(t, fx.database, "toxic_players", 702, 0, 1, bson.M{"name": "reset"}),
	})
	record.Effects = []coredata.Effect{{ID: "effect-toxic-41", Topic: "reset", Payload: []byte("payload")}}
	ticket, err := fx.runtime.Projector.CommitSystem(fx.context(), record)
	if err != nil {
		t.Fatalf("commit failed while NATS connections were being reset: %v", err)
	}
	if err := coredata.WaitProjection(fx.context(), ticket); err != nil {
		t.Fatalf("projection was coupled to the bus: %v", err)
	}
	assertDocumentVersion(t, fx, "toxic_players", 702, 1)
	waitFor(t, 5*time.Second, "outbox item to remain pending while connections reset", func() bool {
		return collectionCount(fx, outboxCollection) == 1
	})

	proxy.reset(t)
	waitFor(t, 30*time.Second, "outbox replay after the network healed", func() bool {
		return collectionCount(fx, outboxCollection) == 0 && handled.Load() == 1
	})
	if got := handled.Load(); got != 1 {
		t.Fatalf("effect deliveries=%d, want exactly 1", got)
	}
}
