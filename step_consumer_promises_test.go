package saga

import (
	"context"
	"errors"
	"testing"
	"time"

	coresaga "github.com/tjbdwanghaibo/roost-core/saga"
	"github.com/tjbdwanghaibo/roost-kit/mongo/mongotest"
)

// U-0092 (C2): the step consumers mirror the Nest-start consumer's refusals
// (U-0035) but had only happy-path tests. Each refusal is pinned here and a
// refused config must never reach Subscribe.
func TestSubscribeMongoStepRefusesEachUnsafeConfig(t *testing.T) {
	valid := StepConsumerConfig{Stream: "ROOST_SAGA", Durable: "game-reserve", Topic: "reserve"}
	cases := []struct {
		name   string
		mutate func(*StepConsumerConfig)
		want   string
	}{
		{"blank stream", func(c *StepConsumerConfig) { c.Stream = "" }, "invalid step consumer configuration"},
		{"blank durable", func(c *StepConsumerConfig) { c.Durable = "" }, "invalid step consumer configuration"},
		{"topic with wildcard", func(c *StepConsumerConfig) { c.Topic = "reserve.>" }, "invalid step consumer configuration"},
		{"max deliver above broker cap", func(c *StepConsumerConfig) { c.MaxDeliver = 1_000_001 }, "unsafe step consumer limits"},
		{"max ack pending above broker cap", func(c *StepConsumerConfig) { c.MaxAckPending = 65_537 }, "unsafe step consumer limits"},
		{"nak backoff above a day", func(c *StepConsumerConfig) { c.NakBackoffMin, c.NakBackoffMax = time.Second, 25 * time.Hour }, "unsafe step consumer limits"},
	}
	handler := func(context.Context, coresaga.Command) (coresaga.Completion, error) { return coresaga.Completion{}, nil }
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &startJetStream{}
			transport, err := NewJetStreamPublisher(client, "roost.saga")
			if err != nil {
				t.Fatal(err)
			}
			inbox, err := NewMongoCommandInbox(mongotest.NewClient(), "game", "")
			if err != nil {
				t.Fatal(err)
			}
			cfg := valid
			tc.mutate(&cfg)
			_, err = SubscribeMongoStep(context.Background(), client, transport, inbox, cfg, handler)
			expectErr(t, err, tc.want)
			if client.handler != nil {
				t.Fatal("a refused config must not subscribe")
			}
		})
	}
	client := &startJetStream{}
	transport, _ := NewJetStreamPublisher(client, "roost.saga")
	inbox, _ := NewMongoCommandInbox(mongotest.NewClient(), "game", "")
	for name, call := range map[string]func() error{
		"nil client": func() error {
			_, err := SubscribeMongoStep(context.Background(), nil, transport, inbox, valid, handler)
			return err
		},
		"nil transport": func() error {
			_, err := SubscribeMongoStep(context.Background(), client, nil, inbox, valid, handler)
			return err
		},
		"nil inbox": func() error {
			_, err := SubscribeMongoStep(context.Background(), client, transport, nil, valid, handler)
			return err
		},
		"nil handler": func() error {
			_, err := SubscribeMongoStep(context.Background(), client, transport, inbox, valid, nil)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) { expectErr(t, call(), "invalid step consumer configuration") })
	}
	if _, err := SubscribeMongoStep(context.Background(), client, transport, inbox, valid, handler); err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
}

