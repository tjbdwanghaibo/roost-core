//go:build integration

package dataengine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	engine "github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// natsToxiproxy drives the proxies these tests created on the environment's
// toxiproxy, one per isolated NATS node. No client library: a handful of
// requests are not worth a dependency in go.mod. RR-20261005-NC-208: the tests
// used the environment's shared nats-1..3 proxies and POST /reset, which
// removes every toxic on every proxy of that toxiproxy — other sessions'
// faults included. Each test now owns uniquely named proxies on ephemeral
// ports in front of the direct node URLs, adds and removes toxics only on them
// and deletes them at cleanup; the fixture ignores discovered servers, so it
// only ever dials these proxies.
type natsToxiproxy struct {
	base    string
	proxies []string
}

// toxiproxyEnv creates this test's own NATS proxies on the toxiproxy the
// environment script exported and returns them with the nats:// URL list that
// goes through them, or skips — unless ROOST_IT_TOXIPROXY=1 (the nightly fault
// matrix), where a missing toxiproxy is a failure, not a skip.
func toxiproxyEnv(t *testing.T) (natsToxiproxy, string) {
	t.Helper()
	if os.Getenv("ROOST_DATAENGINE_IT") != "1" {
		t.Skip("set ROOST_DATAENGINE_IT=1 or use scripts/integration/dataengine-env.sh test")
	}
	api := os.Getenv("ROOST_DATAENGINE_IT_TOXIPROXY_URL")
	directURL := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if api == "" || directURL == "" {
		if os.Getenv("ROOST_IT_TOXIPROXY") == "1" {
			t.Fatal("ROOST_IT_TOXIPROXY=1 but the environment exported no toxiproxy; install toxiproxy-server and rerun dataengine-env.sh up")
		}
		t.Skip("toxiproxy-server not installed; network fault tests need it (brew install toxiproxy)")
	}
	proxy := natsToxiproxy{base: api}
	var proxied []string
	for index, raw := range strings.Split(directURL, ",") {
		upstream := strings.TrimPrefix(strings.TrimSpace(raw), "nats://")
		if upstream == "" {
			continue
		}
		name := fmt.Sprintf("kit-dataengine-toxic-%d-%d-%d", os.Getpid(), time.Now().UnixNano(), index)
		var created struct {
			Listen string `json:"listen"`
		}
		body := proxy.call(t, http.MethodPost, "/proxies", map[string]any{"name": name, "listen": "127.0.0.1:0", "upstream": upstream, "enabled": true})
		if err := json.Unmarshal(body, &created); err != nil || created.Listen == "" {
			t.Fatalf("toxiproxy create %s: %v %s", name, err, body)
		}
		// Deleting the proxy removes its toxics with it.
		t.Cleanup(func() { proxy.call(t, http.MethodDelete, "/proxies/"+name, nil) })
		proxy.proxies = append(proxy.proxies, name)
		proxied = append(proxied, "nats://"+created.Listen)
	}
	if len(proxied) == 0 {
		t.Fatalf("no NATS URL in %q", directURL)
	}
	return proxy, strings.Join(proxied, ",")
}

func (c natsToxiproxy) call(t *testing.T, method, path string, body any) []byte {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, strings.TrimRight(c.base, "/")+path, &payload)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("toxiproxy %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	_, _ = out.ReadFrom(resp.Body)
	if resp.StatusCode >= 300 && !(method == http.MethodDelete && resp.StatusCode == http.StatusNotFound) {
		t.Fatalf("toxiproxy %s %s: status %d %s", method, path, resp.StatusCode, out.String())
	}
	return out.Bytes()
}

// heal removes every toxic on this test's own proxies and nothing else.
func (c natsToxiproxy) heal(t *testing.T) {
	t.Helper()
	for _, name := range c.proxies {
		var toxics []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(c.call(t, http.MethodGet, "/proxies/"+name+"/toxics", nil), &toxics); err != nil {
			t.Fatalf("toxiproxy list toxics of %s: %v", name, err)
		}
		for _, toxic := range toxics {
			c.call(t, http.MethodDelete, "/proxies/"+name+"/toxics/"+toxic.Name, nil)
		}
	}
}

