package saga

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func expectErr(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to contain %q", err, want)
	}
}

// NewEngine refuses configurations whose lease arithmetic cannot hold. Each
// rule is pinned by the field it names, so an operator's error points at the
// knob; only the publisher-batch rule had a test, and it checked err != nil.
func TestNewEngineRefusesEachUnsafeOption(t *testing.T) {
	publisher := PublishFunc(func(context.Context, Command) error { return nil })
	cases := []struct {
		name   string
		mutate func(*Options)
		want   string
	}{
		{"store timeout not below lease", func(o *Options) { o.StoreTimeout = o.LeaseDuration }, "StoreTimeout (15s) must be < LeaseDuration (15s)"},
		{"owner too long", func(o *Options) { o.Owner = strings.Repeat("o", 257) }, "Owner length 257 exceeds 256"},
		{"too many coordinator workers", func(o *Options) { o.CoordinatorWorkers = 1025 }, "CoordinatorWorkers 1025 exceeds 1024"},
		{"too many publisher workers", func(o *Options) { o.PublisherWorkers = 1025 }, "PublisherWorkers 1025 exceeds 1024"},
		{"coordinator batch too large", func(o *Options) { o.CoordinatorBatch = 4097 }, "CoordinatorBatch 4097 exceeds 4096"},
		{"publisher batch too large", func(o *Options) { o.PublisherBatch = 4097 }, "PublisherBatch 4097 exceeds 4096"},
		{"payload cap too large", func(o *Options) { o.MaxPayloadBytes = 4<<20 + 1 }, "MaxPayloadBytes 4194305 exceeds 4194304"},
		{"lease not above publish timeout", func(o *Options) { o.PublishTimeout = o.LeaseDuration }, "LeaseDuration (15s) must be > PublishTimeout (15s)"},
		{"coordinator budget exhausted", func(o *Options) { o.CoordinatorBatch = 5 }, "coordinator budget exhausted"},
		{"publisher budget exhausted by publish timeout", func(o *Options) { o.PublisherBatch = 5 }, "publisher budget exhausted: (LeaseDuration-StoreTimeout)/PublisherBatch = "},
		{"publisher budget exhausted by store timeout", func(o *Options) {
			o.LeaseDuration, o.StoreTimeout, o.PublishTimeout, o.PublisherBatch, o.CoordinatorBatch = 10*time.Second, 3*time.Second, 3*time.Second, 2, 1
		}, "publisher budget exhausted: (LeaseDuration-StoreTimeout)/PublisherBatch - PublishTimeout = "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			options := DefaultOptions()
			tc.mutate(&options)
			_, err := NewEngine(newMemoryStore(), publisher, options)
			expectErr(t, err, tc.want)
		})
	}
	if _, err := NewEngine(newMemoryStore(), publisher, DefaultOptions()); err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}
	if _, err := NewEngine(nil, publisher, DefaultOptions()); err == nil || !strings.Contains(err.Error(), "store and publisher are required") {
		t.Fatalf("nil store = %v", err)
	}
}

func validCommand() Command {
	now := time.Now()
	return Command{
		ID: "cmd-1", IdempotencyKey: "key-1", SagaID: "saga-1", SagaType: "rally", DefinitionVersion: 1,
		BusinessKey: "biz", Step: 0, StepName: "reserve", Phase: PhaseForward, Attempt: 1, Topic: "rally.reserve",
		DeadlineAt: now.Add(time.Minute), CreatedAt: now,
	}
}

// Command and Completion are what crosses the bus between the coordinator and
// the step executors. One giant condition guards each; pin every clause with
// a single-field mutation so a dropped clause turns exactly one case red.
func TestCommandValidateRefusesEachBrokenField(t *testing.T) {
	if err := validCommand().Validate(); err != nil {
		t.Fatalf("baseline rejected: %v", err)
	}
	cases := map[string]func(*Command){
		"empty id":                func(c *Command) { c.ID = "" },
		"id too long":             func(c *Command) { c.ID = strings.Repeat("i", 193) },
		"empty idempotency key":   func(c *Command) { c.IdempotencyKey = "" },
		"saga id with wildcard":   func(c *Command) { c.SagaID = "saga.>" },
		"saga type padded":        func(c *Command) { c.SagaType = " rally" },
		"saga type empty":         func(c *Command) { c.SagaType = "" },
		"definition version zero": func(c *Command) { c.DefinitionVersion = 0 },
		"business key padded":     func(c *Command) { c.BusinessKey = "biz " },
		"business key too long":   func(c *Command) { c.BusinessKey = strings.Repeat("b", 513) },
		"negative step":           func(c *Command) { c.Step = -1 },
		"step name empty":         func(c *Command) { c.StepName = "" },
		"phase out of range":      func(c *Command) { c.Phase = PhaseCompensate + 1 },
		"attempt zero":            func(c *Command) { c.Attempt = 0 },
		"topic with empty token":  func(c *Command) { c.Topic = "rally..reserve" },
		"payload over 4MiB":       func(c *Command) { c.Payload = make([]byte, 4<<20+1) },
		"zero deadline":           func(c *Command) { c.DeadlineAt = time.Time{} },
		"zero created at":         func(c *Command) { c.CreatedAt = time.Time{} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := validCommand()
			mutate(&c)
			if err := c.Validate(); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("Validate = %v, want ErrInvalidRecord", err)
			}
		})
	}
}

