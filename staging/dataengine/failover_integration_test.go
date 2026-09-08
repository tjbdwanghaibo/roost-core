//go:build integration

package dataengine

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestRealMongoPrimaryFailoverContinuesProjection(t *testing.T) {
	fx := newRealFixture(t)
	defer fx.close()
	t.Cleanup(func() { healEnvironment(t) })

	if err := fx.runtime.Store.Project(fx.context(), realRecord(20, []coredata.Mutation{
		realPut(t, fx.database, "failover_players", 501, 0, 1, bson.M{"name": "before"}),
	})); err != nil {
		t.Fatal(err)
	}
	stopped := environmentResult(runEnvironment(t, "fault", "mongo-primary"))
	if !strings.HasPrefix(stopped, "mongo-") {
		t.Fatalf("unexpected stopped Mongo node %q", stopped)
	}

	set, err := bson.Marshal(bson.D{{Key: "name", Value: "after"}})
	if err != nil {
		t.Fatal(err)
	}
	record := realRecord(21, []coredata.Mutation{{
		Key:  coredata.DocumentKey{Database: fx.database, Resource: "failover_players", ID: 501},
		Kind: coredata.MutationPatch, ExpectedVersion: 1, NextVersion: 2,
		Mask: 1, Schema: 1, Codec: "bson-v2", Patch: coredata.FieldPatch{SetBSON: set},
	}})
	ticket, err := fx.runtime.Projector.CommitSystem(fx.context(), record)
	if err != nil {
		t.Fatal(err)
	}
	if err := coredata.WaitProjection(fx.context(), ticket); err != nil {
		t.Fatalf("projection did not survive primary failover: %v", err)
	}
	doc := findDocument(t, fx, "failover_players", 501)
	if documentVersion(t, doc) != 2 || doc["name"] != "after" {
		t.Fatalf("projection after failover=%#v", doc)
	}

	healEnvironment(t)
}

func TestRealNATSOutageDoesNotBlockProjectionAndRecoversOutbox(t *testing.T) {
	fx := newRealFixture(t)
	defer fx.close()
	t.Cleanup(func() { healEnvironment(t) })

	var handled atomic.Int32
	subscription, err := fx.jetStream.Subscribe(fx.context(), fnats.JetStreamConsumerConfig{
		Stream: fx.stream, Name: "outage-consumer", Durable: "outage-consumer",
		FilterSubject: fx.effectSub + ".outage", DeliverPolicy: fnats.JetStreamDeliverAll,
		AckWait: 5 * time.Second, MaxDeliver: 5, MaxAckPending: 8,
	}, func(context.Context, *fnats.JetStreamMsg) error {
		handled.Add(1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(subscription.Stop)

	runEnvironment(t, "fault", "nats-all")
	record := realRecord(22, []coredata.Mutation{
		realPut(t, fx.database, "outage_players", 601, 0, 1, bson.M{"name": "projected"}),
	})
	record.Effects = []coredata.Effect{{ID: "effect-outage-22", Topic: "outage", Payload: []byte("payload")}}
	ticket, err := fx.runtime.Projector.CommitSystem(fx.context(), record)
	if err != nil {
		t.Fatal(err)
	}
	if err := coredata.WaitProjection(fx.context(), ticket); err != nil {
		t.Fatalf("Mongo projection was coupled to NATS availability: %v", err)
	}
	assertDocumentVersion(t, fx, "outage_players", 601, 1)
	waitFor(t, 5*time.Second, "outbox item to remain pending during outage", func() bool {
		return collectionCount(fx, outboxCollection) == 1
	})

	healEnvironment(t)
	waitFor(t, 20*time.Second, "outbox replay after NATS recovery", func() bool {
		return collectionCount(fx, outboxCollection) == 0 && handled.Load() == 1
	})
	if got := handled.Load(); got != 1 {
		t.Fatalf("effect deliveries=%d, want exactly 1", got)
	}
}

func TestRealJetStreamLeaderFailoverPreservesDedupAndOrder(t *testing.T) {
	fx := newRealFixture(t)
	defer fx.close()
	t.Cleanup(func() { healEnvironment(t) })

	var mu sync.Mutex
	var sequences []uint64
	subscription, err := fx.jetStream.Subscribe(fx.context(), fnats.JetStreamConsumerConfig{
		Stream: fx.stream, Name: "leader-consumer", Durable: "leader-consumer",
		FilterSubject: fx.effectSub + ".leader", DeliverPolicy: fnats.JetStreamDeliverAll,
		AckWait: 5 * time.Second, MaxDeliver: 5, MaxAckPending: 8,
	}, func(_ context.Context, msg *fnats.JetStreamMsg) error {
		mu.Lock()
		sequences = append(sequences, msg.StreamSeq)
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(subscription.Stop)

	subject := fx.effectSub + ".leader"
	first, err := fx.jetStream.Publish(fx.context(), subject, []byte("first"), fnats.JetStreamPublishOptions{MsgID: "leader-msg-1"})
	if err != nil || first.Duplicate || first.Sequence == 0 {
		t.Fatalf("first publish ack=%+v err=%v", first, err)
	}
	stopped := environmentResult(runEnvironment(t, "fault", "nats-leader", fx.stream))
	if !strings.HasPrefix(stopped, "nats-") {
		t.Fatalf("unexpected stopped NATS node %q", stopped)
	}

	duplicate := publishEventually(t, fx, subject, []byte("first"), "leader-msg-1")
	if !duplicate.Duplicate || duplicate.Sequence != first.Sequence {
		t.Fatalf("duplicate publish ack=%+v, first=%+v", duplicate, first)
	}
	second := publishEventually(t, fx, subject, []byte("second"), "leader-msg-2")
	if second.Duplicate || second.Sequence <= first.Sequence {
		t.Fatalf("second publish ack=%+v, first=%+v", second, first)
	}
	waitFor(t, 10*time.Second, "two ordered unique consumer deliveries", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(sequences) == 2
	})
	mu.Lock()
	got := append([]uint64(nil), sequences...)
	mu.Unlock()
	if got[0] != first.Sequence || got[1] != second.Sequence {
		t.Fatalf("consumer stream sequences=%v, want [%d %d]", got, first.Sequence, second.Sequence)
	}

	healEnvironment(t)
}

func publishEventually(t *testing.T, fx *realFixture, subject string, payload []byte, msgID string) fnats.JetStreamPublishAck {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		ack, err := fx.jetStream.Publish(ctx, subject, payload, fnats.JetStreamPublishOptions{MsgID: msgID})
		cancel()
		if err == nil {
			return ack
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("publish %s did not recover: %v", msgID, lastErr)
	return fnats.JetStreamPublishAck{}
}

func collectionCount(fx *realFixture, resource string) int64 {
	count, err := fx.mongo.Database(fx.database).Collection(resource).CountDocuments(fx.context(), bson.M{})
	if err != nil {
		return -1
	}
	return count
}

func waitFor(t *testing.T, timeout time.Duration, description string, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", description)
}

func runEnvironment(t *testing.T, arguments ...string) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve integration script path")
	}
	script := filepath.Clean(filepath.Join(filepath.Dir(source), "..", "scripts", "integration", "dataengine-env.sh"))
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, script, arguments...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("environment %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return string(output)
}

func environmentResult(output string) string {
	fields := strings.Fields(output)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

func healEnvironment(t *testing.T) {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Errorf("heal isolated environment: %v", recovered)
		}
	}()
	runEnvironment(t, "heal")
}