// addToxic applies one toxic to every NATS proxy this test owns.
func (c natsToxiproxy) addToxic(t *testing.T, name, kind string, attributes map[string]any) {
	t.Helper()
	for _, proxy := range c.proxies {
		c.call(t, http.MethodPost, "/proxies/"+proxy+"/toxics", map[string]any{
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

	proxy.heal(t)
	waitFor(t, 30*time.Second, "effect delivery after latency cleared", func() bool {
		return collectionCount(fx, engine.OutboxCollection) == 0 && handled.Load() == 1
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
		return collectionCount(fx, engine.OutboxCollection) == 1
	})

	proxy.heal(t)
	waitFor(t, 30*time.Second, "outbox replay after the network healed", func() bool {
		return collectionCount(fx, engine.OutboxCollection) == 0 && handled.Load() == 1
	})
	if got := handled.Load(); got != 1 {
		t.Fatalf("effect deliveries=%d, want exactly 1", got)
	}
}

// Half-open: the `timeout` toxic with timeout=0 holds every NATS connection
// open but never lets a byte come back downstream. Unlike reset_peer, the
// client sees no error at all — its PUB goes out and the acknowledgement
// simply never arrives. This is the failure shape that turns "retry" into
// "hang": the publish must return within a bounded time (not block the
// worker forever), the commit must be unaffected (bus is not on the durable
// path), and once the network heals the effect is delivered exactly once —
// even though the broker may have stored the un-acked publish, because the
// outbox publishes with the effect ID as Msg-Id and the stream de-duplicates.
func TestToxicNATSHalfOpenAckLossIsBoundedAndDeliversExactlyOnce(t *testing.T) {
	proxy, natsURL := toxiproxyEnv(t)
	fx := newRealFixtureWithNATS(t, natsURL)
	defer fx.close()
	handled := subscribeEffects(t, fx, "halfopen-consumer", "halfopen")

	proxy.addToxic(t, "halfopen", "timeout", map[string]any{"timeout": 0})
	record := realRecord(43, []coredata.Mutation{
		realPut(t, fx.database, "toxic_players", 703, 0, 1, bson.M{"name": "halfopen"}),
	})
	record.Effects = []coredata.Effect{{ID: "effect-toxic-43", Topic: "halfopen", Payload: []byte("payload")}}
	started := time.Now()
	ticket, err := fx.runtime.Projector.CommitSystem(fx.context(), record)
	if err != nil {
		t.Fatalf("commit failed while NATS was half-open: %v", err)
	}
	if err := coredata.WaitProjection(fx.context(), ticket); err != nil {
		t.Fatalf("projection was coupled to the bus: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2500*time.Millisecond {
		t.Fatalf("commit+projection took %s while NATS was half-open; the bus is not on the durable path", elapsed)
	}
	assertDocumentVersion(t, fx, "toxic_players", 703, 1)

	// The worker's publish must fail within a bounded time rather than wait
	// for an acknowledgement that will never come; a hung publisher would
	// show neither a publish failure nor a delivery.
	waitFor(t, 20*time.Second, "a bounded publish failure while the ack path is black-holed", func() bool {
		return fx.runtime.Outbox.Stats().PublishFailures >= 1
	})
	if got := collectionCount(fx, engine.OutboxCollection); got != 1 {
		t.Fatalf("outbox items=%d while half-open, want the effect retained", got)
	}
	if got := handled.Load(); got != 0 {
		t.Fatalf("effect handled %d times before the network healed", got)
	}

	proxy.heal(t)
	waitFor(t, 30*time.Second, "outbox replay after the network healed", func() bool {
		return collectionCount(fx, engine.OutboxCollection) == 0 && handled.Load() >= 1
	})
	time.Sleep(2 * time.Second) // give a duplicate every chance to show up
	if got := handled.Load(); got != 1 {
		t.Fatalf("effect deliveries=%d, want exactly 1 (Msg-Id de-duplication after the un-acked publish)", got)
	}
}
