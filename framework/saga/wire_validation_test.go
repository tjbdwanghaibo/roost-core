package saga

import (
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

func validCommand() Command {
	now := time.Now()
	return Command{
		ID: "cmd-1", IdempotencyKey: "key-1", SagaID: "saga-1", SagaType: "rally", DefinitionVersion: 1,
		BusinessKey: "biz", Step: 0, StepName: "reserve", Phase: PhaseForward, Attempt: 1, Topic: "rally.reserve",
		DeadlineAt: now.Add(time.Minute), CreatedAt: now,
	}
}

// Command / Completion 是协调器与步骤执行方之间的线上格式；每个校验子句用单字段变异断言，丢掉一条子句恰好一例变红。
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

// 启动 effect 是留在 WAL 与 JetStream 上的线上载荷；解码器拒绝的每种形状都断言（版本 1 的信封一律拒绝）。
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
	raw, err = json.Marshal(startEffectPayload{Version: 1, Start: StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeStartEffect(raw); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("version 1 start effect = %v, want refused", err)
	}
}
