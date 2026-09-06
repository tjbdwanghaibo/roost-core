package saga

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	coresaga "github.com/tjbdwanghaibo/roost-core/saga"
	"github.com/tjbdwanghaibo/roost-kit/nestwal"
)

func expectErr(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to contain %q", err, want)
	}
}

// A consumer with limits the broker cannot honour, or a process timeout that
// outlives its ack wait, would redeliver work that is still running. Each
// refusal is pinned; only the happy path had a test.
func TestSubscribeNestStartsRefusesEachUnsafeConfig(t *testing.T) {
	valid := NestStartConsumerConfig{Stream: "ROOST_EFFECTS", Durable: "game-saga-start", EffectPrefix: "roost.effect"}
	cases := []struct {
		name   string
		mutate func(*NestStartConsumerConfig)
		want   string
	}{
		{"blank stream", func(c *NestStartConsumerConfig) { c.Stream = " " }, "stream, durable and effect prefix are required"},
		{"blank durable", func(c *NestStartConsumerConfig) { c.Durable = "" }, "stream, durable and effect prefix are required"},
		{"prefix with wildcard", func(c *NestStartConsumerConfig) { c.EffectPrefix = "roost.>" }, "stream, durable and effect prefix are required"},
		{"process timeout not below ack wait", func(c *NestStartConsumerConfig) { c.AckWait, c.ProcessTimeout = 5*time.Second, 5*time.Second }, "unsafe Nest start consumer limits"},
		{"max deliver above broker cap", func(c *NestStartConsumerConfig) { c.MaxDeliver = 1_000_001 }, "unsafe Nest start consumer limits"},
		{"max ack pending above broker cap", func(c *NestStartConsumerConfig) { c.MaxAckPending = 65_537 }, "unsafe Nest start consumer limits"},
		{"nak backoff above a day", func(c *NestStartConsumerConfig) { c.NakBackoffMin, c.NakBackoffMax = time.Second, 25*time.Hour }, "unsafe Nest start consumer limits"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &startJetStream{}
			cfg := valid
			tc.mutate(&cfg)
			_, err := SubscribeNestStarts(context.Background(), client, cfg, &startCapture{})
			expectErr(t, err, tc.want)
			if client.handler != nil {
				t.Fatal("a refused config must not subscribe")
			}
		})
	}
	if _, err := SubscribeNestStarts(context.Background(), nil, valid, &startCapture{}); err == nil {
		t.Fatal("nil client accepted")
	}
}

// Wire decoding on the step side: an oversized frame, a foreign wire
// version, or a command that fails validation must be a permanent refusal,
// never a command handed to a step handler.
func TestDecodeStepCommandRefusesOversizedForeignAndInvalidEnvelopes(t *testing.T) {
	now := time.Now()
	good := coresaga.Command{ID: "c-1", IdempotencyKey: "k-1", SagaID: "s", SagaType: "rally", DefinitionVersion: 1, BusinessKey: "b", StepName: "reserve", Phase: coresaga.PhaseForward, Attempt: 1, Topic: "rally.reserve", DeadlineAt: now.Add(time.Second), CreatedAt: now}
	encode := func(t *testing.T, version uint16, command coresaga.Command) []byte {
		t.Helper()
		raw, err := json.Marshal(commandEnvelope{Version: version, Command: command})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	if got, err := decodeStepCommand(&fnats.JetStreamMsg{Data: encode(t, coresaga.WireVersion, good)}); err != nil || got.ID != "c-1" {
		t.Fatalf("valid envelope: %+v, %v", got, err)
	}
	if _, err := decodeStepCommand(nil); !errors.Is(err, coresaga.ErrInvalidRecord) {
		t.Fatalf("nil message = %v", err)
	}
	if _, err := decodeStepCommand(&fnats.JetStreamMsg{Data: make([]byte, maxWireEnvelopeBytes+1)}); !errors.Is(err, coresaga.ErrInvalidRecord) {
		t.Fatalf("oversized frame = %v", err)
	}
	if _, err := decodeStepCommand(&fnats.JetStreamMsg{Data: encode(t, coresaga.WireVersion+1, good)}); !errors.Is(err, coresaga.ErrInvalidRecord) {
		t.Fatalf("foreign wire version = %v", err)
	}
	broken := good
	broken.Attempt = 0
	if _, err := decodeStepCommand(&fnats.JetStreamMsg{Data: encode(t, coresaga.WireVersion, broken)}); !errors.Is(err, coresaga.ErrInvalidRecord) {
		t.Fatalf("invalid command = %v", err)
	}
}

// The Nest start handler is the other inbound decoder: an oversized frame, an
// envelope without an effect id, or one on a foreign topic must not reach the
// starter, and every refusal is permanent (no redelivery loop).
func TestHandleNestStartRefusesEachMalformedEnvelopePermanently(t *testing.T) {
	encode := func(t *testing.T, env nestwal.EffectEnvelope) []byte {
		t.Helper()
		raw, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	// A start payload that would be accepted on its own, so that only the
	// envelope-level rule under test can be the reason for the refusal.
	effect, err := coresaga.NewStartEffect(coresaga.StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "r-9"})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"oversized frame":   make([]byte, maxWireEnvelopeBytes+1),
		"missing effect id": encode(t, nestwal.EffectEnvelope{Topic: coresaga.StartEffectTopic, Payload: effect.Payload}),
		"foreign topic":     encode(t, nestwal.EffectEnvelope{EffectID: effect.ID, Topic: "other", Payload: effect.Payload}),
		"undecodable start": encode(t, nestwal.EffectEnvelope{EffectID: effect.ID, Topic: coresaga.StartEffectTopic, Payload: []byte(`{"version":99}`)}),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			capture := &startCapture{}
			err := handleNestStart(context.Background(), &fnats.JetStreamMsg{Data: raw}, capture)
			if err == nil {
				t.Fatal("malformed envelope accepted")
			}
			if !fnats.IsPermanent(err) {
				t.Fatalf("refusal must be permanent, got %v", err)
			}
			if len(capture.requests) != 0 {
				t.Fatal("malformed envelope reached the starter")
			}
		})
	}
	if err := handleNestStart(context.Background(), nil, &startCapture{}); err == nil || !fnats.IsPermanent(err) {
		t.Fatalf("nil message = %v", err)
	}
}

// A receipt TTL that does not fit the Mongo index (sub-second, or beyond the
// int32 seconds range) is refused at EnsureInfrastructure with the value.
func TestMongoCommandInboxRefusesUnrepresentableReceiptTTL(t *testing.T) {
	for _, ttl := range []time.Duration{500 * time.Millisecond, time.Duration(int64(^uint32(0)>>1)+1) * time.Second} {
		inbox, err := NewMongoCommandInbox(newInboxMongoFake(), "game", "", CommandInboxOptions{ReceiptTTL: ttl})
		if err != nil {
			t.Fatal(err)
		}
		expectErr(t, inbox.EnsureInfrastructure(context.Background()), "invalid receipt ttl")
	}
}