func TestCompletionValidateRefusesEachBrokenField(t *testing.T) {
	ok := Completion{CommandID: "cmd-1", IdempotencyKey: "key-1", SagaID: "saga-1", Success: true}
	if err := ok.Validate(); err != nil {
		t.Fatalf("baseline rejected: %v", err)
	}
	failed := Completion{CommandID: "cmd-1", IdempotencyKey: "key-1", SagaID: "saga-1", Success: false, Error: "boom", Retryable: true}
	if err := failed.Validate(); err != nil {
		t.Fatalf("failed baseline rejected: %v", err)
	}
	cases := map[string]Completion{
		"empty command id":         {IdempotencyKey: "k", SagaID: "s", Success: true},
		"empty idempotency key":    {CommandID: "c", SagaID: "s", Success: true},
		"saga id with space":       {CommandID: "c", IdempotencyKey: "k", SagaID: "s a", Success: true},
		"error too long":           {CommandID: "c", IdempotencyKey: "k", SagaID: "s", Error: strings.Repeat("e", 4097)},
		"data over 4MiB":           {CommandID: "c", IdempotencyKey: "k", SagaID: "s", Success: true, Data: make([]byte, 4<<20+1)},
		"success with error text":  {CommandID: "c", IdempotencyKey: "k", SagaID: "s", Success: true, Error: "x"},
		"success marked retryable": {CommandID: "c", IdempotencyKey: "k", SagaID: "s", Success: true, Retryable: true},
		"failure without error":    {CommandID: "c", IdempotencyKey: "k", SagaID: "s", Success: false, Error: "  "},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if err := c.Validate(); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("Validate = %v, want ErrInvalidRecord", err)
			}
		})
	}
}

// The start / completion effects are wire payloads that survive in the WAL
// and on JetStream; every shape the decoders refuse is pinned.
func TestNestEffectCodecsRefuseEachMalformedPayload(t *testing.T) {
	if _, err := DecodeStartEffect(nil); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("empty start payload = %v", err)
	}
	raw, err := json.Marshal(startEffectPayload{Version: WireVersion + 1, Start: StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeStartEffect(raw); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("future start version = %v", err)
	}
	for name, req := range map[string]StartRequest{
		"empty type":         {DefinitionVersion: 1, BusinessKey: "b"},
		"type too long":      {Type: strings.Repeat("t", 129), DefinitionVersion: 1, BusinessKey: "b"},
		"zero definition":    {Type: "rally", BusinessKey: "b"},
		"empty business key": {Type: "rally", DefinitionVersion: 1},
		"data over 4MiB":     {Type: "rally", DefinitionVersion: 1, BusinessKey: "b", Data: make([]byte, 4<<20+1)},
		"unsafe explicit id": {Type: "rally", DefinitionVersion: 1, BusinessKey: "b", ID: "a.b"},
	} {
		t.Run("start/"+name, func(t *testing.T) {
			if _, err := NewStartEffect(req); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("NewStartEffect = %v, want ErrInvalidRecord", err)
			}
			raw, err := json.Marshal(startEffectPayload{Version: WireVersion, Start: req})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeStartEffect(raw); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("DecodeStartEffect = %v, want ErrInvalidRecord", err)
			}
		})
	}
	if _, err := DecodeCompletionEffect(nil); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("empty completion payload = %v", err)
	}
	if _, err := DecodeCompletionEffect([]byte("{not json")); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("undecodable completion payload = %v", err)
	}
	raw, err = json.Marshal(completionEffectPayload{Version: WireVersion + 1, Completion: Completion{CommandID: "c", IdempotencyKey: "k", SagaID: "s", Success: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCompletionEffect(raw); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("future completion version = %v", err)
	}
	raw, err = json.Marshal(completionEffectPayload{Version: WireVersion, Completion: Completion{CommandID: "c", IdempotencyKey: "k", SagaID: "s", Success: false}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCompletionEffect(raw); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("completion failing validation = %v", err)
	}
}