func TestSubscribeDataEngineStepRefusesEachUnsafeConfig(t *testing.T) {
	valid := StepConsumerConfig{Stream: "ROOST_SAGA", Durable: "game-reserve", Topic: "reserve", AckWait: 30 * time.Second}
	newInbox := func(t *testing.T) *DataEngineStepInbox {
		inbox, err := NewDataEngineStepInbox(mongotest.NewClient(), "game", DataEngineStepInboxOptions{Owner: "game-1", LeaseDuration: 2 * time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		return inbox
	}
	handler := func(context.Context, coresaga.Command) (coresaga.Completion, error) { return coresaga.Completion{}, nil }
	cases := []struct {
		name   string
		mutate func(*StepConsumerConfig)
		want   string
	}{
		{"blank stream", func(c *StepConsumerConfig) { c.Stream = "" }, "invalid dataengine step consumer configuration"},
		{"topic with wildcard", func(c *StepConsumerConfig) { c.Topic = "reserve.*" }, "invalid dataengine step consumer configuration"},
		{"max deliver above broker cap", func(c *StepConsumerConfig) { c.MaxDeliver = 1_000_001 }, "unsafe dataengine step consumer limits"},
		{"lease not longer than ack wait", func(c *StepConsumerConfig) { c.AckWait = 2 * time.Minute }, "unsafe dataengine step consumer limits"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &startJetStream{}
			transport, _ := NewJetStreamPublisher(client, "roost.saga")
			cfg := valid
			tc.mutate(&cfg)
			_, err := SubscribeDataEngineStep(context.Background(), client, transport, newInbox(t), cfg, handler)
			expectErr(t, err, tc.want)
			if client.handler != nil {
				t.Fatal("a refused config must not subscribe")
			}
		})
	}
	client := &startJetStream{}
	transport, _ := NewJetStreamPublisher(client, "roost.saga")
	if _, err := SubscribeDataEngineStep(context.Background(), client, transport, newInbox(t), valid, handler); err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
	if _, err := NewDataEngineStepInbox(mongotest.NewClient(), "game", DataEngineStepInboxOptions{}); err == nil {
		t.Fatal("inbox without owner accepted")
	}
	if _, err := NewDataEngineStepInbox(nil, "game", DataEngineStepInboxOptions{Owner: "game-1"}); err == nil {
		t.Fatal("inbox without client accepted")
	}
}

// Replay must hand back a completion only for the exact command that earned
// it: a receipt under the same command ID with a different digest is an
// identity conflict, an unknown ID is simply "nothing to replay".
func TestMongoCommandInboxReplayRefusesForeignReceipt(t *testing.T) {
	if _, err := NewMongoCommandInbox(nil, "game", ""); err == nil {
		t.Fatal("inbox without client accepted")
	}
	if _, err := NewMongoCommandInbox(mongotest.NewClient(), " ", ""); err == nil {
		t.Fatal("inbox without database accepted")
	}
	inbox, err := NewMongoCommandInbox(mongotest.NewClient(), "game", "")
	if err != nil {
		t.Fatal(err)
	}
	handler := func(context.Context, coresaga.Command) (coresaga.Completion, error) {
		return coresaga.Completion{Success: true, Data: []byte("ok")}, nil
	}
	now := time.Now()
	command := coresaga.Command{ID: "one", IdempotencyKey: "op", SagaID: "saga", SagaType: "rally", DefinitionVersion: 1, BusinessKey: "r-3", Step: 0, StepName: "reserve", Phase: coresaga.PhaseForward, Attempt: 1, Topic: "reserve", Payload: []byte("a"), CreatedAt: now, DeadlineAt: now.Add(time.Second)}
	if _, _, err := inbox.Handle(context.Background(), command, handler); err != nil {
		t.Fatal(err)
	}
	completion, found, err := inbox.Replay(context.Background(), command)
	if err != nil || !found || !completion.Success || string(completion.Data) != "ok" {
		t.Fatalf("replay of the committed command = %+v, %v, %v", completion, found, err)
	}
	foreign := command
	foreign.Payload = []byte("different")
	if _, _, err := inbox.Replay(context.Background(), foreign); !errors.Is(err, coresaga.ErrIdentityConflict) {
		t.Fatalf("replay with a foreign digest err = %v, want ErrIdentityConflict", err)
	}
	unknown := command
	unknown.ID = "two"
	if _, found, err := inbox.Replay(context.Background(), unknown); err != nil || found {
		t.Fatalf("replay of an unknown command = %v, %v; want not found without error", found, err)
	}
	var nilInbox *MongoCommandInbox
	if _, _, err := nilInbox.Replay(context.Background(), command); !errors.Is(err, coresaga.ErrInvalidRecord) {
		t.Fatalf("nil inbox replay err = %v", err)
	}
	if _, _, err := nilInbox.Handle(context.Background(), command, handler); !errors.Is(err, coresaga.ErrInvalidRecord) {
		t.Fatalf("nil inbox handle err = %v", err)
	}
}
